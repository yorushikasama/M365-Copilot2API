FROM golang:1.23-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Stamp the release the same way the release workflow does. Without these a
# container build reports the embedded VERSION file only, and the commit and
# build time stay "unknown" in /api/version.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w \
      -X m365-copilot2api/internal/web.Version=${VERSION} \
      -X m365-copilot2api/internal/web.Commit=${COMMIT} \
      -X m365-copilot2api/internal/web.BuildTime=${BUILD_TIME}" \
    -o /out/m365-copilot2api ./cmd/server

FROM alpine:3.20
RUN addgroup -S m365 && adduser -S -G m365 m365 \
    && mkdir -p /data /app
WORKDIR /app
# The web UI is compiled into the binary (//go:embed all:web in
# internal/web/security_http.go), so no asset directory is copied here. The
# previous COPY of /src/web shipped a second, unread copy of every asset.
COPY --from=build /out/m365-copilot2api /app/m365-copilot2api
RUN chown -R m365:m365 /app /data
USER m365
EXPOSE 4141
ENV M365_LISTEN=0.0.0.0:4141 \
    M365_DATA_DIR=/data \
    M365_CONFIG=/data/accounts.json \
    M365_TOKEN_CACHE=/data/token-cache.json \
    M365_SESSION_CACHE=/data/sessions.json \
    M365_API_KEYS=/data/api-keys.json \
    M365_ADMIN_PASSWORD_FILE=/data/admin-password \
    M365_ADMIN_PASSWORD_BOOTSTRAP_FILE=/run/secrets/m365_admin_password
VOLUME ["/data"]
ENTRYPOINT ["/app/m365-copilot2api"]
