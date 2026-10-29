# ByteDown

**Smaller files. More possibilities.**

ByteDown is a self-hosted Go service for one operator and trusted server-side
applications. Upload a static JPEG or PNG; keep its original bytes; poll a durable
PostgreSQL job for an optimized original-format copy and responsive WebP/AVIF
variants. No accounts, dashboard, billing, remote URL imports, or automatic codec
installation. This is an initial release, not a production-readiness guarantee.

## Run with Docker Compose

Requirements: Linux Docker Engine with Compose v2, about 2 GiB available memory
for the default services, and disk sized for originals, variants, staging and
backups. CPU-heavy AVIF encoding can take seconds or longer.

```sh
cp .env.example .env
chmod 600 .env
openssl rand -hex 32  # copy into POSTGRES_PASSWORD
openssl rand -hex 32  # independently copy into API_KEY
# Edit .env: replace both placeholders. Do not commit .env.
docker compose config --quiet
docker compose up --build -d
docker compose ps -a
docker compose logs --tail=50 migrate api worker
curl --fail http://127.0.0.1:8080/health/ready
```

`.env` is read by Compose; it is **not** automatically loaded by a local Go
process or the example clients. Export the same `API_KEY` into the shell running
clients, without copying it into browser code or shell history. Use hex for the
PostgreSQL password because Compose interpolates it into `DATABASE_URL`; other
characters require URL percent-encoding. An existing database volume keeps its
original credentials: changing `.env` alone does not rotate a database password.

Compose runs a one-shot root `storage-init` to give the image volume to UID/GID
10001; `migrate` waits for it and healthy PostgreSQL; API and worker wait for
successful migration. Application containers run unprivileged with dropped
capabilities, read-only root filesystems and bounded memory/CPU/PIDs. Only API is
published, on **127.0.0.1:8080**. PostgreSQL has no host port. Named volumes
`bytedown_images` and `bytedown_postgres` persist across container replacement.
Keep the Compose project name fixed; changing it creates different volumes.

```sh
docker compose run --rm --no-deps api probe
mkdir -p demo-output
docker compose run --rm --no-deps --entrypoint sh api -c \
  'bytedown probe; bytedown version; dpkg-query -W libvips42 libheif1 libaom3 libwebp7' \
  > demo-output/runtime-info.txt
python3 examples/demo.py --benchmark --runtime-info demo-output/runtime-info.txt --delete
# Or use a real image:
python3 examples/demo.py path/to/photo.jpg --benchmark
sh examples/curl.sh path/to/photo.jpg
node examples/server-upload.mjs path/to/photo.jpg
```

The Python example uses only the standard library, generates a PNG when no sample
is supplied, bounds polling/downloads, verifies immutable original bytes and each
variant's byte count, magic and encoded dimensions. Results and downloads go to
`demo-output/`. `--delete` deletes the server image, not those local copies.
The benchmark reports measured client timings, server processing time, sizes,
capabilities and client platform, not invented speedups or visual-quality scores.
Set `BENCHMARK_ENV` to record host CPU/RAM/container limits and record native
versions with the commands below. A synthetic PNG is not representative of a
photo workload. Compare the same images, profiles, versions and hardware.

## HTTP contract and integrations

See [OpenAPI](docs/openapi.yaml), [HTTP details](docs/api.md),
[architecture](docs/architecture.md), [product boundaries](docs/product-brief.md)
and [implementation plan](docs/implementation-plan.md).

Send `Authorization` with the `Bearer` scheme and your server API key on **all**
`/v1` requests. Keys must stay server-side; this is a single-operator shared-key
service, not a multi-tenant isolation boundary. Multipart POST `/v1/images` has
one `file` and optional `profile` (default `web`):

```json
{"image_id":"…","job_id":"…","status_url":"/v1/jobs/…"}
```

Poll `status_url` until `succeeded` or `failed`, with a deadline. States are
`queued`, `processing`, `succeeded`, `failed`. `generated`/`planned` are output
counts, not a fabricated percentage; native work need not produce smooth
progress. GET `/v1/images/{id}` returns metadata, authenticated `original_url`
and complete `outputs` only after successful publication. Every output reports
format, dimensions, bytes, per-output processing duration, URL, whether smaller, signed
`percent_size_difference` and `includes_resizing`. Negative size difference means
smaller. Resized savings include resizing; outputs may be larger than originals.
GET `/v1/images?limit=20&status=succeeded` returns `{images,next_cursor}`; pass
`next_cursor` back as `cursor`. DELETE `/v1/images/{id}` returns 204.

