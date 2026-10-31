# Architecture

## Runtime shape

SitePaladin is a single Rust/Axum process. Axum serves Askama-rendered HTML,
plain CSS, and a small locally served JavaScript file. Tokio runs the web
server and bounded scan worker tasks. Reqwest performs outbound requests,
Scraper parses server-returned HTML, and `url` resolves and normalizes links.
SQLx and SQLite persist jobs, pages, links, and findings. The code separates
HTTP handling, crawl policy, audit rules, persistence, and report encoding so
each can be tested without public network access.

## Data flow

1. An authenticated operator submits an HTTP(S) seed URL and a page limit.
2. Input is validated and a queued scan with a configuration snapshot is
   committed to SQLite.
3. A single in-process worker claims the job and discovers pages breadth-first
   within the effective seed origin. Request and body limits apply to every
   fetch; redirects are checked one hop at a time.
4. Page metadata, link occurrences, and deterministic findings are persisted
   in short transactions. Cancellation is stored and checked between requests.
5. Authenticated report and export handlers read saved rows; they never rescan.

On startup, scans left in `running` become `interrupted`. Queued jobs remain
visible and partial results are preserved; the application does not claim that
an in-memory task resumes after restart.

## Security boundaries

The login uses a configured Argon2 password hash and server-side sessions.
State-changing requests require a session CSRF token. Cookie security follows
the configured HTTPS deployment mode. Routes containing scan data are behind
the authentication boundary.

The crawler must resolve each hostname, reject any non-public result (including
IPv4-mapped IPv6), and pin connections to the validated addresses. It disables
environment proxies and automatic redirects. Each redirect target is parsed,
resolved, port-checked, and validated again before connecting. A test-only
exception may permit an exact local fixture address; it is not an operator
setting in normal deployments.

## Persistence

Versioned SQLx migrations create SQLite tables with foreign keys and indexes.
WAL and a busy timeout are configured at connection setup. Network operations
never run inside database transactions.

## Explicit non-goals

No separate queue service, microservices, JavaScript execution, accounts,
multitenancy, automatic PDF renderer, email, scheduling, or SEO scoring are
part of the first milestone.
