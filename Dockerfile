FROM golang:1.26.10-bookworm@sha256:23ce71337b726608afb1bc00986e630a05034a87a87ea1fc2cd1ae5cf33387eb AS build
RUN apt-get update && apt-get install -y --no-install-recommends libvips-dev pkg-config && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/bytedown ./cmd/bytedown
RUN mkdir -p /out/licenses/go \
    && cp /usr/local/go/LICENSE /usr/local/go/PATENTS /out/licenses/go/ \
    && go list -m -f '{{.Dir}}' all | while IFS= read -r dir; do \
      if [ "$dir" != /src ] && [ -n "$dir" ]; then \
        dest="/out/licenses/$(basename "$dir")"; mkdir -p "$dest"; \
        for file in "$dir"/LICENSE* "$dir"/NOTICE* "$dir"/PATENTS*; do \
          if [ -f "$file" ]; then cp "$file" "$dest/"; fi; \
        done; \
      fi; \
    done

FROM debian:bookworm-slim@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3
RUN apt-get update && apt-get install -y --no-install-recommends libvips42 ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 bytedown \
    && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin bytedown \
    && install -d -o 10001 -g 10001 -m 0700 /data
COPY --from=build /out/bytedown /usr/local/bin/bytedown
COPY LICENSE /usr/share/doc/bytedown/LICENSE
COPY --from=build /out/licenses /usr/share/doc/bytedown/third-party
ENV STORAGE_DIR=/data LISTEN=:8080
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 CMD ["bytedown", "health", "api"]
ENTRYPOINT ["bytedown"]
CMD ["api"]
