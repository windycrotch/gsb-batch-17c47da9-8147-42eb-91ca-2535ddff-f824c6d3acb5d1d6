# syntax=docker/dockerfile:1

# ============================================================================
# Build stage - compiles the Go binary
# ============================================================================
FROM golang:1.23-alpine AS builder

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git ca-certificates

# Copy go mod files first for better layer caching
COPY go.mod ./
RUN go mod download

# Copy source code
COPY *.go ./

# Build the binary
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s -X main.Version=${VERSION}" \
    -o yt-dlp-restream .

# ============================================================================
# Development stage - includes shell and debugging tools
# ============================================================================
FROM alpine:3.20 AS development

WORKDIR /app

# Install runtime dependencies + dev tools
RUN apk add --no-cache \
    ca-certificates \
    ffmpeg \
    python3 \
    py3-pip \
    curl \
    bash \
    && pip3 install --break-system-packages yt-dlp

# Copy binary from builder
COPY --from=builder /build/yt-dlp-restream /app/yt-dlp-restream

# Non-root user
RUN adduser -D -u 1000 appuser
USER appuser

ENV PORT=8080
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:${PORT}/health || exit 1

ENTRYPOINT ["/app/yt-dlp-restream"]

# ============================================================================
# Production stage - minimal image
# ============================================================================
FROM alpine:3.20 AS production

WORKDIR /app

# Install only runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    ffmpeg \
    python3 \
    py3-pip \
    && pip3 install --break-system-packages yt-dlp \
    && rm -rf /root/.cache

# Copy binary from builder
COPY --from=builder /build/yt-dlp-restream /app/yt-dlp-restream

# Non-root user for security
RUN adduser -D -u 1000 appuser
USER appuser

ENV PORT=8080
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q --spider http://localhost:${PORT}/health || exit 1

ENTRYPOINT ["/app/yt-dlp-restream"]

# ============================================================================
# Default target is production
# ============================================================================
FROM production
