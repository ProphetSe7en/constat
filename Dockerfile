# Stage 1: Build Go binary
FROM golang:1.24-alpine AS builder

ARG VERSION=0.9.13

WORKDIR /build
COPY ui/ .
RUN go mod download
RUN CGO_ENABLED=0 go build -o constat-ui -ldflags="-s -w -X main.Version=${VERSION}" .

# Stage 2: Runtime
FROM alpine:3.21

ARG VERSION=0.9.13
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

ARG REGCTL_VERSION=0.8.3
RUN wget -qO /usr/local/bin/regctl \
    "https://github.com/regclient/regclient/releases/download/v${REGCTL_VERSION}/regctl-linux-amd64" && \
    chmod +x /usr/local/bin/regctl

ENV TZ=Europe/Oslo

COPY constat.sh /constat.sh
COPY constat.conf.sample /constat.conf.sample
COPY entrypoint.sh /entrypoint.sh
COPY --from=builder /build/constat-ui /constat-ui

RUN chmod +x /constat.sh /entrypoint.sh /constat-ui

HEALTHCHECK --interval=60s --timeout=5s --retries=3 \
    CMD wget -qO- http://localhost:7890/api/summary > /dev/null 2>&1 || exit 1

EXPOSE 7890

ENTRYPOINT ["/entrypoint.sh"]
