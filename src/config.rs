use serde::{Deserialize, Serialize};
use std::{env, net::IpAddr, path::PathBuf, str::FromStr, time::Duration};

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ScanLimits {
    pub max_pages: usize,
    pub max_concurrency: usize,
    pub max_per_origin: usize,
    pub min_origin_delay_ms: u64,
    pub request_timeout_secs: u64,
    pub max_scan_duration_secs: u64,
    pub max_response_bytes: usize,
    pub max_unique_links: usize,
    pub max_link_occurrences: usize,
    pub max_query_variants_per_path: usize,
    pub max_robots_origins: usize,
    pub max_redirects: usize,
    pub allowed_ports: Vec<u16>,
}

#[derive(Clone)]
pub struct Config {
    pub host: String,
    pub port: u16,
    pub database_url: String,
    pub operator_username: String,
    pub operator_password_hash: String,
    pub session_secret: String,
    pub cookie_secure: bool,
    pub max_pages: usize,
    pub max_concurrency: usize,
    pub max_per_origin: usize,
    pub min_origin_delay: Duration,
    pub request_timeout: Duration,
    pub max_scan_duration: Duration,
    pub max_response_bytes: usize,
    pub max_unique_links: usize,
    pub max_link_occurrences: usize,
    pub max_query_variants_per_path: usize,
    pub max_robots_origins: usize,
    pub max_redirects: usize,
    pub allowed_ports: Vec<u16>,
    pub database_path: PathBuf,
    pub fixture_enabled: bool,
}

impl Config {
    pub fn scan_limits(&self, max_pages: usize) -> ScanLimits {
        ScanLimits {
            max_pages,
            max_concurrency: self.max_concurrency,
            max_per_origin: self.max_per_origin,
            min_origin_delay_ms: self.min_origin_delay.as_millis() as u64,
            request_timeout_secs: self.request_timeout.as_secs(),
            max_scan_duration_secs: self.max_scan_duration.as_secs(),
            max_response_bytes: self.max_response_bytes,
            max_unique_links: self.max_unique_links,
            max_link_occurrences: self.max_link_occurrences,
            max_query_variants_per_path: self.max_query_variants_per_path,
            max_robots_origins: self.max_robots_origins,
            max_redirects: self.max_redirects,
            allowed_ports: self.allowed_ports.clone(),
        }
    }

    pub fn restrict_with_snapshot(&self, limits: &ScanLimits) -> Self {
        let mut effective = self.clone();
        effective.max_pages = effective.max_pages.min(limits.max_pages);
        effective.max_concurrency = effective.max_concurrency.min(limits.max_concurrency);
        effective.max_per_origin = effective.max_per_origin.min(limits.max_per_origin);
        effective.min_origin_delay = effective
            .min_origin_delay
            .max(Duration::from_millis(limits.min_origin_delay_ms));
        effective.request_timeout = effective
            .request_timeout
            .min(Duration::from_secs(limits.request_timeout_secs));
        effective.max_scan_duration = effective
            .max_scan_duration
            .min(Duration::from_secs(limits.max_scan_duration_secs));
        effective.max_response_bytes = effective.max_response_bytes.min(limits.max_response_bytes);
        effective.max_unique_links = effective.max_unique_links.min(limits.max_unique_links);
        effective.max_link_occurrences = effective
            .max_link_occurrences
            .min(limits.max_link_occurrences);
        effective.max_query_variants_per_path = effective
            .max_query_variants_per_path
            .min(limits.max_query_variants_per_path);
        effective.max_robots_origins = effective.max_robots_origins.min(limits.max_robots_origins);
        effective.max_redirects = effective.max_redirects.min(limits.max_redirects);
        effective
            .allowed_ports
            .retain(|port| limits.allowed_ports.contains(port));
        effective
    }

