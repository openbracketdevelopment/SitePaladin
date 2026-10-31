use crate::{
    audit::{self, Finding, Metadata},
    config::{Config, is_public_ip},
};
use futures_util::StreamExt;
use robotstxt::DefaultMatcher;
use serde::Serialize;
use sqlx::{Row, SqlitePool};
use std::{
    collections::{HashMap, HashSet, VecDeque},
    net::{IpAddr, SocketAddr},
    time::{Duration, Instant, SystemTime},
};
use tokio::time::sleep;
use tokio_util::sync::CancellationToken;
use url::Url;

const USER_AGENT: &str = "SitePaladin/0.1 (+https://github.com/openbracketdevelopment/SitePaladin)";
const ROBOTS_AGENT: &str = "SitePaladin";
const ROBOTS_BODY_LIMIT: usize = 64 * 1024;

#[derive(Debug)]
enum FetchError {
    Unsafe(String),
    RobotsDenied,
    Cancelled,
    Timeout,
    Network(String),
    TooLarge,
}

impl FetchError {
    fn health(&self) -> &'static str {
        match self {
            Self::Unsafe(_) => "skipped",
            Self::RobotsDenied => "skipped",
            Self::Cancelled => "cancelled",
            Self::Timeout => "timeout",
            Self::Network(message) if message.contains("redirect") => "redirect_loop",
            Self::Network(message) if message.contains("dns") => "unreachable",
            Self::Network(message)
                if message.contains("certificate") || message.contains("TLS") =>
            {
                "tls_error"
            }
            Self::Network(_) => "unreachable",
            Self::TooLarge => "inconclusive",
        }
    }

    fn message(&self) -> String {
        match self {
            Self::Unsafe(message) | Self::Network(message) => message.clone(),
            Self::RobotsDenied => {
                "Request was disallowed by robots.txt or robots policy could not be established."
                    .into()
            }
            Self::Cancelled => "Scan was cancelled.".into(),
            Self::Timeout => "Request timed out.".into(),
            Self::TooLarge => "Response exceeded the configured body-size limit.".into(),
        }
    }
}

struct FetchResult {
    final_url: Url,
    status: u16,
    body: Vec<u8>,
    redirected: bool,
    redirect_chain: Vec<RedirectHop>,
    content_type: String,
}

#[derive(Clone, Debug, Serialize)]
struct RedirectHop {
    url: String,
    status: u16,
}

struct FetchContext<'a> {
    config: &'a Config,
    cancel: &'a CancellationToken,
    robots: HashMap<String, Option<String>>,
    last_request: HashMap<String, Instant>,
    retry_after_until: HashMap<String, Instant>,
}

impl<'a> FetchContext<'a> {
    fn new(config: &'a Config, cancel: &'a CancellationToken) -> Self {
        Self {
            config,
            cancel,
            robots: HashMap::new(),
            last_request: HashMap::new(),
            retry_after_until: HashMap::new(),
        }
    }

    async fn fetch(
        &mut self,
        url: Url,
        method: reqwest::Method,
        body_limit: usize,
    ) -> Result<FetchResult, FetchError> {
        let mut current = url;
        let mut redirected = false;
        let mut redirect_chain = Vec::new();
        let mut visited = HashSet::new();
        for hop in 0..=self.config.max_redirects {
            if self.cancel.is_cancelled() {
                return Err(FetchError::Cancelled);
            }
            if !visited.insert(fetch_key(&current)) {
                return Err(FetchError::Network("redirect loop detected".into()));
            }
            if !self.robots_allowed(&current).await? {
                return Err(FetchError::RobotsDenied);
            }
            let response = self.fetch_one(&current, method.clone()).await?;
            if response.status().is_redirection() {
                let Some(location) = response.headers().get(reqwest::header::LOCATION) else {
                    return Err(FetchError::Network(
                        "redirect response has no Location header".into(),
                    ));
                };
                if hop >= self.config.max_redirects {
                    return Err(FetchError::Network("redirect limit exceeded".into()));
                }
                let location = location
                    .to_str()
                    .map_err(|_| FetchError::Unsafe("invalid redirect Location header".into()))?;
                let next = current
                    .join(location)
                    .map_err(|_| FetchError::Unsafe("invalid redirect destination".into()))?;
                validate_url(&next, self.config)?;
                redirect_chain.push(RedirectHop {
                    url: current.to_string(),
                    status: response.status().as_u16(),
                });
                current = next;
                redirected = true;
                continue;
            }
            let status = response.status().as_u16();
            let content_type = response
                .headers()
                .get(reqwest::header::CONTENT_TYPE)
                .and_then(|v| v.to_str().ok())
                .unwrap_or_default()
                .to_owned();
            let mut stream = response.bytes_stream();
            let mut body = Vec::new();
            while let Some(chunk) = tokio::select! {
                _ = self.cancel.cancelled() => return Err(FetchError::Cancelled),
                chunk = stream.next() => chunk,
            } {
                let chunk = chunk.map_err(network_error)?;
                if body.len().saturating_add(chunk.len()) > body_limit {
                    return Err(FetchError::TooLarge);
                }
                body.extend_from_slice(&chunk);
            }
            return Ok(FetchResult {
                final_url: current,
                status,
                body,
                redirected,
                redirect_chain,
                content_type,
            });
        }
        Err(FetchError::Network("redirect limit exceeded".into()))
    }

