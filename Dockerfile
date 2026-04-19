# Stage 1: Build Go binary
FROM golang:1.25-alpine AS builder

ARG VERSION=0.9.17

WORKDIR /build
COPY ui/ .
RUN go mod download
RUN CGO_ENABLED=0 go build -o constat-ui -ldflags="-s -w -X main.Version=${VERSION}" .

# Stage 2: Runtime
FROM alpine:3.21

ARG VERSION=0.9.17
ENV CONSTAT_VERSION=${VERSION}
LABEL org.opencontainers.image.version=${VERSION}
LABEL maintainer="ProphetSe7en" \
      description="Docker container monitor with web UI, Discord notifications, and auto-restart"

RUN apk add --no-cache \
    bash \
    curl \
    jq \
    docker-cli \
    tzdata

# regctl removed in v0.9.15 — update checks now use the Docker daemon's
# /distribution/{ref}/json endpoint (client.DistributionInspect) directly.
# Saves ~38 MB image size and eliminates subprocess spawn per container.

ENV TZ=Europe/Oslo
# Where regctl and docker-cli look for registry credentials. Persisted to
# /config so the user's logins survive container recreates.
ENV DOCKER_CONFIG=/config/.docker

COPY constat.sh /constat.sh
COPY constat.conf.sample /constat.conf.sample
COPY entrypoint.sh /entrypoint.sh
COPY --from=builder /build/constat-ui /constat-ui

RUN chmod +x /constat.sh /entrypoint.sh /constat-ui

HEALTHCHECK --interval=60s --timeout=5s --retries=3 \
    CMD wget -qO- http://localhost:7890/api/health > /dev/null 2>&1 || exit 1

EXPOSE 7890

ENTRYPOINT ["/entrypoint.sh"]
