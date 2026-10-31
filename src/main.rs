mod audit;
mod config;
mod crawler;

use argon2::{
    Argon2,
    password_hash::{PasswordHash, PasswordHasher, PasswordVerifier, SaltString, rand_core::OsRng},
};
use askama::Template;
use axum::{
    Json, Router,
    extract::{Form, Path, State},
    http::{StatusCode, header},
    middleware::{self, Next},
    response::{Html, IntoResponse, Redirect, Response},
    routing::{get, post},
};
use config::{Config, ScanLimits};
use serde::{Deserialize, Serialize};
use sqlx::{Row, SqlitePool, sqlite::SqliteConnectOptions};
use std::{
    collections::VecDeque,
    io::{self, BufRead},
    net::SocketAddr,
    str::FromStr,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use tokio::{sync::Notify, time::sleep};
use tokio_util::sync::CancellationToken;
use tower_sessions::{Expiry, MemoryStore, Session, SessionManagerLayer, cookie::SameSite};
use tracing_subscriber::EnvFilter;
use url::Url;

#[derive(Clone)]
struct AppState {
    pool: SqlitePool,
    config: Arc<Config>,
    active_scan: Arc<Mutex<bool>>,
    cancellations: Arc<Mutex<std::collections::HashMap<i64, CancellationToken>>>,
    scan_notify: Arc<Notify>,
    shutdown: CancellationToken,
    login_attempts: Arc<Mutex<VecDeque<Instant>>>,
}

struct WebError(Box<Response>);

impl IntoResponse for WebError {
    fn into_response(self) -> Response {
        *self.0
    }
}

#[derive(Clone, Debug, Serialize)]
struct ScanSummary {
    id: i64,
    seed_url: String,
    state: String,
    max_pages: i64,
    limits: ScanLimits,
    created_at: String,
    started_at: Option<String>,
    finished_at: Option<String>,
    error: Option<String>,
    pages_discovered: i64,
    pages_checked: i64,
    links_checked: i64,
    truncated: bool,
    duration_seconds: Option<i64>,
}

#[derive(Clone, Debug, Serialize)]
struct FindingView {
    rule_id: String,
    category: String,
    severity: String,
    status: String,
    evidence: String,
    explanation: String,
    suggestion: String,
}

#[derive(Clone, Debug, Serialize)]
struct LinkView {
    raw_href: String,
    classification: String,
    fragment: Option<String>,
    destination_url: Option<String>,
    health: Option<String>,
    status_code: Option<i64>,
    redirect_chain: String,
}

#[derive(Clone, Debug, Serialize)]
struct PageView {
    url: String,
    final_url: Option<String>,
    status_code: Option<i64>,
    fetch_state: String,
    redirect_chain: String,
    checked_at: String,
    findings: Vec<FindingView>,
    links: Vec<LinkView>,
}

#[derive(Template)]
#[template(path = "login.html")]
struct LoginTemplate {
    csrf_token: String,
    error: String,
}

#[derive(Template)]
#[template(path = "index.html")]
struct IndexTemplate {
    csrf_token: String,
    max_pages: usize,
    scans: Vec<ScanSummary>,
    active_scan: bool,
}

#[derive(Template)]
#[template(path = "report.html")]
struct ReportTemplate {
    csrf_token: String,
    scan: ScanSummary,
    pages: Vec<PageView>,
    issues: usize,
    warnings: usize,
    inconclusive: usize,
}

#[derive(Deserialize)]
struct LoginForm {
    username: String,
    password: String,
    csrf_token: String,
}

#[derive(Deserialize)]
struct ScanForm {
    url: String,
    max_pages: usize,
    csrf_token: String,
}

#[derive(Deserialize)]
struct CsrfForm {
    csrf_token: String,
}

#[derive(Serialize)]
struct ProgressView {
    state: String,
    pages_discovered: i64,
    pages_checked: i64,
    links_checked: i64,
    truncated: bool,
    error: Option<String>,
}

#[derive(Serialize)]
struct ExportReport {
    scan: ScanSummary,
    pages: Vec<PageView>,
}

#[tokio::main]
async fn main() {
    if std::env::args().nth(1).as_deref() == Some("hash-password") {
        let read_stdin = std::env::args().nth(2).as_deref() == Some("--stdin");
        if let Err(error) = hash_password_command(read_stdin) {
            eprintln!("{error}");
            std::process::exit(1);
        }
        return;
    }
    if let Err(error) = run().await {
        eprintln!("SitePaladin failed to start: {error}");
        std::process::exit(1);
    }
}

fn hash_password_command(read_stdin: bool) -> Result<(), String> {
    let password = if read_stdin {
        let mut password = String::new();
        io::stdin()
            .lock()
            .read_line(&mut password)
            .map_err(|error| format!("could not read password from stdin: {error}"))?;
        password.trim_end_matches(['\r', '\n']).to_owned()
    } else {
        rpassword::prompt_password("Operator password: ").map_err(|e| e.to_string())?
    };
    if password.chars().count() < 12 {
        return Err("Use a password with at least 12 characters.".into());
    }
    let salt = SaltString::generate(&mut OsRng);
    let hash = Argon2::default()
        .hash_password(password.as_bytes(), &salt)
        .map_err(|e| format!("could not hash password: {e}"))?;
    println!("{hash}");
    Ok(())
}

async fn run() -> Result<(), String> {
    if std::path::Path::new(".env").exists() {
        dotenvy::dotenv().map_err(|error| format!("could not load .env: {error}"))?;
    }
    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info")),
        )
        .init();
    let config = Config::from_env()?;
    config.parsed_password_hash()?;
    let session_key = tower_sessions::cookie::Key::try_from(config.session_secret.as_bytes())
        .map_err(|error| format!("invalid SESSION_SECRET: {error}"))?;
    if let Some(parent) = config.database_path.parent() {
        std::fs::create_dir_all(parent)
            .map_err(|e| format!("could not create database directory: {e}"))?;
    }
    let options = SqliteConnectOptions::from_str(&config.database_url)
        .map_err(|e| format!("invalid DATABASE_URL: {e}"))?
        .create_if_missing(true)
        .foreign_keys(true)
        .busy_timeout(Duration::from_secs(5))
        .journal_mode(sqlx::sqlite::SqliteJournalMode::Wal);
    let pool = SqlitePool::connect_with(options)
        .await
        .map_err(|e| format!("could not connect to SQLite: {e}"))?;
    sqlx::migrate!("./migrations")
        .run(&pool)
        .await
        .map_err(|e| format!("database migration failed: {e}"))?;
    sqlx::query("UPDATE scans SET state='interrupted', finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'), error='Process restarted while scan was running; partial results were preserved.' WHERE state='running'")
        .execute(&pool).await.map_err(|e| format!("could not recover previous scans: {e}"))?;

    let state = AppState {
        pool,
        config: Arc::new(config.clone()),
        active_scan: Arc::new(Mutex::new(false)),
        cancellations: Arc::new(Mutex::new(std::collections::HashMap::new())),
        scan_notify: Arc::new(Notify::new()),
        shutdown: CancellationToken::new(),
        login_attempts: Arc::new(Mutex::new(VecDeque::new())),
    };
    let worker = tokio::spawn(worker_loop(state.clone()));
    let shutdown = state.shutdown.clone();

    let public = Router::new()
        .route("/healthz", get(|| async { "ok" }))
        .route("/login", get(login_page).post(login))
        .route("/static/site.css", get(css))
        .route("/static/app.js", get(javascript));
    let protected = Router::new()
        .route("/", get(index))
        .route("/logout", post(logout))
        .route("/scans", post(create_scan))
        .route("/scans/{id}", get(report))
        .route("/scans/{id}/progress", get(progress))
        .route("/scans/{id}/cancel", post(cancel_scan))
        .route("/scans/{id}/export.json", get(export_json))
        .route("/scans/{id}/export.csv", get(export_csv))
        .route_layer(middleware::from_fn(require_auth));
    let app = public.merge(protected).with_state(state.clone()).layer(
        SessionManagerLayer::new(MemoryStore::default())
            .with_name("sitepaladin_session")
            .with_http_only(true)
            .with_same_site(SameSite::Lax)
            .with_secure(config.cookie_secure)
            .with_expiry(Expiry::OnInactivity(time::Duration::hours(12)))
            .with_signed(session_key),
    );
    let address: SocketAddr = format!("{}:{}", config.host, config.port)
        .parse()
        .map_err(|e| format!("invalid HOST/PORT: {e}"))?;
    let listener = tokio::net::TcpListener::bind(address)
        .await
        .map_err(|e| format!("could not listen on {address}: {e}"))?;
    tracing::info!(%address, "SitePaladin is listening");
    axum::serve(listener, app)
        .with_graceful_shutdown(async move {
            shutdown_signal().await;
            shutdown.cancel();
        })
        .await
        .map_err(|e| format!("HTTP server failed: {e}"))?;
    worker
        .await
        .map_err(|e| format!("scan worker failed: {e}"))?;
    Ok(())
}

