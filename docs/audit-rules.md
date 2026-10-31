# Audit rules

Findings are observations, not search-ranking predictions. Each finding has a
stable rule ID, category, severity, status, evidence, explanation, and
suggested action. Statuses are `pass`, `warning`, `issue`, and `not_checked`.

## Titles

- `title.missing`, `title.empty`, and `title.multiple` report missing, blank,
  or repeated title elements.
- `title.length` uses a configurable review range of 30–60 Unicode characters
  after whitespace normalization. This is guidance, not a search-engine rule;
  displayed result width varies and length alone does not determine quality.
- `title.duplicate` reports exact normalized duplicates only among scanned
  pages.

## Descriptions

- `description.missing`, `description.empty`, and `description.multiple`
  report missing, blank, or repeated description tags.
- `description.length` uses a configurable review range of 70–160 Unicode
  characters after whitespace normalization. It is not a ranking rule.
- `description.duplicate` applies only to scanned pages.

## Canonical and Open Graph metadata

Canonical checks report absence as a warning, empty or invalid values,
duplicates, relative values for review, and whether the resolved URL is
self-referencing. Cross-page and cross-domain targets are not inherently
incorrect. HTML-only discovery does not inspect HTTP `Link` headers.

The `og:title`, `og:description`, `og:url`, and `og:image` checks report missing
or empty values as social-sharing warnings. Multiple Open Graph images are
valid. Metadata URLs are treated as untrusted destinations and are never
embedded as third-party resources in reports.

## Links and limits

Link occurrences retain source page, raw href, resolved URL, classification,
and checked health separately. A working redirect is informational; 404 and
410 are confirmed broken; authentication restrictions, throttling, server
errors, connection failures, TLS failures, timeouts, redirect failures,
skips, and inconclusive results remain distinct outcomes. Fragments are
recorded but target existence is not checked.

If a scan is partial, cancelled, truncated, blocked, or interrupted, that state
is visible in its report. Duplicate analysis never claims to cover pages that
were not scanned.
