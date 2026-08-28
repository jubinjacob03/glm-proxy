# syntax=docker/dockerfile:1

# ============================================================================
# Stage 1 — build both binaries
# ============================================================================
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first, so a source-only edit does not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO stays off: modernc.org/sqlite is pure Go, so the binaries are static.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/zai-api . \
 && go build -trimpath -ldflags="-s -w" -o /out/token-collector ./cmd/token-collector

# ============================================================================
# Stage 2 — Playwright browser + OS dependencies
# ============================================================================
# The token collector drives a headless Chromium to harvest device tokens, and
# the proxy re-runs it automatically whenever the store runs low. So the runtime
# image needs a real browser, not just the binaries.
FROM golang:1.26-bookworm AS runtime

ENV DEBIAN_FRONTEND=noninteractive \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata curl \
 && rm -rf /var/lib/apt/lists/*

# Keep this pinned to the same playwright-go version as go.mod.
ARG PLAYWRIGHT_GO_VERSION=v0.6201.1
RUN set -eu; \
    for attempt in 1 2 3 4 5; do \
      if go run github.com/mxschmitt/playwright-go/cmd/playwright@${PLAYWRIGHT_GO_VERSION} install --with-deps chromium; then \
        break; \
      fi; \
      if [ "$attempt" = 5 ]; then \
        echo "playwright install failed after $attempt attempts" >&2; \
        exit 1; \
      fi; \
      echo "playwright install attempt $attempt failed; retrying in 10s..." >&2; \
      sleep 10; \
    done; \
    rm -rf /root/.cache/go-build /root/go/pkg/mod

WORKDIR /app

COPY --from=build /out/zai-api         /app/zai-api
COPY --from=build /out/token-collector /app/token-collector
RUN chmod +x /app/zai-api /app/token-collector

# Baked in at build time, as requested. NOTE: this puts your ZAI_TOKEN and
# AUTH_TOKEN into an image layer — anyone who can pull or export the image can
# read them. Keep the image private, or drop this COPY and pass the values with
# `env_file:` / `-e` at run time instead.
COPY .env /app/.env

# No entrypoint script: the proxy loads /app/.env itself, creates the token
# store if it is missing, and its monitor collects the first batch in the
# background — so `docker compose up -d` is all that is needed. tokens.sqlite
# and logs live on the /data volume so they survive a container replacement.
VOLUME ["/data"]
ENV LOG_DIR=/data/logs \
    HOST=0.0.0.0 \
    PORT=3007

EXPOSE 3007

HEALTHCHECK --interval=30s --timeout=5s --start-period=300s --retries=5 \
    CMD curl -fsS "http://127.0.0.1:${PORT}/health" >/dev/null || exit 1

CMD ["/app/zai-api", "--db-path", "/data/tokens.sqlite"]