async fn shutdown_signal() {
    let ctrl_c = async {
        if let Err(error) = tokio::signal::ctrl_c().await {
            tracing::error!(%error, "failed to listen for Ctrl-C");
        }
    };
    #[cfg(unix)]
    let terminate = async {
        match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            Ok(mut signal) => {
                signal.recv().await;
            }
            Err(error) => tracing::error!(%error, "failed to listen for terminate signal"),
        }
    };
    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();
    tokio::select! { _ = ctrl_c => {}, _ = terminate => {} }
}

async fn worker_loop(state: AppState) {
    loop {
        if state.shutdown.is_cancelled() {
            return;
        }
        let next = sqlx::query("SELECT id FROM scans WHERE state='queued' ORDER BY id LIMIT 1")
            .fetch_optional(&state.pool)
            .await;
        match next {
            Ok(Some(row)) => {
                let scan_id: i64 = row.get("id");
                let can_start = match state.active_scan.lock() {
                    Ok(mut active) if !*active => {
                        *active = true;
                        true
                    }
                    Ok(_) => false,
                    Err(error) => {
                        tracing::error!(%error, "scan worker lock was poisoned");
                        false
                    }
                };
                if !can_start {
                    sleep(Duration::from_millis(250)).await;
                    continue;
                }
                let cancel = CancellationToken::new();
                match state.cancellations.lock() {
                    Ok(mut cancels) => {
                        cancels.insert(scan_id, cancel.clone());
                    }
                    Err(error) => tracing::error!(%error, "cancellation lock was poisoned"),
                }
                let mut scan = tokio::spawn(crawler::run_scan(
                    state.pool.clone(),
                    (*state.config).clone(),
                    scan_id,
                    cancel.clone(),
                ));
                tokio::select! {
                    result = &mut scan => {
                        if let Err(error) = result {
                            tracing::error!(scan_id, %error, "scan task failed unexpectedly");
                        }
                    }
                    _ = state.shutdown.cancelled() => {
                        cancel.cancel();
                        if let Err(error) = scan.await {
                            tracing::error!(scan_id, %error, "scan task failed during shutdown");
                        }
                    }
                }
                if let Ok(mut cancels) = state.cancellations.lock() {
                    cancels.remove(&scan_id);
                }
                if let Ok(mut active) = state.active_scan.lock() {
                    *active = false;
                }
            }
            Ok(None) => {
                tokio::select! {
                    _ = state.shutdown.cancelled() => return,
                    _ = state.scan_notify.notified() => {}
                    _ = sleep(Duration::from_secs(1)) => {}
                }
            }
            Err(error) => {
                tracing::error!(%error, "scan worker could not read queued jobs");
                sleep(Duration::from_secs(2)).await;
            }
        }
    }
}

