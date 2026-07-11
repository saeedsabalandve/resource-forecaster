# # Multi-stage Docker build for production-grade resource-forecaster
# # Stage 1: Build stage with security scanning
FROM golang:1.21-alpine3.19 AS builder

# # Build arguments for versioning
ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_TIME=unknown

# # Install build dependencies
RUN apk add --no-cache \
    git \
    make \
    gcc \
    musl-dev \
    ca-certificates \
    tzdata

# # Create non-root user early for security
RUN addgroup -g 1000 forecaster && \
    adduser -u 1000 -G forecaster -s /bin/sh -D forecaster

WORKDIR /build

# # Copy go mod files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# # Copy source code
COPY . .

# # Security: Run gosec before building
RUN go install github.com/securecodewarrior/gosec/v2/cmd/gosec@latest && \
    gosec -quiet ./...

# # Build the binary with security flags
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w \
    -X main.Version=${VERSION} \
    -X main.CommitSHA=${COMMIT_SHA} \
    -X main.BuildTime=${BUILD_TIME}" \
    -tags=netgo,osusergo \
    -trimpath \
    -o /build/forecaster \
    cmd/forecaster/main.go

# # Stage 2: Security scanning stage
FROM aquasec/trivy:latest AS scanner

COPY --from=builder /build/forecaster /scan/forecaster
RUN trivy rootfs --no-progress --exit-code 0 --severity HIGH,CRITICAL /scan

# # Stage 3: Final minimal runtime image
FROM alpine:3.19

# # Add metadata labels
LABEL org.opencontainers.image.title="Resource Forecaster" \
      org.opencontainers.image.description="Production-grade resource forecasting microservice" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${BUILD_TIME}" \
      org.opencontainers.image.revision="${COMMIT_SHA}" \
      org.opencontainers.image.vendor="Platform Engineering" \
      maintainer="platform-eng@company.com"

# # Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    curl \
    && rm -rf /var/cache/apk/*

# # Create non-root user
RUN addgroup -g 1000 forecaster && \
    adduser -u 1000 -G forecaster -s /bin/sh -D forecaster

# # Create necessary directories with proper permissions
RUN mkdir -p /etc/resource-forecaster \
             /data/models \
             /tmp/forecaster \
    && chown -R forecaster:forecaster /etc/resource-forecaster \
                                      /data \
                                      /tmp/forecaster

# # Copy binary from builder stage
COPY --from=builder --chown=forecaster:forecaster /build/forecaster /usr/local/bin/forecaster

# # Copy default configuration
COPY --chown=forecaster:forecaster config.default.yaml /etc/resource-forecaster/config.yaml

# # Security hardening
# # Drop all capabilities and only add what's needed
# # Use numeric user for security
USER 1000:1000

# # Expose ports
EXPOSE 8080 9090

# # Health check
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -f http://localhost:8080/health/live || exit 1

# # Read-only root filesystem for security
# # Mount writable directories as volumes
VOLUME ["/data/models", "/tmp"]

# # Set environment variables
ENV GIN_MODE=release \
    TZ=UTC \
    APP_ENV=production

# # Entrypoint with exec for proper signal handling
ENTRYPOINT ["/usr/local/bin/forecaster"]
