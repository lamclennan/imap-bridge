# ---------- Build Stage ----------
FROM golang:1.21-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO required for go-sqlite3. Let Docker resolve GOARCH from the build platform.
RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o bridge main.go

# ---------- Runtime Stage ----------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates sqlite && \
    adduser -D -h /app appuser

WORKDIR /app

COPY --from=builder /app/bridge .

# data/          — SQLite state db (persistent volume)
# config.json    — mounted at runtime via docker-compose volume
# keys/          — OAuth2 client_secret files (one per Gmail project), read-only
# tokens/        — OAuth2 token cache (one file per account), writable
RUN mkdir -p /app/data /app/keys /app/tokens && chown -R appuser:appuser /app

USER appuser

VOLUME ["/app/data"]

STOPSIGNAL SIGTERM

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s \
  CMD pgrep bridge || exit 1

CMD ["./bridge"]
