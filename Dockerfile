# ---------- Build Stage ----------
FROM golang:1.21-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /app
COPY . .

RUN go mod tidy && \
    CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o bridge main.go


# ---------- Runtime Stage ----------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates sqlite && \
    adduser -D -h /app appuser

WORKDIR /app

COPY --from=builder /app/bridge .

# Create data dir with correct permissions
RUN mkdir -p /app/data && chown -R appuser:appuser /app

USER appuser

VOLUME ["/app/data"]

# Graceful shutdown support
STOPSIGNAL SIGTERM

# Optional healthcheck (basic process check)
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s \
  CMD pgrep bridge || exit 1

CMD ["./bridge"]