    async fn robots_allowed(&mut self, url: &Url) -> Result<bool, FetchError> {
        let origin = origin_key(url);
        if !self.robots.contains_key(&origin) {
            if self.robots.len() >= self.config.max_robots_origins {
                tracing::warn!(
                    origin,
                    "robots origin cache limit reached; denying requests to this origin"
                );
                return Ok(false);
            }
            let robots_url = url
                .join("/robots.txt")
                .map_err(|_| FetchError::Unsafe("could not construct robots.txt URL".into()))?;
            let policy = match self
                .fetch_unchecked(robots_url, reqwest::Method::GET, ROBOTS_BODY_LIMIT)
                .await
            {
                Ok(response) if response.status == 404 => Some(String::new()),
                Ok(response) if (200..300).contains(&response.status) => {
                    Some(String::from_utf8_lossy(&response.body).into_owned())
                }
                Ok(response) => {
                    tracing::warn!(
                        origin,
                        status = response.status,
                        "robots.txt request failed; denying requests to this origin"
                    );
                    None
                }
                Err(error) => {
                    tracing::warn!(origin, error = %error.message(), "robots.txt request failed; denying requests to this origin");
                    None
                }
            };
            self.robots.insert(origin.clone(), policy);
        }
        let Some(policy) = self.robots.get(&origin).and_then(Option::as_ref) else {
            return Ok(false);
        };
        if policy.is_empty() {
            return Ok(true);
        }
        let mut matcher = DefaultMatcher::default();
        Ok(matcher.one_agent_allowed_by_robots(policy, ROBOTS_AGENT, url.as_str()))
    }

    async fn fetch_unchecked(
        &mut self,
        url: Url,
        method: reqwest::Method,
        body_limit: usize,
    ) -> Result<FetchResult, FetchError> {
        let mut current = url;
        let mut redirected = false;
        let mut redirect_chain = Vec::new();
        let mut visited = HashSet::new();
        for hop in 0..=self.config.max_redirects {
            if !visited.insert(fetch_key(&current)) {
                return Err(FetchError::Network("redirect loop detected".into()));
            }
            let response = self.fetch_one(&current, method.clone()).await?;
            if response.status().is_redirection() {
                if hop >= self.config.max_redirects {
                    return Err(FetchError::Network("redirect limit exceeded".into()));
                }
                let location = response
                    .headers()
                    .get(reqwest::header::LOCATION)
                    .and_then(|v| v.to_str().ok())
                    .ok_or_else(|| FetchError::Unsafe("invalid redirect Location header".into()))?;
                current = current
                    .join(location)
                    .map_err(|_| FetchError::Unsafe("invalid redirect destination".into()))?;
                validate_url(&current, self.config)?;
                redirect_chain.push(RedirectHop {
                    url: response.url().to_string(),
                    status: response.status().as_u16(),
                });
                redirected = true;
                continue;
            }
            let status = response.status().as_u16();
            let content_type = response
                .headers()
                .get(reqwest::header::CONTENT_TYPE)
                .and_then(|v| v.to_str().ok())
                .unwrap_or_default()
                .to_owned();
            let mut stream = response.bytes_stream();
            let mut body = Vec::new();
            while let Some(chunk) = tokio::select! {
                _ = self.cancel.cancelled() => return Err(FetchError::Cancelled),
                chunk = stream.next() => chunk,
            } {
                let chunk = chunk.map_err(network_error)?;
                if body.len().saturating_add(chunk.len()) > body_limit {
                    return Err(FetchError::TooLarge);
                }
                body.extend_from_slice(&chunk);
            }
            return Ok(FetchResult {
                final_url: current,
                status,
                body,
                redirected,
                redirect_chain,
                content_type,
            });
        }
        Err(FetchError::Network("redirect limit exceeded".into()))
    }

    async fn fetch_one(
        &mut self,
        url: &Url,
        method: reqwest::Method,
    ) -> Result<reqwest::Response, FetchError> {
        validate_url(url, self.config)?;
        let host = url
            .host_str()
            .ok_or_else(|| FetchError::Unsafe("URL has no host".into()))?;
        self.wait_origin(url).await;
        if self.cancel.is_cancelled() {
            return Err(FetchError::Cancelled);
        }
        let addresses =
            resolve_public_addresses(host, url.port_or_known_default().unwrap_or(80), self.config)
                .await?;
        let client = reqwest::Client::builder()
            .no_proxy()
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(8))
            .timeout(self.config.request_timeout)
            .user_agent(USER_AGENT)
            .resolve_to_addrs(host, &addresses)
            .build()
            .map_err(|e| FetchError::Network(format!("could not configure HTTP client: {e}")))?;
        let response = tokio::select! {
            _ = self.cancel.cancelled() => Err(FetchError::Cancelled),
            result = client.request(method, url.clone()).send() => result.map_err(network_error),
        }?;
        if response.status().as_u16() == 429 {
            if let Some(delay) = response
                .headers()
                .get(reqwest::header::RETRY_AFTER)
                .and_then(|value| value.to_str().ok())
                .and_then(parse_retry_after)
            {
                self.retry_after_until
                    .insert(origin_key(url), Instant::now() + delay);
            }
        }
        Ok(response)
    }

    async fn wait_origin(&mut self, url: &Url) {
        if self.config.min_origin_delay.is_zero() {
            return;
        }
        let origin = origin_key(url);
        if let Some(last) = self.last_request.get(&origin) {
            let remaining = self.config.min_origin_delay.saturating_sub(last.elapsed());
            let retry_after = self
                .retry_after_until
                .get(&origin)
                .map(|until| until.saturating_duration_since(Instant::now()))
                .unwrap_or_default();
            let delay = remaining.max(retry_after);
            if !delay.is_zero() {
                tokio::select! {
                    _ = self.cancel.cancelled() => return,
                    _ = sleep(delay) => {}
                }
            }
        }
        self.last_request.insert(origin, Instant::now());
    }
}

