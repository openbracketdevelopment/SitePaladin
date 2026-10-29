# First-release implementation plan

1. Establish product boundaries, architecture, HTTP contract, and this plan.
2. Implement validated configuration and named profiles; generated storage IDs;
   bounded static JPEG/PNG validation and govips processing; codec probes.
3. Add versioned PostgreSQL migrations, idempotent acceptance, atomic claims,
   fenced leases/heartbeats, bounded retries, complete publication and deletion.
4. Add authenticated HTTP handlers, controlled downloads, readiness, pagination,
   and consistent errors.
5. Package one executable into a pinned, non-root container; Compose runs
   migration, API, worker, PostgreSQL, and persistent volumes.
6. Add generated-fixture tests, PostgreSQL integration checks, CI, bounded demo,
   server-side JavaScript and scoped Next.js examples.
7. Exercise deployment and representative outputs; run available checks and
   security scans. Record actual results and remaining limitations honestly.

Each implementation step must preserve the architecture's invariants. No
dashboard, remote imports, automatic codecs, or unused future modules are added.
