# SitePaladin product brief

## What it does

SitePaladin is a self-hosted, single-operator website auditor. It crawls a bounded
number of server-rendered HTML pages, checks linked HTTP destinations, and
reports deterministic title, description, canonical, and Open Graph findings.
Saved scans can be reviewed and exported without repeating network requests.

## First milestone

Milestone 1 includes an authenticated scan form, bounded crawl and link checks,
SQLite-backed scan history, progress and cancellation, page-organized reports,
JSON and CSV exports, and browser print/Save as PDF. It is intentionally not an
SEO score or a prediction of search ranking.

The scanner sees only HTML returned by the server. It does not execute
JavaScript, validate in-page fragment targets, or inspect indexing directives
comprehensively. Different subdomains are external. Findings describe what the
scanner observed during a particular scan.

## Operator and safety model

The application binds to localhost by default and has one operator account,
configured through environment variables. User-supplied URLs are untrusted.
Outbound requests must reject non-public addresses and validate every redirect;
operators should additionally restrict outbound network access at deployment.

## Later milestones

1. Automatic PDF generation, after evaluating its runtime dependencies.
2. Operator-configured SMTP delivery with TLS, recipient restrictions, and
   persistent delivery state.
3. Sitemap discovery, fragment-target checks, scheduled comparisons, and
   optional JavaScript-rendered scans.
4. A richer frontend only if measured interaction needs justify it.
