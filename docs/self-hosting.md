# Self-hosting SitePaladin

## Requirements and local setup

Use the stable Rust toolchain declared in `rust-toolchain.toml` and SQLite
support available through SQLx. Create configuration:

```sh
cp .env.example .env
chmod 600 .env
cargo run -- hash-password
# Put the printed Argon2 string in OPERATOR_PASSWORD_HASH in .env.
# Non-interactive secret-manager input can use: printf '%s\n' "$PASSWORD" | cargo run -- hash-password --stdin
openssl rand -hex 32
# Put the 64-character result in SESSION_SECRET in .env.
cargo run
```

SitePaladin listens on `127.0.0.1:3000` by default, creates `data/` and applies
versioned SQLite migrations on startup. The database is never deleted on
startup. The session cookie is HttpOnly, SameSite=Lax, and signed using the
configured session secret. Set `COOKIE_SECURE=true` when HTTPS terminates in a
trusted reverse proxy. Do not expose the app directly to an untrusted network.

The initial operator is configured with `OPERATOR_USERNAME` and
`OPERATOR_PASSWORD_HASH`; there is no default password or registration flow.
Keep `.env` and database backups private.

## Local fixture demo

The fixture exception is available only in the opt-in `dev-fixtures` Cargo
feature and only for the exact `127.0.0.1:4173` destination. It is not a
general private-network bypass.

Terminal 1:

```sh
cargo run --features dev-fixtures --bin fixture-site
```

Terminal 2: set `ALLOW_LOCAL_FIXTURE=true` in the local `.env`, then run:

```sh
cargo run --features dev-fixtures
```

Sign in and scan `http://127.0.0.1:4173/`. The fixture includes valid and
problematic metadata, duplicate titles and descriptions, empty and multiple
metadata, relative URLs and a base URL, cross-page canonicals, a missing link,
redirects and a loop, restricted and rate-limited responses, a slow response,
robots exclusions, query variants, oversized content, and a non-HTML response.
Stop both processes when finished.

## Tests and checks

```sh
cargo fmt --check
cargo clippy --all-targets --all-features -- -D warnings
cargo test
```

The fixture is intended for an operator-run demo. Automated tests use local
logic and do not require access to public websites.

## Docker Compose

Build and start with:

```sh
cp .env.example .env
# Configure OPERATOR_PASSWORD_HASH and SESSION_SECRET as above.
docker compose up --build -d
docker compose logs -f sitepaladin
```

SQLite data persists in the `sitepaladin-data` named volume. The container
binds its application port to localhost. Back up the database while the app is
stopped, or use SQLite's online backup API for a live copy:

```sh
docker compose stop sitepaladin
docker run --rm -v sitepaladin-data:/data -v "$PWD:/backup" alpine \
  cp /data/sitepaladin.db /backup/sitepaladin.db
docker compose start sitepaladin
```

Restore by stopping the service, replacing the database in the volume with a
known-good backup, and starting the service again. Keep a separate encrypted,
off-host backup.

## Operations, limits, and upgrades

The app validates environment values at startup. Default limits are 25 pages,
four as the maximum global request concurrency (the first implementation
currently schedules requests serially), two per origin, 250 ms minimum per
origin delay, 15-second request timeout, 10-minute maximum scan duration,
2 MB response size, 200 unique link destinations, 10,000 saved link
occurrences, three query variants per path, 256 cached robots origins, and
five redirects.
Operators can lower or raise limits within bounded ranges. The serial worker
is intentionally conservative; configured concurrency is not a throughput
guarantee.

Logs are structured with `tracing`; set `RUST_LOG=sitepaladin=debug` for
diagnostic detail. Never include credentials in a scanned URL. Network failures
in robots policy fail closed for that origin; a missing robots file is allowed.
External links are not recursively crawled. Results do not validate JavaScript
rendered content, fragment targets, or HTTP `Link` canonical headers.

Database migrations run during startup and are forward-only. Stop and back up
the database before an upgrade, deploy the new version, and inspect startup
logs for migration errors. On restart, queued work remains queued and is
claimed by the in-process worker; previously running work becomes `interrupted`
with partial results preserved.

## Reverse proxy and network controls

Keep the application bound to localhost. Place it behind a maintained TLS
reverse proxy, configure that proxy to forward only trusted requests, set
`COOKIE_SECURE=true`, and do not expose the backend listener publicly. Use
proxy request limits and authentication monitoring as additional controls.
SitePaladin does not trust forwarded headers for client identity.

Restrict outbound network access from the process/container to public HTTP and
HTTPS destinations and the configured allowed ports. Application-level DNS
validation and address pinning are defense in depth, not a substitute for
network egress controls. The app rejects non-public addresses and validates
each redirect destination before connecting.

## Troubleshooting

- **Startup asks for secrets:** set a valid Argon2 hash and at least 64
  characters of random `SESSION_SECRET`; there is deliberately no default
  password.
- **Database path errors:** ensure the database's parent directory is writable
  by the runtime user.
- **A page is skipped:** inspect its robots rules, port, DNS resolution, and
  configured size/time limits. Robots access failures are conservatively
  denied; HTTP 404 for robots.txt permits crawling.
- **No scan progress:** check application logs and SQLite write permissions.
  A process restart interrupts an active scan; partial pages remain available.
- **Fixture URLs are rejected:** build both commands with `--features
  dev-fixtures` and set `ALLOW_LOCAL_FIXTURE=true` only in the local demo
  environment.