fn parse_retry_after(value: &str) -> Option<Duration> {
    let seconds = value.parse::<u64>().ok().map(Duration::from_secs);
    let date = httpdate::parse_http_date(value)
        .ok()
        .and_then(|at| at.duration_since(SystemTime::now()).ok());
    seconds
        .or(date)
        .map(|duration| duration.min(Duration::from_secs(60)))
}

pub async fn run_scan(pool: SqlitePool, config: Config, scan_id: i64, cancel: CancellationToken) {
    let result = sqlx::query("SELECT seed_url, max_pages, config_snapshot FROM scans WHERE id = ?")
        .bind(scan_id)
        .fetch_one(&pool)
        .await;
    let Ok(scan) = result else {
        tracing::error!(scan_id, "queued scan row is missing");
        return;
    };
    let seed: String = scan.get("seed_url");
    let requested_max_pages: i64 = scan.get("max_pages");
    let snapshot: String = scan.get("config_snapshot");
    let limits: crate::config::ScanLimits = match serde_json::from_str(&snapshot) {
        Ok(limits) => limits,
        Err(error) => {
            let message = format!("invalid scan configuration snapshot: {error}");
            tracing::error!(scan_id, %message);
            let _ = sqlx::query("UPDATE scans SET state='failed',error=?,finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?")
                .bind(message).bind(scan_id).execute(&pool).await;
            return;
        }
    };
    let config = config.restrict_with_snapshot(&limits);
    let max_pages = (requested_max_pages as usize).min(config.max_pages);
    let claim = match sqlx::query("UPDATE scans SET state='running', started_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND state='queued'")
        .bind(scan_id).execute(&pool).await {
        Ok(result) => result,
        Err(error) => {
            tracing::error!(scan_id, %error, "could not claim scan");
            return;
        }
    };
    if claim.rows_affected() == 0 {
        return;
    }
    let scan_future = crawl(
        pool.clone(),
        &config,
        scan_id,
        &seed,
        max_pages as usize,
        &cancel,
    );
    let outcome = tokio::time::timeout(config.max_scan_duration, scan_future).await;
    let (state, error) = match outcome {
        Ok(Ok(())) if cancel.is_cancelled() => ("cancelled", None),
        Ok(Ok(())) => ("completed", None),
        Ok(Err(FetchError::Cancelled)) => ("cancelled", None),
        Ok(Err(error)) => ("failed", Some(error.message())),
        Err(_) => {
            cancel.cancel();
            if let Err(error) = sqlx::query("UPDATE scans SET truncated=1 WHERE id=?")
                .bind(scan_id)
                .execute(&pool)
                .await
            {
                tracing::error!(scan_id, %error, "could not mark duration-limited scan");
            }
            (
                "cancelled",
                Some("Maximum scan duration was reached.".into()),
            )
        }
    };
    if let Some(error) = &error {
        tracing::warn!(scan_id, %error, "scan did not complete normally");
    }
    if let Err(error) = sqlx::query("UPDATE scans SET state=?, error=?, finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?")
        .bind(state).bind(error).bind(scan_id).execute(&pool).await {
        tracing::error!(scan_id, %error, "could not persist final scan state");
    }
}

