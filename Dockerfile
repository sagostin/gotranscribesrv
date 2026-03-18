# ============================================================
# Go Backend — Multi-stage build
# ============================================================
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Cache dependency downloads
COPY go.mod go.sum ./
RUN go mod download

# Build — target linux (Docker container OS), detect arch at build time
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/server cmd/server/main.go

# ── Runtime ─────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /bin/server .

EXPOSE 3000

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
  CMD wget -qO- http://localhost:3000/health || exit 1

ENTRYPOINT ["./server"]