POST supports an `Idempotency-Key` up to 200 bytes. Same key/content/resolved
profile returns the same IDs; conflicting reuse returns 409. Keys remain until
image deletion. Without a key, identical content is a separate image.
Errors are `{"error":{"code":"…","message":"…"}}`; expect 400, 401, 404, 409,
413, 422 and 503. Retry transient infrastructure/capacity failures with bounded
backoff and idempotency; do not retry permanent invalid inputs indefinitely.
The API has a fixed 60-second total request-read deadline, including the upload
body (and a 5-second header deadline). The byte ceiling is not a bandwidth or
completion guarantee: slow uploads may time out before reaching it. Reverse
proxies can impose additional limits.

`examples/server-upload.mjs` is a Node.js 22+ server-only upload/poll example.
`examples/nextjs-picture.tsx` is a scoped App Router **server component**, not a
complete app: copy it into your project and set `BYTEDOWN_INTERNAL_URL`,
`BYTEDOWN_PUBLIC_URL` and server-only `API_KEY`. Explicitly set
`PUBLIC_VARIANTS=true` on ByteDown before using its generated `/media/` URLs in
a browser. Never prefix the key with `NEXT_PUBLIC_`. The snippet uses plain
`picture`/`source`/`img`, responsive `srcset`/`sizes`, intrinsic dimensions, and an
optimized original-format fallback. It never uses the authenticated original as
a browser fallback and avoids Next Image double optimization.
After your trusted server-side multipart upload returns `job_id`, call the
snippet's exported `waitForByteDownJob(job_id)` helper (120-second bounded
authenticated polling, no response caching), then render
`<ByteDownPicture id={completed.image_id} alt="Description" />` using its result.
Do not expose that helper as an unauthenticated server action. The standalone
`server-upload.mjs` demonstrates the complete executable upload/poll workflow;
the TSX is a scoped integration example, not a tested full Next.js application.

Variants are private by default. `PUBLIC_VARIANTS=true` makes every published
variant available to anyone who knows its URL. Opaque IDs are not access control.
Originals always require authentication. Public caches may outlive deletion.
CORS is off by default; allow only explicit trusted origins. CORS is not
authentication, and enabling it is not permission to expose the server key.

### Conceptual Craft/Payload adapter flow

Craft/Payload integrations are future work, not included plugins or supported
adapters. A prospective server-side adapter would:

1. Accept an editor's JPEG/PNG through the CMS's own authenticated upload flow.
2. Send that file from the CMS backend to ByteDown with a named profile and an
   idempotency key; keep ByteDown credentials exclusively on the backend.
3. Store the returned image/job IDs alongside the CMS asset's processing state.
4. Poll the job with a bounded background task; surface failure/retry state to
   editors, and fetch the complete manifest only after success.
5. Store variant metadata and render responsive `picture` markup with an
   optimized original-format fallback. Explicitly enable public variants only
   when those assets are meant to be public; never expose authenticated originals.
6. On replacement/deletion, update CMS references and call ByteDown deletion
   according to the site's retention policy, accounting for caches and backups.

Future milestones are operational hardening and load evaluation, reviewed
Craft/Payload integrations, optional completion webhooks, S3-compatible storage,
and deliberately reviewed additional image formats. These are not implemented
capabilities or delivery commitments. PDF/OCR/archive processing would require
separate modules rather than extensions to the image codec path.

## Configuration

The executable reads environment variables. Docker defaults are in
`compose.yaml`; direct execution additionally needs `DATABASE_URL` and `API_KEY`.

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | required | PostgreSQL URL; enable TLS for remote databases |
| `API_KEY` | required | At least 32 characters, no surrounding whitespace |
| `STORAGE_DIR` | `./data` (`/data` in Docker) | Private writable filesystem on one host |
| `LISTEN` | `:8080` | API host:port; health check uses the configured local port |
| `PUBLIC_VARIANTS` | `false` | Anonymous access to generated variants only |
| `CORS_ORIGINS` | empty | Comma-separated explicit HTTP(S) origins, no paths/wildcards |
| `WORKER_CONCURRENCY` | `1` | Slots per worker process; replicas multiply slots |
| `MAX_UPLOAD_BYTES` | `20971520` | Accepted file byte ceiling |
| `MAX_PIXELS` | `40000000` | Decoded pixel ceiling |
| `MAX_DIMENSION` | `20000` | Maximum input width/height |
| `MAX_OUTPUT_COUNT` | `17` | Output ceiling; default web plan produces at most 11 |
| `MAX_OUTPUT_BYTES` | `209715200` | Aggregate encoded-output publication ceiling |
| `PROCESSING_SECONDS` | `120` | Native child processing deadline; configurable up to 86400 seconds |
| `PROFILES_JSON` | built-in `web` | JSON object replacing all named profiles |