async fn crawl(
    pool: SqlitePool,
    config: &Config,
    scan_id: i64,
    seed: &str,
    max_pages: usize,
    cancel: &CancellationToken,
) -> Result<(), FetchError> {
    let mut seed_url =
        Url::parse(seed).map_err(|e| FetchError::Unsafe(format!("invalid seed URL: {e}")))?;
    validate_url(&seed_url, config)?;
    strip_fragment(&mut seed_url);
    let mut context = FetchContext::new(config, cancel);
    let mut queue = VecDeque::from([seed_url.clone()]);
    let mut seen_pages = HashSet::new();
    let mut seen_destinations = HashSet::new();
    let mut path_query_variants: HashMap<String, HashSet<Option<String>>> = HashMap::new();
    let mut pages = Vec::new();
    let mut link_occurrences = 0;
    let mut root_origin = origin_key(&seed_url);
    let started = Instant::now();

    while let Some(url) = queue.pop_front() {
        if cancel.is_cancelled() {
            return Err(FetchError::Cancelled);
        }
        if started.elapsed() > config.max_scan_duration {
            return Err(FetchError::Timeout);
        }
        let key = fetch_key(&url);
        if !seen_pages.insert(key.clone()) || seen_pages.len() > max_pages {
            continue;
        }
        path_query_variants
            .entry(path_key(&url))
            .or_default()
            .insert(url.query().map(str::to_owned));
        sqlx::query("UPDATE scans SET pages_discovered=? WHERE id=?")
            .bind(seen_pages.len() as i64)
            .bind(scan_id)
            .execute(&pool)
            .await
            .map_err(|e| FetchError::Network(format!("database update failed: {e}")))?;

        let fetched = context
            .fetch(url.clone(), reqwest::Method::GET, config.max_response_bytes)
            .await;
        let (final_url, status, fetch_state, redirect_chain, metadata, findings) = match fetched {
            Ok(response) => {
                let redirect_chain =
                    serde_json::to_string(&response.redirect_chain).map_err(|e| {
                        FetchError::Network(format!("could not serialize redirect chain: {e}"))
                    })?;
                if pages.is_empty() {
                    root_origin = origin_key(&response.final_url);
                }
                let fetch_state = if !(200..300).contains(&response.status) {
                    health_for_status(response.status, response.redirected).to_owned()
                } else if !is_html(&response.content_type) {
                    "non_html".to_owned()
                } else {
                    "checked".to_owned()
                };
                if is_html(&response.content_type) && (200..300).contains(&response.status) {
                    let html = String::from_utf8_lossy(&response.body);
                    let (metadata, mut findings) = audit::parse_html(&html, &response.final_url)
                        .map_err(FetchError::Network)?;
                    if response.redirected {
                        findings.push(make_finding(
                            "page.redirected",
                            "page",
                            "info",
                            "warning",
                            response.final_url.as_str(),
                            "The page redirected before returning HTML.",
                            "Review the redirect if the original URL is linked elsewhere.",
                        ));
                    }
                    (
                        response.final_url,
                        Some(response.status),
                        fetch_state,
                        redirect_chain,
                        Some(metadata),
                        findings,
                    )
                } else {
                    let findings = if (200..300).contains(&response.status)
                        && !is_html(&response.content_type)
                    {
                        vec![make_finding(
                            "page.not_html",
                            "page",
                            "info",
                            "not_checked",
                            &response.content_type,
                            "The response is not a supported HTML page.",
                            "Link checking can still record this destination.",
                        )]
                    } else {
                        vec![make_finding(
                            "page.fetch",
                            "links",
                            "warning",
                            "issue",
                            &response.status.to_string(),
                            "The page did not return a successful HTTP response.",
                            "Review the response status and destination.",
                        )]
                    };
                    (
                        response.final_url,
                        Some(response.status),
                        fetch_state,
                        redirect_chain,
                        None,
                        findings,
                    )
                }
            }
            Err(error) => {
                if matches!(error, FetchError::Cancelled) {
                    return Err(error);
                }
                let finding = make_finding(
                    "page.fetch",
                    "links",
                    if error.health() == "skipped" {
                        "info"
                    } else {
                        "warning"
                    },
                    if error.health() == "skipped" {
                        "not_checked"
                    } else {
                        "issue"
                    },
                    &error.message(),
                    "The page could not be fetched within the configured policy.",
                    "Review the URL, robots policy, and network result.",
                );
                (
                    url.clone(),
                    None,
                    error.health().to_owned(),
                    "[]".into(),
                    None,
                    vec![finding],
                )
            }
        };

        let page_id = save_page(
            &pool,
            scan_id,
            &url,
            &final_url,
            status,
            &fetch_state,
            &redirect_chain,
            metadata.as_ref(),
            &findings,
        )
        .await
        .map_err(|e| FetchError::Network(format!("could not save page result: {e}")))?;
        pages.push((page_id, metadata.clone()));
        sqlx::query("UPDATE scans SET pages_checked=? WHERE id=?")
            .bind(pages.len() as i64)
            .bind(scan_id)
            .execute(&pool)
            .await
            .map_err(|e| FetchError::Network(format!("database update failed: {e}")))?;

        let page_health = match fetch_state.as_str() {
            "checked" | "non_html" => status
                .map(|value| health_for_status(value, final_url != url))
                .unwrap_or("inconclusive"),
            other => other,
        };
        let updated_destinations = sqlx::query("UPDATE destinations SET health=?, status_code=?, redirect_chain=? WHERE scan_id=? AND url=? AND health='pending'")
            .bind(page_health).bind(status.map(i64::from)).bind(&redirect_chain).bind(scan_id).bind(fetch_key(&url))
            .execute(&pool).await.map_err(|e| FetchError::Network(format!("could not update internal link status: {e}")))?
            .rows_affected();
        if updated_destinations > 0 && page_health != "skipped" {
            sqlx::query("UPDATE scans SET links_checked=links_checked+1 WHERE id=?")
                .bind(scan_id)
                .execute(&pool)
                .await
                .map_err(|e| FetchError::Network(format!("could not update link progress: {e}")))?;
            if matches!(
                page_health,
                "broken"
                    | "timeout"
                    | "unreachable"
                    | "tls_error"
                    | "redirect_loop"
                    | "server_error"
            ) {
                let id: i64 =
                    sqlx::query_scalar("SELECT id FROM destinations WHERE scan_id=? AND url=?")
                        .bind(scan_id)
                        .bind(fetch_key(&url))
                        .fetch_one(&pool)
                        .await
                        .map_err(|e| {
                            FetchError::Network(format!("could not find updated destination: {e}"))
                        })?;
                save_link_findings(&pool, id, url.as_str(), page_id)
                    .await
                    .map_err(|e| {
                        FetchError::Network(format!("could not save link findings: {e}"))
                    })?;
            }
        }
        let Some(metadata) = metadata else { continue };
        let base = effective_base(&metadata, &final_url);
        let occurrences = resolve_occurrences(&metadata.links, &base, &final_url, &root_origin);
        for occurrence in occurrences {
            if link_occurrences >= config.max_link_occurrences {
                sqlx::query("UPDATE scans SET truncated=1 WHERE id=?")
                    .bind(scan_id)
                    .execute(&pool)
                    .await
                    .map_err(|e| {
                        FetchError::Network(format!("could not mark link occurrence limit: {e}"))
                    })?;
                break;
            }
            link_occurrences += 1;
            let destination_url = occurrence
                .resolved
                .as_ref()
                .and_then(|raw| Url::parse(raw).ok());
            let classification = occurrence.classification.as_str();
            let mut destination_id = None;
            if let Some(destination) = destination_url {
                let destination_key = fetch_key(&destination);
                if !destination_key.is_empty() && matches!(classification, "internal" | "external")
                {
                    let is_new = seen_destinations.insert(destination_key.clone());
                    if is_new
                        && seen_destinations.len() <= config.max_unique_links
                        && matches!(classification, "internal" | "external")
                    {
                        let (health, status_code, chain) = if classification == "internal" {
                            let previously_fetched = sqlx::query(
                                "SELECT fetch_state,status_code,final_url,redirect_chain FROM pages WHERE scan_id=? AND (url=? OR final_url=?) LIMIT 1",
                            )
                            .bind(scan_id).bind(&destination_key).bind(&destination_key)
                            .fetch_optional(&pool).await
                            .map_err(|e| FetchError::Network(format!("could not check previously fetched page: {e}")))?;
                            if let Some(row) = previously_fetched {
                                let fetch_state: String = row.get("fetch_state");
                                let code: Option<i64> = row.get("status_code");
                                let redirected =
                                    row.get::<Option<String>, _>("final_url").as_deref()
                                        != Some(destination_key.as_str());
                                let chain: String = row.get("redirect_chain");
                                (
                                    code.map(|code| {
                                        health_for_status(code as u16, redirected).to_owned()
                                    })
                                    .unwrap_or(fetch_state),
                                    code,
                                    chain,
                                )
                            } else if origin_key(&destination) == root_origin {
                                let path_variants = path_query_variants
                                    .entry(path_key(&destination))
                                    .or_default();
                                let query = destination.query().map(str::to_owned);
                                if !path_variants.contains(&query)
                                    && path_variants.len() >= config.max_query_variants_per_path
                                {
                                    sqlx::query("UPDATE scans SET truncated=1 WHERE id=?")
                                        .bind(scan_id)
                                        .execute(&pool)
                                        .await
                                        .map_err(|e| {
                                            FetchError::Network(format!(
                                                "could not mark query variant limit: {e}"
                                            ))
                                        })?;
                                    ("skipped".to_owned(), None, "[]".to_owned())
                                } else if seen_pages.len() + queue.len() < max_pages {
                                    path_variants.insert(query);
                                    queue.push_back(destination.clone());
                                    ("pending".to_owned(), None, "[]".to_owned())
                                } else {
                                    sqlx::query("UPDATE scans SET truncated=1 WHERE id=?")
                                        .bind(scan_id)
                                        .execute(&pool)
                                        .await
                                        .map_err(|e| {
                                            FetchError::Network(format!(
                                                "could not mark page limit: {e}"
                                            ))
                                        })?;
                                    ("skipped".to_owned(), None, "[]".to_owned())
                                }
                            } else {
                                if origin_key(&destination) == root_origin {
                                    sqlx::query("UPDATE scans SET truncated=1 WHERE id=?")
                                        .bind(scan_id)
                                        .execute(&pool)
                                        .await
                                        .map_err(|e| {
                                            FetchError::Network(format!(
                                                "could not mark page limit: {e}"
                                            ))
                                        })?;
                                }
                                ("skipped".to_owned(), None, "[]".to_owned())
                            }
                        } else {
                            check_external(&mut context, destination.clone()).await?
                        };
                        let insert = sqlx::query("INSERT INTO destinations(scan_id,url,classification,health,status_code,redirect_chain) VALUES(?,?,?,?,?,?)")
                            .bind(scan_id).bind(&destination_key).bind(classification).bind(&health).bind(status_code).bind(&chain)
                            .execute(&pool).await
                            .map_err(|e| FetchError::Network(format!("could not save link destination: {e}")))?;
                        destination_id = Some(insert.last_insert_rowid());
                        if health != "pending" && health != "skipped" {
                            sqlx::query(
                                "UPDATE scans SET links_checked=links_checked+1 WHERE id=?",
                            )
                            .bind(scan_id)
                            .execute(&pool)
                            .await
                            .map_err(|e| {
                                FetchError::Network(format!("could not update link progress: {e}"))
                            })?;
                        }
                    } else if is_new && seen_destinations.len() > config.max_unique_links {
                        sqlx::query("UPDATE scans SET truncated=1 WHERE id=?")
                            .bind(scan_id)
                            .execute(&pool)
                            .await
                            .map_err(|e| {
                                FetchError::Network(format!("could not mark link limit: {e}"))
                            })?;
                    } else if !is_new {
                        destination_id = sqlx::query_scalar(
                            "SELECT id FROM destinations WHERE scan_id=? AND url=?",
                        )
                        .bind(scan_id)
                        .bind(&destination_key)
                        .fetch_optional(&pool)
                        .await
                        .map_err(|e| {
                            FetchError::Network(format!(
                                "could not read repeated link destination: {e}"
                            ))
                        })?;
                    }
                }
            }
            save_occurrence(
                &pool,
                page_id,
                destination_id,
                &occurrence.raw,
                &occurrence.classification,
                occurrence.fragment.as_deref(),
            )
            .await
            .map_err(|e| FetchError::Network(format!("could not save link occurrence: {e}")))?;
            if let (Some(destination_id), Some(url)) =
                (destination_id, occurrence.resolved.as_deref())
            {
                let health: Option<String> =
                    sqlx::query_scalar("SELECT health FROM destinations WHERE id=?")
                        .bind(destination_id)
                        .fetch_optional(&pool)
                        .await
                        .map_err(|e| {
                            FetchError::Network(format!("could not read link health: {e}"))
                        })?;
                if health.as_deref().is_some_and(|value| {
                    matches!(
                        value,
                        "broken"
                            | "timeout"
                            | "unreachable"
                            | "tls_error"
                            | "redirect_loop"
                            | "server_error"
                    )
                }) {
                    save_link_findings(&pool, destination_id, url, page_id)
                        .await
                        .map_err(|e| {
                            FetchError::Network(format!("could not save link findings: {e}"))
                        })?;
                }
            }
            if occurrence.classification == "invalid" {
                let finding = make_finding(
                    "link.invalid",
                    "links",
                    "warning",
                    "issue",
                    &occurrence.raw,
                    "This href could not be resolved as a URL.",
                    "Correct the href or remove it if the destination is no longer valid.",
                );
                save_finding(&pool, page_id, &finding).await.map_err(|e| {
                    FetchError::Network(format!("could not save invalid-link finding: {e}"))
                })?;
            } else if occurrence.classification == "unsupported" {
                let finding = make_finding(
                    "link.unsupported",
                    "links",
                    "info",
                    "not_checked",
                    &occurrence.raw,
                    "This href uses a scheme that SitePaladin does not fetch.",
                    "Review the link type; only HTTP(S) destinations are checked.",
                );
                save_finding(&pool, page_id, &finding).await.map_err(|e| {
                    FetchError::Network(format!("could not save unsupported-link finding: {e}"))
                })?;
            }
        }
    }

    if !queue.is_empty() {
        sqlx::query("UPDATE scans SET truncated=1 WHERE id=?")
            .bind(scan_id)
            .execute(&pool)
            .await
            .map_err(|e| {
                FetchError::Network(format!("could not persist crawl limit notice: {e}"))
            })?;
    }
    add_duplicate_findings(&pool, &pages)
        .await
        .map_err(|e| FetchError::Network(format!("could not save duplicate findings: {e}")))?;
    Ok(())
}

