FROM rust:1-bookworm AS builder
WORKDIR /app
COPY Cargo.toml Cargo.lock ./
COPY migrations ./migrations
COPY src ./src
COPY templates ./templates
COPY static ./static
RUN cargo build --locked --release --bin sitepaladin

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install --no-install-recommends -y curl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --create-home sitepaladin
WORKDIR /app
COPY --from=builder /app/target/release/sitepaladin /usr/local/bin/sitepaladin
COPY --from=builder /app/migrations /app/migrations
RUN mkdir -p /data && chown sitepaladin:sitepaladin /data
USER sitepaladin
ENV HOST=0.0.0.0 PORT=3000 DATABASE_URL=sqlite:///data/sitepaladin.db
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD ["sh", "-c", "curl -fsS http://127.0.0.1:3000/healthz || exit 1"]
ENTRYPOINT ["/usr/local/bin/sitepaladin"]
