# HTTP API

All management endpoints require `Authorization: ****** key>`.
Errors have `{"error":{"code":"...","message":"..."}}`; infrastructure errors
are sanitized. JSON property names use snake_case. See `openapi.yaml` for the
machine-readable contract.

| Method | Path | Behavior |
|---|---|---|
| GET | /health/live | process liveness |
| GET | /health/ready | database, storage, and processing readiness; generic failure |
| GET | /v1/capabilities | versions, inputs, available/configured formats, profiles, limits |
| POST | /v1/images | multipart `file`, `profile`; 202 accepted |
| GET | /v1/jobs/{job_id} | state and generated/planned counts, sanitized failure |
| GET | /v1/images/{image_id} | metadata, original URL, complete manifest |
| GET | /v1/images | cursor pagination, optional status filter |
| GET | /v1/images/{image_id}/original | authenticated original bytes |
| GET | /media/{asset_id} | completed immutable variant; authenticated by default |
| DELETE | /v1/images/{image_id} | remove visibility and fence workers; 204 |

POST accepts an optional Idempotency-Key. The same key with the same content
hash and resolved profile returns the existing IDs; conflicting reuse returns
409. Database uniqueness serializes concurrent submissions. Without a key,
identical content produces separate images. Keys are retained until image
deletion. Uploads do not perform all variant encoding synchronously.

Job states: queued, processing, succeeded, failed. Progress is an output count,
not an invented percentage. The manifest appears only after every required
output succeeds. It reports source/output sizes and dimensions, format, duration,
percentage size difference, whether smaller, and whether resizing contributed.
Dimensions describe orientation-corrected display dimensions; an original's
encoded raster can have swapped axes when EXIF orientation rotates it.
Percentage difference is `(output_bytes - original_bytes) / original_bytes * 100`;
negative means smaller. Per-output durations include resizing, encoding, and
writing; job processing time also includes source normalization. Durations are
measured milliseconds, not estimates.

HTTP errors include 400 malformed input, 401 missing/invalid credentials,
404 absent resource, 409 idempotency conflict, 413 upload limit, 422 invalid or
unsupported image/profile, and 503 infrastructure unavailable.

Deletion is immediately authoritative in PostgreSQL. Active children may finish
their isolated attempt, but cannot publish. Files are reclaimed immediately when
possible and by periodic reconciliation after crashes. This is not a secure
erase guarantee; backups and public caches may retain bytes.
An already-open download may finish after deletion.