async fn require_auth(session: Session, request: axum::extract::Request, next: Next) -> Response {
    match session.get::<bool>("authenticated").await {
        Ok(Some(true)) => next.run(request).await,
        Ok(_) => Redirect::to("/login").into_response(),
        Err(error) => {
            tracing::error!(%error, "could not read operator session");
            StatusCode::INTERNAL_SERVER_ERROR.into_response()
        }
    }
}

async fn login_page(session: Session) -> Result<Html<String>, WebError> {
    let token = csrf_token(&session).await?;
    render(LoginTemplate {
        csrf_token: token,
        error: String::new(),
    })
}

async fn login(
    State(state): State<AppState>,
    session: Session,
    Form(form): Form<LoginForm>,
) -> Result<Response, WebError> {
    verify_csrf(&session, &form.csrf_token).await?;
    let now = Instant::now();
    {
        let mut attempts = state.login_attempts.lock().map_err(|error| {
            tracing::error!(%error, "login rate-limit lock was poisoned");
            error_response(
                StatusCode::INTERNAL_SERVER_ERROR,
                "Login is temporarily unavailable.",
            )
        })?;
        while attempts
            .front()
            .is_some_and(|time| now.duration_since(*time) > Duration::from_secs(60))
        {
            attempts.pop_front();
        }
        if attempts.len() >= 5 {
            return Err(error_response(
                StatusCode::TOO_MANY_REQUESTS,
                "Too many login attempts. Wait one minute and try again.",
            ));
        }
        attempts.push_back(now);
    }
    let parsed = PasswordHash::new(&state.config.operator_password_hash).map_err(|error| {
        tracing::error!(%error, "configured operator password hash became invalid");
        error_response(
            StatusCode::INTERNAL_SERVER_ERROR,
            "Operator authentication is misconfigured.",
        )
    })?;
    let valid = form.username == state.config.operator_username
        && Argon2::default()
            .verify_password(form.password.as_bytes(), &parsed)
            .is_ok();
    if !valid {
        let token = csrf_token(&session).await?;
        return render(LoginTemplate {
            csrf_token: token,
            error: "The username or password was not accepted.".into(),
        })
        .map(IntoResponse::into_response);
    }
    session.cycle_id().await.map_err(|error| {
        tracing::error!(%error, "could not rotate session identifier");
        error_response(
            StatusCode::INTERNAL_SERVER_ERROR,
            "Could not establish an operator session.",
        )
    })?;
    session
        .insert("authenticated", true)
        .await
        .map_err(|error| {
            tracing::error!(%error, "could not save authenticated session");
            error_response(
                StatusCode::INTERNAL_SERVER_ERROR,
                "Could not establish an operator session.",
            )
        })?;
    state
        .login_attempts
        .lock()
        .map_err(|error| {
            tracing::error!(%error, "login rate-limit lock was poisoned");
            error_response(
                StatusCode::INTERNAL_SERVER_ERROR,
                "Login is temporarily unavailable.",
            )
        })?
        .clear();
    Ok(Redirect::to("/").into_response())
}