#[derive(Debug)]
struct Occurrence {
    raw: String,
    resolved: Option<String>,
    classification: String,
    fragment: Option<String>,
}

fn resolve_occurrences(
    hrefs: &[String],
    base: &Url,
    page_url: &Url,
    internal_origin: &str,
) -> Vec<Occurrence> {
    hrefs
        .iter()
        .map(|href| {
            let trimmed = href.trim();
            if trimmed.is_empty() {
                return Occurrence {
                    raw: href.clone(),
                    resolved: None,
                    classification: "empty".into(),
                    fragment: None,
                };
            }
            match trimmed.to_ascii_lowercase().as_str() {
                value if value.starts_with("mailto:") => {
                    return Occurrence {
                        raw: href.clone(),
                        resolved: None,
                        classification: "email".into(),
                        fragment: None,
                    };
                }
                value if value.starts_with("tel:") => {
                    return Occurrence {
                        raw: href.clone(),
                        resolved: None,
                        classification: "telephone".into(),
                        fragment: None,
                    };
                }
                _ => {}
            }
            match base.join(trimmed) {
                Ok(mut url) if matches!(url.scheme(), "http" | "https") => {
                    let fragment = url.fragment().map(str::to_owned);
                    url.set_fragment(None);
                    let mut current = page_url.clone();
                    current.set_fragment(None);
                    let classification = if current == url {
                        "fragment"
                    } else if internal_origin == origin_key(&url) {
                        "internal"
                    } else {
                        "external"
                    };
                    Occurrence {
                        raw: href.clone(),
                        resolved: Some(url.to_string()),
                        classification: classification.into(),
                        fragment,
                    }
                }
                Ok(url) => Occurrence {
                    raw: href.clone(),
                    resolved: Some(url.to_string()),
                    classification: "unsupported".into(),
                    fragment: url.fragment().map(str::to_owned),
                },
                Err(_) => Occurrence {
                    raw: href.clone(),
                    resolved: None,
                    classification: "invalid".into(),
                    fragment: None,
                },
            }
        })
        .collect()
}