    pub fn from_env() -> Result<Self, String> {
        let host = value("HOST", "127.0.0.1");
        let port = parse("PORT", 3000_u16)?;
        let database_url = value("DATABASE_URL", "sqlite://data/sitepaladin.db");
        let database_path = database_url
            .strip_prefix("sqlite://")
            .filter(|s| !s.is_empty() && *s != ":memory:")
            .map(PathBuf::from)
            .ok_or("DATABASE_URL must be a file-backed sqlite:// URL")?;
        let operator_password_hash = env::var("OPERATOR_PASSWORD_HASH")
            .map_err(|_| "OPERATOR_PASSWORD_HASH is required; there is no default password")?;
        if operator_password_hash.is_empty() {
            return Err("OPERATOR_PASSWORD_HASH must not be empty".into());
        }
        let session_secret = env::var("SESSION_SECRET")
            .map_err(|_| "SESSION_SECRET is required; generate a random secret")?;
        if session_secret.len() < 64 {
            return Err("SESSION_SECRET must contain at least 64 characters; generate it with openssl rand -hex 32".into());
        }
        let max_pages = bounded("MAX_PAGES", 25, 1, 500)?;
        let max_concurrency = bounded("MAX_CONCURRENCY", 4, 1, 16)?;
        let max_per_origin = bounded("MAX_PER_ORIGIN", 2, 1, max_concurrency)?;
        let min_origin_delay_ms = bounded("MIN_ORIGIN_DELAY_MS", 250, 0, 60_000)?;
        let request_timeout_secs = bounded("REQUEST_TIMEOUT_SECS", 15, 1, 120)?;
        let max_scan_duration_secs = bounded("MAX_SCAN_DURATION_SECS", 600, 5, 7200)?;
        let max_response_bytes = bounded("MAX_RESPONSE_BYTES", 2_000_000, 1024, 20_000_000)?;
        let max_unique_links = bounded("MAX_UNIQUE_LINKS", 200, 1, 10_000)?;
        let max_link_occurrences = bounded("MAX_LINK_OCCURRENCES", 10_000, 100, 100_000)?;
        let max_query_variants_per_path = bounded("MAX_QUERY_VARIANTS_PER_PATH", 3, 1, 50)?;
        let max_robots_origins = bounded("MAX_ROBOTS_ORIGINS", 256, 1, 4096)?;
        let max_redirects = bounded("MAX_REDIRECTS", 5, 0, 10)?;
        let allowed_ports = value("ALLOWED_PORTS", "80,443")
            .split(',')
            .map(|p| {
                p.trim()
                    .parse::<u16>()
                    .map_err(|e| format!("invalid ALLOWED_PORTS value: {e}"))
            })
            .collect::<Result<Vec<_>, _>>()?;
        if allowed_ports.is_empty() {
            return Err("ALLOWED_PORTS must include at least one port".into());
        }
        let fixture_enabled = cfg!(feature = "dev-fixtures")
            && env::var("ALLOW_LOCAL_FIXTURE").as_deref() == Ok("true");

        Ok(Self {
            host,
            port,
            database_url,
            operator_username: value("OPERATOR_USERNAME", "operator"),
            operator_password_hash,
            session_secret,
            cookie_secure: parse("COOKIE_SECURE", false)?,
            max_pages,
            max_concurrency,
            max_per_origin,
            min_origin_delay: Duration::from_millis(min_origin_delay_ms as u64),
            request_timeout: Duration::from_secs(request_timeout_secs as u64),
            max_scan_duration: Duration::from_secs(max_scan_duration_secs as u64),
            max_response_bytes,
            max_unique_links,
            max_link_occurrences,
            max_query_variants_per_path,
            max_robots_origins,
            max_redirects,
            allowed_ports,
            database_path,
            fixture_enabled,
        })
    }

    pub fn parsed_password_hash(&self) -> Result<argon2::password_hash::PasswordHash<'_>, String> {
        argon2::password_hash::PasswordHash::new(&self.operator_password_hash)
            .map_err(|e| format!("OPERATOR_PASSWORD_HASH is not a valid Argon2 PHC string: {e}"))
    }
}

fn value(name: &str, default: &str) -> String {
    env::var(name).unwrap_or_else(|_| default.to_owned())
}

fn parse<T: FromStr>(name: &str, default: T) -> Result<T, String>
where
    T::Err: std::fmt::Display,
{
    env::var(name)
        .map(|v| {
            v.parse()
                .map_err(|e| format!("{name} has invalid value: {e}"))
        })
        .unwrap_or(Ok(default))
}

fn bounded(name: &str, default: usize, min: usize, max: usize) -> Result<usize, String> {
    let value = parse(name, default)?;
    if !(min..=max).contains(&value) {
        return Err(format!("{name} must be between {min} and {max}"));
    }
    Ok(value)
}

pub fn is_public_ip(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(ip) => {
            let [a, b, _, _] = ip.octets();
            !(ip.is_private()
                || ip.is_loopback()
                || ip.is_link_local()
                || ip.is_multicast()
                || ip.is_unspecified()
                || ip.is_broadcast()
                || a == 0
                || a >= 224
                || (a == 100 && (64..=127).contains(&b))
                || (a == 192 && (b == 0 || b == 2 || b == 88))
                || (a == 198 && (b == 18 || b == 19 || b == 51))
                || (a == 203 && b == 0))
        }
        IpAddr::V6(ip) => {
            if let Some(mapped) = ip.to_ipv4_mapped() {
                return is_public_ip(IpAddr::V4(mapped));
            }
            let segments = ip.segments();
            !((segments[0] & 0xe000) != 0x2000
                || ip.is_loopback()
                || ip.is_unspecified()
                || ip.is_multicast()
                || (segments[0] & 0xfe00) == 0xfc00
                || (segments[0] & 0xffc0) == 0xfe80
                || (segments[0] & 0xff00) == 0xff00
                || (segments[0] == 0x2001 && (segments[1] <= 0x01ff || segments[1] == 0x0db8))
                || segments[0] == 0x2002
                || (segments[0] == 0x0064 && segments[1] == 0xff9b))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::IpAddr;

    #[test]
    fn rejects_non_public_ipv4_and_ipv6_addresses() {
        for address in [
            "127.0.0.1",
            "10.0.0.1",
            "169.254.1.1",
            "192.168.1.1",
            "100.64.0.1",
            "224.0.0.1",
            "0.0.0.0",
            "::1",
            "::",
            "fc00::1",
            "fe80::1",
            "ff02::1",
            "::ffff:127.0.0.1",
            "2001:db8::1",
            "2001:2::1",
            "2002:0808:0808::1",
            "64:ff9b::a00:1",
        ] {
            assert!(
                !is_public_ip(address.parse::<IpAddr>().unwrap()),
                "{address}"
            );
        }
    }

    #[test]
    fn accepts_global_addresses() {
        assert!(is_public_ip("8.8.8.8".parse().unwrap()));
        assert!(is_public_ip("2606:4700:4700::1111".parse().unwrap()));
    }
}