async fn logout(session: Session, Form(form): Form<CsrfForm>) -> Result<Redirect, WebError> {
    verify_csrf(&session, &form.csrf_token).await?;
    session.delete().await.map_err(|error| {
        tracing::error!(%error, "could not delete operator session");
        error_response(StatusCode::INTERNAL_SERVER_ERROR, "Could not log out.")
    })?;
    Ok(Redirect::to("/login"))
}

async fn index(State(state): State<AppState>, session: Session) -> Result<Html<String>, WebError> {
    let scans = load_scans(&state.pool, 20).await.map_err(internal_error)?;
    let active_scan = scans
        .iter()
        .any(|scan| matches!(scan.state.as_str(), "queued" | "running"));
    let csrf_token = csrf_token(&session).await?;
    render(IndexTemplate {
        csrf_token,
        max_pages: state.config.max_pages,
        scans,
        active_scan,
    })
}

async fn create_scan(
    State(state): State<AppState>,
    session: Session,
    Form(form): Form<ScanForm>,
) -> Result<Redirect, WebError> {
    verify_csrf(&session, &form.csrf_token).await?;
    if form.max_pages == 0 || form.max_pages > state.config.max_pages {
        return Err(error_response(
            StatusCode::BAD_REQUEST,
            "Page limit must be within the operator-configured range.",
        ));
    }
    let seed = validate_seed(&form.url, &state.config)
        .map_err(|message| error_response(StatusCode::BAD_REQUEST, &message))?;
    let active: i64 =
        sqlx::query_scalar("SELECT COUNT(*) FROM scans WHERE state IN ('queued','running')")
            .fetch_one(&state.pool)
            .await
            .map_err(internal_error)?;
    if active > 0 {
        return Err(error_response(
            StatusCode::CONFLICT,
            "Only one scan may be queued or running at a time.",
        ));
    }
    let limits = state.config.scan_limits(form.max_pages);
    let snapshot = serde_json::to_string(&limits).map_err(internal_error)?;
    let result = sqlx::query(
        "INSERT INTO scans(seed_url,state,max_pages,config_snapshot) VALUES(?,'queued',?,?)",
    )
    .bind(seed.as_str())
    .bind(form.max_pages as i64)
    .bind(snapshot)
    .execute(&state.pool)
    .await
    .map_err(internal_error)?;
    state.scan_notify.notify_one();
    Ok(Redirect::to(&format!(
        "/scans/{}",
        result.last_insert_rowid()
    )))
}