fn effective_base(metadata: &Metadata, final_url: &Url) -> Url {
    metadata
        .base_url
        .as_deref()
        .and_then(|base| final_url.join(base).ok())
        .filter(|url| matches!(url.scheme(), "http" | "https"))
        .unwrap_or_else(|| final_url.clone())
}

async fn check_external(
    context: &mut FetchContext<'_>,
    url: Url,
) -> Result<(String, Option<i64>, String), FetchError> {
    match context
        .fetch(url.clone(), reqwest::Method::HEAD, 1024)
        .await
    {
        Ok(response) if matches!(response.status, 405 | 501) => {
            match context.fetch(url, reqwest::Method::GET, 64 * 1024).await {
                Ok(response) => Ok((
                    health_for_status(response.status, response.redirected).into(),
                    Some(response.status as i64),
                    serde_json::to_string(&response.redirect_chain).map_err(|e| {
                        FetchError::Network(format!("could not serialize redirect chain: {e}"))
                    })?,
                )),
                Err(FetchError::Cancelled) => Err(FetchError::Cancelled),
                Err(error) => Ok((error.health().into(), None, "[]".into())),
            }
        }
        Ok(response) => Ok((
            health_for_status(response.status, response.redirected).into(),
            Some(response.status as i64),
            serde_json::to_string(&response.redirect_chain).map_err(|e| {
                FetchError::Network(format!("could not serialize redirect chain: {e}"))
            })?,
        )),
        Err(FetchError::Cancelled) => Err(FetchError::Cancelled),
        Err(error) => Ok((error.health().into(), None, "[]".into())),
    }
}

fn health_for_status(status: u16, redirected: bool) -> &'static str {
    match status {
        200..=299 if redirected => "redirected",
        200..=299 => "healthy",
        404 | 410 => "broken",
        401 | 403 => "restricted",
        429 => "rate_limited",
        500..=599 => "server_error",
        300..=399 => "redirect_loop",
        _ => "inconclusive",
    }
}