Default `web`: widths 480/960/1440/1920 plus source width, no crop or upscale,
WebP and AVIF, plus one full-size optimized copy in the original format.
JPEG quality 85, WebP quality 80/effort 4, AVIF quality 50/effort 4.
PNG optimization is lossless, not palette quantization. Orientation is applied,
ICC profiles converted to sRGB, unnecessary metadata stripped; alpha preserved
where supported. Unprofiled CMYK and unsupported color configurations fail
instead of being guessed. Animated images, SVG, GIF and other inputs are rejected.
Custom profiles may request `jpeg`, `png`, `webp` or `avif`; explicitly requesting
JPEG for a transparent source fails rather than silently flattening its alpha.

Example complete replacement (all settings must be supplied):

```json
{"web":{"widths":[480,960],"formats":["webp","avif"],"include_source":true,"jpeg_quality":85,"webp_quality":80,"webp_effort":4,"avif_quality":50,"avif_effort":4}}
```

For Compose, put this JSON in a protected local `compose.override.yaml`, under
**both** `services.api.environment.PROFILES_JSON` and
`services.worker.environment.PROFILES_JSON`, as a quoted YAML string. Do not
expect an unreferenced `.env` variable to be automatically passed to containers.
Accepted jobs snapshot their resolved profile and pipeline version; changed
profiles affect newly accepted jobs. Resource limits are not snapshotted: workers
use current limits for active/retried work too. Operators must keep limits
compatible with queued profiles and in-flight work during configuration changes.
GET `/v1/capabilities` shows effective settings.

Container defaults: API/worker 1 GiB memory, 2 CPUs and 128 PIDs each; PostgreSQL
512 MiB/1 CPU/128 PIDs. Native children use two libvips threads. Tune concurrency
and limits with actual measurements. Encoded buffers can exist before byte
checks; these byte ceilings are **not native allocation limits**. Container
resource limits remain necessary. Horizontal workers need the same PostgreSQL
and a coherent shared filesystem; multi-host/network-filesystem deployment has
not been qualified.
Native deadlines may be followed by up to two seconds of subprocess-pipe cleanup
waiting; they are not an exact end-to-end HTTP/job latency guarantee.

## TLS, health and operations

Keep the API on loopback and put a maintained TLS reverse proxy in front.
Terminate HTTPS with a valid certificate; forward only needed routes, disable
directory listing, limit upload/request sizes (allow multipart overhead), set
timeouts above accepted processing deadlines, and apply rate limits. Never expose
PostgreSQL, storage directories, or the container socket publicly. Do not log
Authorization headers, image bytes, secrets or sensitive metadata. Use TLS on
any non-local database connection; `sslmode=disable` is only the private Compose
network default. Protect Docker host access and backups as sensitive data.

`/health/live` means the API process responds; `/health/ready` checks PostgreSQL,
writable storage and required codec smoke tests, not queue latency or capacity.
Codec probe results are cached for up to 30 seconds; database/storage checks run
on each request. `bytedown health api` checks API readiness using `LISTEN`;
`bytedown health worker` checks worker dependencies and codecs without an HTTP
listener. Both exit nonzero on failure.
Check `docker compose ps`, worker logs, failed jobs, disk space and processing
latency as well. A healthy API alone does not prove a worker is running.
Docker does not automatically restart a merely unhealthy container.
The worker's dependency/codec health check is not a heartbeat or a guarantee of
queue progress; monitor durable job age and completions separately.

### Backup and restore

Back up database **and** image volume at the same quiescent point. Stop all writers
(including external worker replicas) and block uploads at the proxy:

```sh
mkdir -p backups
chmod 700 backups
docker compose stop api worker
docker compose exec -T db pg_dump -U bytedown -d bytedown -Fc > backups/database.dump
docker compose run --rm --no-deps --entrypoint tar api \
  -C /data -czf - . > backups/images.tar.gz
docker compose start api worker
```

