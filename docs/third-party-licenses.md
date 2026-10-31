# Third-party licenses

The MIT license in the repository applies to original SitePaladin project code;
it does not relicense third-party crates. Rust dependencies remain subject to
their own licenses and copyright notices. `Cargo.lock` records the resolved
dependency versions. No third-party source is copied into this repository.

Direct runtime dependencies include Argon2, Askama, Axum, csv, dotenvy,
futures-util, http, httpdate, rand_core, rpassword, robotstxt, Reqwest,
Scraper, Serde, Serde JSON, SQLx, Tokio, Tokio Util, tower-sessions, tracing,
tracing-subscriber, url, uuid, and time. Consult each resolved crate's license
metadata and bundled notices before redistributing binaries.

To review licenses for the complete resolved dependency tree, including
transitive crates, inspect Cargo package metadata or use an approved license
inventory tool in the release pipeline. Keep its generated inventory with
release materials; this short document is not a substitute for those notices.