async fn report(
    State(state): State<AppState>,
    Path(id): Path<i64>,
    session: Session,
) -> Result<Html<String>, WebError> {
    let scan = load_scan(&state.pool, id).await?;
    let pages = load_pages(&state.pool, id).await?;
    let csrf_token = csrf_token(&session).await?;
    let issues = pages
        .iter()
        .flat_map(|page| &page.findings)
        .filter(|finding| finding.status == "issue")
        .count();
    let warnings = pages
        .iter()
        .flat_map(|page| &page.findings)
        .filter(|finding| finding.status == "warning")
        .count();
    let inconclusive = pages
        .iter()
        .flat_map(|page| &page.findings)
        .filter(|finding| finding.status == "not_checked")
        .count()
        + pages
            .iter()
            .flat_map(|page| &page.links)
            .filter(|link| {
                matches!(
                    link.health.as_deref(),
                    Some("inconclusive" | "skipped" | "pending")
                )
            })
            .count();
    render(ReportTemplate {
        csrf_token,
        scan,
        pages,
        issues,
        warnings,
        inconclusive,
    })
}

async fn progress(
    State(state): State<AppState>,
    Path(id): Path<i64>,
) -> Result<Json<ProgressView>, WebError> {
    let row = sqlx::query("SELECT state,pages_discovered,pages_checked,links_checked,truncated,error FROM scans WHERE id=?")
        .bind(id).fetch_optional(&state.pool).await.map_err(internal_error)?
        .ok_or_else(|| error_response(StatusCode::NOT_FOUND, "Scan not found."))?;
    Ok(Json(ProgressView {
        state: row.get("state"),
        pages_discovered: row.get("pages_discovered"),
        pages_checked: row.get("pages_checked"),
        links_checked: row.get("links_checked"),
        truncated: row.get::<i64, _>("truncated") != 0,
        error: row.get("error"),
    }))
}

async fn cancel_scan(
    State(state): State<AppState>,
    Path(id): Path<i64>,
    session: Session,
    Form(form): Form<CsrfForm>,
) -> Result<Redirect, WebError> {
    verify_csrf(&session, &form.csrf_token).await?;
    let token = state
        .cancellations
        .lock()
        .map_err(|error| {
            tracing::error!(%error, "cancellation lock was poisoned");
            error_response(
                StatusCode::INTERNAL_SERVER_ERROR,
                "Cancellation is temporarily unavailable.",
            )
        })?
        .get(&id)
        .cloned();
    if let Some(token) = token {
        token.cancel();
    } else {
        let result = sqlx::query("UPDATE scans SET state='cancelled',finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND state='queued'")
            .bind(id).execute(&state.pool).await.map_err(internal_error)?;
        if result.rows_affected() == 0 {
            let exists: Option<i64> = sqlx::query_scalar("SELECT id FROM scans WHERE id=?")
                .bind(id)
                .fetch_optional(&state.pool)
                .await
                .map_err(internal_error)?;
            if exists.is_none() {
                return Err(error_response(StatusCode::NOT_FOUND, "Scan not found."));
            }
        }
    }
    Ok(Redirect::to(&format!("/scans/{id}")))
}

async fn export_json(
    State(state): State<AppState>,
    Path(id): Path<i64>,
) -> Result<Response, WebError> {
    let report = ExportReport {
        scan: load_scan(&state.pool, id).await?,
        pages: load_pages(&state.pool, id).await?,
    };
    let body = serde_json::to_vec_pretty(&report).map_err(internal_error)?;
    Ok((
        [
            (header::CONTENT_TYPE, "application/json; charset=utf-8"),
            (
                header::CONTENT_DISPOSITION,
                "attachment; filename=\"sitepaladin-scan.json\"",
            ),
        ],
        body,
    )
        .into_response())
}

