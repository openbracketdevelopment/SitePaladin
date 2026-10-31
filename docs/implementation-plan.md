# Implementation plan

## Milestone 1 — bounded website auditor

- [x] Establish the product, architecture, and deterministic rule boundaries.
- [ ] Add operator configuration, login, sessions, CSRF, and login throttling.
- [ ] Add versioned SQLite persistence and restart recovery.
- [ ] Implement a bounded, SSRF-resistant HTTP crawler and link checker.
- [ ] Add deterministic metadata rules and page-level reports.
- [ ] Add authenticated scan, progress, cancellation, and history UI.
- [ ] Add JSON/CSV exports and print-friendly browser PDF workflow.
- [ ] Add deterministic local fixtures and tests.
- [ ] Add Docker/Compose and self-hosting documentation.
- [ ] Run formatting, Clippy, tests, and manually verify UI where available.

The boxes above are a delivery checklist; completion is recorded only after the
implementation and validation actually exist.

## Milestone 2 — automatic PDF

Evaluate Rust PDF libraries against the browser print output, then document
native/browser dependencies and limitations before choosing a renderer.

## Milestone 3 — email delivery

Use a maintained Rust SMTP library, operator-managed TLS configuration,
recipient restrictions, sending limits, and persistent delivery status. Never
operate as a public relay.

## Milestone 4 — crawl coverage

Consider sitemap discovery, anchor-target validation, scheduling and scan
comparisons, and optional JavaScript-rendered scanning.

## Milestone 5 — richer frontend

Consider a richer Rust-oriented frontend only if real interaction requirements
justify the additional complexity.