#[allow(clippy::too_many_arguments)]
async fn save_page(
    pool: &SqlitePool,
    scan_id: i64,
    url: &Url,
    final_url: &Url,
    status: Option<u16>,
    fetch_state: &str,
    redirect_chain: &str,
    metadata: Option<&Metadata>,
    findings: &[Finding],
) -> Result<i64, sqlx::Error> {
    let result = sqlx::query(
        "INSERT INTO pages(scan_id,url,final_url,status_code,fetch_state,redirect_chain,title,description,canonical,og_title,og_description,og_url,og_image,robots) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
    )
    .bind(scan_id).bind(url.as_str()).bind(final_url.as_str()).bind(status.map(i64::from)).bind(fetch_state).bind(redirect_chain)
    .bind(metadata.map(|m| m.title.as_str())).bind(metadata.map(|m| m.description.as_str()))
    .bind(metadata.map(|m| m.canonical.as_str())).bind(metadata.map(|m| m.og_title.as_str()))
    .bind(metadata.map(|m| m.og_description.as_str())).bind(metadata.map(|m| m.og_url.as_str()))
    .bind(metadata.map(|m| m.og_image.as_str())).bind(metadata.map(|m| m.robots.as_str()))
    .execute(pool).await?;
    let page_id = result.last_insert_rowid();
    for finding in findings {
        save_finding(pool, page_id, finding).await?;
    }
    Ok(page_id)
}

async fn save_finding(
    pool: &SqlitePool,
    page_id: i64,
    finding: &Finding,
) -> Result<(), sqlx::Error> {
    sqlx::query("INSERT INTO findings(page_id,rule_id,category,severity,status,evidence,explanation,suggestion) VALUES(?,?,?,?,?,?,?,?)")
        .bind(page_id).bind(&finding.rule_id).bind(&finding.category).bind(&finding.severity)
        .bind(&finding.status).bind(&finding.evidence).bind(&finding.explanation).bind(&finding.suggestion)
        .execute(pool).await?;
    Ok(())
}

async fn save_occurrence(
    pool: &SqlitePool,
    page_id: i64,
    destination_id: Option<i64>,
    raw: &str,
    classification: &str,
    fragment: Option<&str>,
) -> Result<(), sqlx::Error> {
    sqlx::query("INSERT INTO link_occurrences(page_id,destination_id,raw_href,classification,fragment) VALUES(?,?,?,?,?)")
        .bind(page_id).bind(destination_id).bind(raw).bind(classification).bind(fragment).execute(pool).await?;
    Ok(())
}

async fn save_link_findings(
    pool: &SqlitePool,
    destination_id: i64,
    url: &str,
    current_page_id: i64,
) -> Result<(), sqlx::Error> {
    let rows = sqlx::query("SELECT DISTINCT page_id FROM link_occurrences WHERE destination_id=?")
        .bind(destination_id)
        .fetch_all(pool)
        .await?;
    let mut page_ids = rows
        .iter()
        .map(|row| row.get::<i64, _>("page_id"))
        .collect::<HashSet<_>>();
    page_ids.insert(current_page_id);
    for page_id in page_ids {
        let exists: bool = sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM findings WHERE page_id=? AND rule_id='link.problem' AND evidence=?)")
            .bind(page_id).bind(url).fetch_one(pool).await?;
        if !exists {
            save_finding(
                pool,
                page_id,
                &make_finding(
                    "link.problem",
                    "links",
                    "warning",
                    "issue",
                    url,
                    "This HTTP link destination did not return a healthy response.",
                    "Review the destination and update or remove the link if appropriate.",
                ),
            )
            .await?;
        }
    }
    Ok(())
}

async fn add_duplicate_findings(
    pool: &SqlitePool,
    pages: &[(i64, Option<Metadata>)],
) -> Result<(), sqlx::Error> {
    for (field, rule_id, label) in [
        ("title", "title.duplicate", "title"),
        ("description", "description.duplicate", "description"),
    ] {
        let mut values: HashMap<String, Vec<i64>> = HashMap::new();
        for (page_id, metadata) in pages {
            let Some(metadata) = metadata else { continue };
            let value = if field == "title" {
                &metadata.title
            } else {
                &metadata.description
            };
            if !value.is_empty() {
                values
                    .entry(audit::normalize_whitespace(value))
                    .or_default()
                    .push(*page_id);
            }
        }
        for (value, page_ids) in values.into_iter().filter(|(_, pages)| pages.len() > 1) {
            for page_id in page_ids {
                save_finding(
                    pool,
                    page_id,
                    &make_finding(
                        rule_id,
                        "metadata",
                        "warning",
                        "issue",
                        &value,
                        &format!("This {label} exactly matches another scanned page."),
                        &format!("Review whether the {label} should be distinct."),
                    ),
                )
                .await?;
            }
        }
    }
    Ok(())
}

fn make_finding(
    rule_id: &str,
    category: &str,
    severity: &str,
    status: &str,
    evidence: &str,
    explanation: &str,
    suggestion: &str,
) -> Finding {
    Finding {
        rule_id: rule_id.into(),
        category: category.into(),
        severity: severity.into(),
        status: status.into(),
        evidence: evidence.into(),
        explanation: explanation.into(),
        suggestion: suggestion.into(),
    }
}

fn is_html(content_type: &str) -> bool {
    let content_type = content_type.to_ascii_lowercase();
    content_type.starts_with("text/html") || content_type.starts_with("application/xhtml+xml")
}

fn origin_key(url: &Url) -> String {
    format!(
        "{}://{}:{}",
        url.scheme(),
        url.host_str().unwrap_or_default(),
        url.port_or_known_default().unwrap_or(0)
    )
}

fn path_key(url: &Url) -> String {
    format!("{}{}", origin_key(url), url.path())
}

fn fetch_key(url: &Url) -> String {
    let mut normalized = url.clone();
    normalized.set_fragment(None);
    normalized.to_string()
}

fn strip_fragment(url: &mut Url) {
    url.set_fragment(None);
}