async fn export_csv(
    State(state): State<AppState>,
    Path(id): Path<i64>,
) -> Result<Response, WebError> {
    let scan = load_scan(&state.pool, id).await?;
    let scan_id = scan.id.to_string();
    let mut writer = csv::Writer::from_writer(Vec::new());
    writer
        .write_record([
            "scan_id",
            "scan_target",
            "scan_state",
            "scan_created_at",
            "scan_started_at",
            "scan_finished_at",
            "page_url",
            "record_type",
            "rule_id",
            "category",
            "severity",
            "status",
            "evidence",
            "explanation",
            "suggestion",
            "link_url",
            "link_health",
            "http_status",
            "redirect_chain",
        ])
        .map_err(internal_error)?;
    for page in load_pages(&state.pool, id).await? {
        for finding in &page.findings {
            writer
                .write_record([
                    csv_safe(&scan_id),
                    csv_safe(&scan.seed_url),
                    csv_safe(&scan.state),
                    csv_safe(&scan.created_at),
                    csv_safe(scan.started_at.as_deref().unwrap_or_default()),
                    csv_safe(scan.finished_at.as_deref().unwrap_or_default()),
                    csv_safe(&page.url),
                    "finding".into(),
                    csv_safe(&finding.rule_id),
                    csv_safe(&finding.category),
                    csv_safe(&finding.severity),
                    csv_safe(&finding.status),
                    csv_safe(&finding.evidence),
                    csv_safe(&finding.explanation),
                    csv_safe(&finding.suggestion),
                    String::new(),
                    String::new(),
                    String::new(),
                    String::new(),
                ])
                .map_err(internal_error)?;
        }
        for link in &page.links {
            let status_code = link.status_code.map(|code| code.to_string());
            writer
                .write_record([
                    csv_safe(&scan_id),
                    csv_safe(&scan.seed_url),
                    csv_safe(&scan.state),
                    csv_safe(&scan.created_at),
                    csv_safe(scan.started_at.as_deref().unwrap_or_default()),
                    csv_safe(scan.finished_at.as_deref().unwrap_or_default()),
                    csv_safe(&page.url),
                    "link".into(),
                    String::new(),
                    "links".into(),
                    String::new(),
                    csv_safe(&link.classification),
                    csv_safe(&link.raw_href),
                    String::new(),
                    String::new(),
                    csv_safe(link.destination_url.as_deref().unwrap_or_default()),
                    csv_safe(link.health.as_deref().unwrap_or_default()),
                    csv_safe(status_code.as_deref().unwrap_or_default()),
                    csv_safe(&link.redirect_chain),
                ])
                .map_err(internal_error)?;
        }
    }
    let body = writer.into_inner().map_err(internal_error)?;
    Ok((
        [
            (header::CONTENT_TYPE, "text/csv; charset=utf-8"),
            (
                header::CONTENT_DISPOSITION,
                "attachment; filename=\"sitepaladin-scan.csv\"",
            ),
        ],
        body,
    )
        .into_response())
}

fn csv_safe(value: &str) -> String {
    if value
        .trim_start_matches([' ', '\t', '\r', '\n'])
        .starts_with(['=', '+', '-', '@', '\t', '\r', '\n'])
    {
        format!("'{value}")
    } else {
        value.to_owned()
    }
}

async fn css() -> impl IntoResponse {
    (
        [(header::CONTENT_TYPE, "text/css; charset=utf-8")],
        include_str!("../static/site.css"),
    )
}

async fn javascript() -> impl IntoResponse {
    (
        [(header::CONTENT_TYPE, "text/javascript; charset=utf-8")],
        include_str!("../static/app.js"),
    )
}

async fn csrf_token(session: &Session) -> Result<String, WebError> {
    if let Some(token) = session
        .get::<String>("csrf_token")
        .await
        .map_err(session_error)?
    {
        return Ok(token);
    }
    let token = uuid::Uuid::new_v4().simple().to_string();
    session
        .insert("csrf_token", &token)
        .await
        .map_err(session_error)?;
    Ok(token)
}

async fn verify_csrf(session: &Session, supplied: &str) -> Result<(), WebError> {
    let expected = session
        .get::<String>("csrf_token")
        .await
        .map_err(session_error)?;
    match expected {
        Some(expected) if constant_time_eq(expected.as_bytes(), supplied.as_bytes()) => Ok(()),
        _ => Err(error_response(
            StatusCode::FORBIDDEN,
            "The form token is invalid or expired. Reload the page and try again.",
        )),
    }
}

fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    if left.len() != right.len() {
        return false;
    }
    left.iter()
        .zip(right)
        .fold(0_u8, |diff, (a, b)| diff | (a ^ b))
        == 0
}

fn validate_seed(raw: &str, config: &Config) -> Result<Url, String> {
    let mut url = Url::parse(raw.trim()).map_err(|e| format!("Enter a valid HTTP(S) URL: {e}"))?;
    if !matches!(url.scheme(), "http" | "https") {
        return Err("Only HTTP and HTTPS URLs are accepted.".into());
    }
    if !url.username().is_empty() || url.password().is_some() {
        return Err("URLs containing embedded credentials are not accepted.".into());
    }
    let port = url
        .port_or_known_default()
        .ok_or("The URL has no valid network port.")?;
    let fixture_port =
        config.fixture_enabled && url.host_str() == Some("127.0.0.1") && port == 4173;
    if !config.allowed_ports.contains(&port) && !fixture_port {
        return Err(format!(
            "Port {port} is not allowed by the operator configuration."
        ));
    }
    url.set_fragment(None);
    Ok(url)
}

