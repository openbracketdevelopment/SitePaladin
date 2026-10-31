# SitePaladin

SitePaladin is a self-hosted website auditor that checks links and common SEO
metadata on a bounded set of pages, then presents saved findings in a
page-organized report. It checks server-returned HTML only; it does not predict
search rankings.

## Run locally

Install the stable Rust toolchain (see `rust-toolchain.toml`).

If you installed Rust with rustup and your terminal says `cargo: command not
found`, run `source "$HOME/.cargo/env"` to load Cargo into your current shell.

```sh
cp .env.example .env
chmod 600 .env
cargo run -- hash-password
# Enter the password you want to use when signing in.
# Copy the printed hash into .env: OPERATOR_PASSWORD_HASH='...'
openssl rand -hex 32
# Copy the printed secret into .env: SESSION_SECRET=...
cargo run
```

Keep the password hash in single quotes so its `$` characters remain literal.
If `.env` already exists, edit it rather than copying over it. Open
http://127.0.0.1:3000 and sign in as `operator` with the password you chose.

By default the app listens only on `127.0.0.1:3000` and stores its SQLite
database under `./data`. Configure an Argon2 password hash and a random session
secret before use; there is no default password. See [self-hosting](docs/self-hosting.md)
for hash generation, Docker, the fixture demo, tests, backups, upgrades, and
reverse-proxy guidance.

## Development

```sh
cargo fmt --check
cargo clippy --all-targets --all-features -- -D warnings
cargo test
```

See the [product brief](docs/product-brief.md), [architecture](docs/architecture.md),
and [audit rules](docs/audit-rules.md). See the [third-party license notes](docs/third-party-licenses.md);
the original project code is MIT licensed.