fn validate_url(url: &Url, config: &Config) -> Result<(), FetchError> {
    if !matches!(url.scheme(), "http" | "https") {
        return Err(FetchError::Unsafe(
            "only HTTP and HTTPS URLs are allowed".into(),
        ));
    }
    if !url.username().is_empty() || url.password().is_some() {
        return Err(FetchError::Unsafe(
            "URLs with embedded credentials are not allowed".into(),
        ));
    }
    let port = url
        .port_or_known_default()
        .ok_or_else(|| FetchError::Unsafe("URL has no valid network port".into()))?;
    if !config.allowed_ports.contains(&port) {
        let fixture_port =
            config.fixture_enabled && url.host_str() == Some("127.0.0.1") && port == 4173;
        if !fixture_port {
            return Err(FetchError::Unsafe(format!(
                "port {port} is not in ALLOWED_PORTS"
            )));
        }
    }
    Ok(())
}

async fn resolve_public_addresses(
    host: &str,
    port: u16,
    config: &Config,
) -> Result<Vec<SocketAddr>, FetchError> {
    if config.fixture_enabled && host == "127.0.0.1" && port == 4173 {
        return Ok(vec![SocketAddr::new(
            IpAddr::V4(std::net::Ipv4Addr::LOCALHOST),
            port,
        )]);
    }
    let addresses = if let Ok(ip) = host.parse::<IpAddr>() {
        vec![SocketAddr::new(ip, port)]
    } else {
        tokio::net::lookup_host((host, port))
            .await
            .map_err(|e| FetchError::Network(format!("dns resolution failed: {e}")))?
            .collect::<Vec<_>>()
    };
    if addresses.is_empty() {
        return Err(FetchError::Network(
            "dns resolution returned no addresses".into(),
        ));
    }
    if addresses.iter().any(|address| !is_public_ip(address.ip())) {
        return Err(FetchError::Unsafe(
            "hostname resolved to a non-public address; request blocked".into(),
        ));
    }
    let mut unique = HashSet::new();
    let addresses = addresses
        .into_iter()
        .filter(|a| unique.insert(*a))
        .take(16)
        .collect::<Vec<_>>();
    if addresses.is_empty() {
        return Err(FetchError::Network(
            "dns resolution returned no usable addresses".into(),
        ));
    }
    Ok(addresses)
}

fn network_error(error: reqwest::Error) -> FetchError {
    if error.is_timeout() {
        FetchError::Timeout
    } else {
        FetchError::Network(format!("network request failed: {error}"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn url_boundaries_reject_credentials_schemes_and_unconfigured_ports() {
        let config = test_config();
        for raw in [
            "ftp://example.com/",
            "https://user:pass@example.com/",
            "http://example.com:8080/",
        ] {
            let url = Url::parse(raw).unwrap();
            assert!(validate_url(&url, &config).is_err(), "{raw}");
        }
    }

    #[test]
    fn hrefs_classify_email_telephone_fragments_and_cross_origin_links() {
        let base = Url::parse("https://example.com/path/page?q=1").unwrap();
        let links = resolve_occurrences(
            &[
                "#section".into(),
                "mailto:a@example.com".into(),
                "tel:+1".into(),
                "https://other.example/".into(),
            ],
            &base,
            &base,
            &origin_key(&base),
        );
        assert_eq!(links[0].classification, "fragment");
        assert_eq!(links[0].fragment.as_deref(), Some("section"));
        assert_eq!(links[1].classification, "email");
        assert_eq!(links[2].classification, "telephone");
        assert_eq!(links[3].classification, "external");
    }

    #[test]
    fn classifies_http_outcomes_without_calling_restrictions_broken() {
        assert_eq!(health_for_status(200, false), "healthy");
        assert_eq!(health_for_status(200, true), "redirected");
        assert_eq!(health_for_status(302, false), "redirect_loop");
        assert_eq!(health_for_status(404, false), "broken");
        assert_eq!(health_for_status(403, false), "restricted");
        assert_eq!(health_for_status(429, false), "rate_limited");
        assert_eq!(health_for_status(503, false), "server_error");
    }

    #[test]
    fn fetch_normalization_removes_fragments_but_preserves_query_and_path_case() {
        let url = Url::parse("https://example.test/Case/Path?b=2&a=1#section").unwrap();
        assert_eq!(fetch_key(&url), "https://example.test/Case/Path?b=2&a=1");
    }

    #[test]
    fn retry_after_is_parsed_and_bounded() {
        assert_eq!(parse_retry_after("12"), Some(Duration::from_secs(12)));
        assert_eq!(parse_retry_after("999"), Some(Duration::from_secs(60)));
        assert_eq!(parse_retry_after("invalid"), None);
    }

    #[test]
    fn robots_parser_applies_sitepaladin_disallow_rules() {
        let mut matcher = DefaultMatcher::default();
        let robots = "User-agent: SitePaladin\nDisallow: /private\n";
        assert!(!matcher.one_agent_allowed_by_robots(
            robots,
            ROBOTS_AGENT,
            "http://127.0.0.1:4173/private/hidden",
        ));
    }

    fn test_config() -> Config {
        Config {
            host: "127.0.0.1".into(),
            port: 3000,
            database_url: "sqlite::memory:".into(),
            operator_username: "operator".into(),
            operator_password_hash: String::new(),
            session_secret: "test-secret-long-enough".into(),
            cookie_secure: false,
            max_pages: 25,
            max_concurrency: 4,
            max_per_origin: 2,
            min_origin_delay: Duration::ZERO,
            request_timeout: Duration::from_secs(1),
            max_scan_duration: Duration::from_secs(30),
            max_response_bytes: 4096,
            max_unique_links: 200,
            max_redirects: 5,
            max_link_occurrences: 1000,
            max_robots_origins: 256,
            max_query_variants_per_path: 3,
            allowed_ports: vec![80, 443],
            database_path: Default::default(),
            fixture_enabled: false,
        }
    }
}