async fn load_scans(pool: &SqlitePool, limit: i64) -> Result<Vec<ScanSummary>, sqlx::Error> {
    let rows = sqlx::query("SELECT *, CASE WHEN started_at IS NOT NULL THEN CAST((julianday(COALESCE(finished_at,strftime('%Y-%m-%dT%H:%M:%fZ','now')))-julianday(started_at))*86400 AS INTEGER) END AS duration_seconds FROM scans ORDER BY created_at DESC LIMIT ?")
        .bind(limit).fetch_all(pool).await?;
    rows.iter().map(scan_from_row).collect()
}

async fn load_scan(pool: &SqlitePool, id: i64) -> Result<ScanSummary, WebError> {
    let row = sqlx::query("SELECT *, CASE WHEN started_at IS NOT NULL THEN CAST((julianday(COALESCE(finished_at,strftime('%Y-%m-%dT%H:%M:%fZ','now')))-julianday(started_at))*86400 AS INTEGER) END AS duration_seconds FROM scans WHERE id=?")
        .bind(id).fetch_optional(pool).await.map_err(internal_error)?
        .ok_or_else(|| error_response(StatusCode::NOT_FOUND, "Scan not found."))?;
    scan_from_row(&row).map_err(internal_error)
}

fn scan_from_row(row: &sqlx::sqlite::SqliteRow) -> Result<ScanSummary, sqlx::Error> {
    let snapshot: String = row.try_get("config_snapshot")?;
    let limits =
        serde_json::from_str(&snapshot).map_err(|error| sqlx::Error::Decode(Box::new(error)))?;
    Ok(ScanSummary {
        id: row.try_get("id")?,
        seed_url: row.try_get("seed_url")?,
        state: row.try_get("state")?,
        max_pages: row.try_get("max_pages")?,
        limits,
        created_at: row.try_get("created_at")?,
        started_at: row.try_get("started_at")?,
        finished_at: row.try_get("finished_at")?,
        error: row.try_get("error")?,
        pages_discovered: row.try_get("pages_discovered")?,
        pages_checked: row.try_get("pages_checked")?,
        links_checked: row.try_get("links_checked")?,
        truncated: row.try_get::<i64, _>("truncated")? != 0,
        duration_seconds: row.try_get("duration_seconds")?,
    })
}

async fn load_pages(pool: &SqlitePool, scan_id: i64) -> Result<Vec<PageView>, WebError> {
    let page_rows = sqlx::query("SELECT * FROM pages WHERE scan_id=? ORDER BY id")
        .bind(scan_id)
        .fetch_all(pool)
        .await
        .map_err(internal_error)?;
    let mut pages = Vec::new();
    for row in page_rows {
        let page_id: i64 = row.get("id");
        let finding_rows = sqlx::query("SELECT rule_id,category,severity,status,evidence,explanation,suggestion FROM findings WHERE page_id=? ORDER BY id")
            .bind(page_id).fetch_all(pool).await.map_err(internal_error)?;
        let findings = finding_rows
            .iter()
            .map(|finding| FindingView {
                rule_id: finding.get("rule_id"),
                category: finding.get("category"),
                severity: finding.get("severity"),
                status: finding.get("status"),
                evidence: finding.get("evidence"),
                explanation: finding.get("explanation"),
                suggestion: finding.get("suggestion"),
            })
            .collect();
        let link_rows = sqlx::query(        "SELECT lo.raw_href,lo.classification,lo.fragment,d.url,d.health,d.status_code,COALESCE(d.redirect_chain,'[]') AS redirect_chain FROM link_occurrences lo LEFT JOIN destinations d ON d.id=lo.destination_id WHERE lo.page_id=? ORDER BY lo.id")
            .bind(page_id).fetch_all(pool).await.map_err(internal_error)?;
        let links = link_rows
            .iter()
            .map(|link| LinkView {
                raw_href: link.get("raw_href"),
                classification: link.get("classification"),
                fragment: link.get("fragment"),
                destination_url: link.get("url"),
                health: link.get("health"),
                status_code: link.get("status_code"),
                redirect_chain: link.get("redirect_chain"),
            })
            .collect();
        pages.push(PageView {
            url: row.get("url"),
            final_url: row.get("final_url"),
            status_code: row.get("status_code"),
            fetch_state: row.get("fetch_state"),
            redirect_chain: row.get("redirect_chain"),
            checked_at: row.get("checked_at"),
            findings,
            links,
        });
    }
    Ok(pages)
}