These are plaintext backups of private content; encrypt, restrict access and
copy off-host. Record the deployed Git revision/image digests and protected
configuration separately. Check command exit statuses and periodically perform
a restore rehearsal; a dump's existence is not proof of recoverability.

Restore into a **fresh separate Compose project/empty volumes** with matching
application and PostgreSQL versions and correct `.env`:

```sh
export COMPOSE_PROJECT_NAME=bytedown-restore  # Must be a new, unused project name.
docker compose up -d --wait --wait-timeout 60 db
docker compose run --rm --no-deps storage-init
docker compose run --rm --no-deps --entrypoint tar api \
  -C /data -xzf - < backups/images.tar.gz
docker compose exec -T db pg_restore -U bytedown -d bytedown \
  --exit-on-error --no-owner --no-acl < backups/database.dump
docker compose up -d
```

Restore only trusted archives. Tar runs as UID10001 into the initialized writable
volume; bind mounts, if substituted, must have matching ownership.
Keep this project name selected while verifying the restored stack; do not point
these restore commands at the original installation's volumes.
Wait for `db` to be healthy before `pg_restore`. Do not start API/worker before
both restores succeed. Verify readiness, image listing, original downloads and
variant downloads; expired in-flight leases should be reclaimed by workers.
Restoring only one volume breaks consistency. Filesystem cleanup is best effort;
deletion is not secure erase, and backups/public caches may still contain bytes.
Workers reconcile unreferenced files at startup and hourly, with a 48-hour age
grace period to avoid reclaiming files belonging to active processing attempts.
This grace delays orphan reclamation; it is not an image-retention policy.

### Upgrade, rollback and development reset

Back up first, stop API/worker, review release/migration notes and native codec
changes, update source and pinned image digests deliberately, then:

```sh
docker compose build
docker compose run --rm migrate
docker compose up -d --force-recreate api worker
docker compose ps -a
```

Migrations are embedded/versioned and serialized; failure blocks startup. Do not
assume schema downgrade or old-binary compatibility. For rollback, restore the
matched database+images backup with the previous image/configuration. PostgreSQL
major upgrades require a planned pg_dump/restore or pg_upgrade, not pointing a
new major image at old data. Patch tags/digests and Debian packages also require
routine security review; pinning does not make an image permanently safe.

For disposable development data only, `docker compose down --volumes` destroys
**all** stored images and database data. A normal `docker compose down` preserves
volumes. Never use volume deletion as an upgrade procedure.

## Local development and verification

The module requires Go **1.25 or newer** and CGO. The image/CI use verified Go
1.26.10, Debian bookworm and PostgreSQL 17.11, with pinned image manifest digests.
This is not a dependency-free static executable. On Debian/Ubuntu:

```sh
sudo apt-get update
sudo apt-get install -y libvips-dev pkg-config
```

On **Ubuntu 24.04 only**, also install the separate libheif AV1 plugins:

```sh
sudo apt-get install -y libheif-plugin-aomenc libheif-plugin-aomdec
```

Then build and run:

```sh
mkdir -p bin
go build -o bin/bytedown ./cmd/bytedown
./bin/bytedown probe
# Export DATABASE_URL, API_KEY, STORAGE_DIR; use a dedicated development DB.
./bin/bytedown migrate
./bin/bytedown api       # another terminal: ./bin/bytedown worker
go test -race ./...
go vet ./...
# Optional integration tests require a disposable PostgreSQL database:
TEST_DATABASE_URL='postgres://localhost/bytedown_test?sslmode=disable' go test -race ./...
```

Tests create their own image fixtures. CI installs libvips and libheif AV1 plugins
on Ubuntu 24.04, runs
race tests/vet against PostgreSQL, validates Compose/examples and builds/probes
the actual Debian runtime. Verify the actual installed native packages:

```sh
docker compose run --rm --no-deps --entrypoint sh api -c \
  'bytedown version; dpkg-query -W libvips42 libheif1 libaom3 libwebp7'
```

The initial container probe was exercised with libvips 8.14.1; codec availability
is tested, not inferred from a library name. Compression, visual quality and
browser support must be evaluated on your workload.

## License

ByteDown uses the existing [MIT license](LICENSE), copyright 2026 Tam.
Third-party software retains its own licenses; see
[third-party licensing notes](docs/third-party-licenses.txt).