fn render<T: Template>(template: T) -> Result<Html<String>, WebError> {
    template.render().map(Html).map_err(|error| {
        tracing::error!(%error, "could not render HTML template");
        error_response(
            StatusCode::INTERNAL_SERVER_ERROR,
            "Could not render this page.",
        )
    })
}

fn session_error(error: tower_sessions::session::Error) -> WebError {
    tracing::error!(%error, "session operation failed");
    error_response(StatusCode::INTERNAL_SERVER_ERROR, "Session storage failed.")
}

fn internal_error(error: impl std::fmt::Display) -> WebError {
    tracing::error!(%error, "request failed");
    error_response(
        StatusCode::INTERNAL_SERVER_ERROR,
        "The request could not be completed.",
    )
}

fn error_response(status: StatusCode, message: &str) -> WebError {
    WebError(Box::new(
        (
            status,
            [(header::CONTENT_TYPE, "text/plain; charset=utf-8")],
            message.to_owned(),
        )
            .into_response(),
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;

    #[test]
    fn csv_formula_injection_is_escaped_without_changing_json_values() {
        for value in ["=1+1", " +SUM(A1:A2)", "-1+2", "@cmd", "\t=1"] {
            assert!(csv_safe(value).starts_with('\''));
        }
        assert_eq!(csv_safe("normal text"), "normal text");
        assert_eq!(serde_json::to_string("=1+1").unwrap(), "\"=1+1\"");
    }

    #[test]
    fn url_input_rejects_non_http_and_userinfo() {
        let config = Config {
            host: "127.0.0.1".into(),
            port: 3000,
            database_url: "sqlite::memory:".into(),
            operator_username: "operator".into(),
            operator_password_hash: String::new(),
            session_secret:
                "test-secret-long-enough-for-a-test-key-0000000000000000000000000000000000000000"
                    .into(),
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
            allowed_ports: vec![80, 443],
            max_link_occurrences: 1000,
            max_query_variants_per_path: 3,
            max_robots_origins: 256,
            database_path: Default::default(),
            fixture_enabled: false,
        };
        assert!(validate_seed("file:///etc/passwd", &config).is_err());
        assert!(validate_seed("https://user:pass@example.com/", &config).is_err());
    }

    #[test]
    fn csrf_comparison_handles_mismatched_and_matching_tokens() {
        assert!(constant_time_eq(b"token", b"token"));
        assert!(!constant_time_eq(b"token", b"other"));
        assert!(!constant_time_eq(b"short", b"longer"));
    }

    #[test]
    fn login_template_escapes_untrusted_error_text() {
        let html = LoginTemplate {
            csrf_token: "token".into(),
            error: "<img src=x onerror=alert(1)>".into(),
        }
        .render()
        .unwrap();
        assert!(!html.contains("<img"));
        assert!(html.contains("&#60;img"));
    }

    #[tokio::test]
    async fn csrf_requires_a_matching_server_side_session_token() {
        let session = Session::new(None, Arc::new(MemoryStore::default()), None);
        session.insert("csrf_token", "valid-token").await.unwrap();
        assert!(verify_csrf(&session, "valid-token").await.is_ok());
        assert!(verify_csrf(&session, "attacker-token").await.is_err());
    }

    #[tokio::test]
    async fn sqlite_schema_persists_scans_and_enforces_one_active_job() {
        let pool = sqlx::sqlite::SqlitePoolOptions::new()
            .max_connections(1)
            .connect("sqlite::memory:")
            .await
            .unwrap();
        sqlx::migrate!("./migrations").run(&pool).await.unwrap();
        sqlx::query("INSERT INTO scans(seed_url,state,max_pages) VALUES('https://example.test/','queued',1)")
            .execute(&pool).await.unwrap();
        let second = sqlx::query(
            "INSERT INTO scans(seed_url,state,max_pages) VALUES('https://other.test/','queued',1)",
        )
        .execute(&pool)
        .await;
        assert!(second.is_err());
        let count: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM scans WHERE state='queued'")
            .fetch_one(&pool)
            .await
            .unwrap();
        assert_eq!(count, 1);
    }
}
