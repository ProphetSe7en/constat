# Stage 1: Build Go binary
FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY ui/ .
RUN go mod download
RUN CGO_ENABLED=0 go build -o constat-ui -ldflags="-s -w" .

# Stage 2: Runtime
FROM alpine:3.21

LABEL maintainer="ProphetSe7en" \
      description="Docker container monitor with web UI, Discord notifications, and auto-restart"

RUN apk add --no-cache \
    bash \
    curl \
    jq \
    docker-cli \
    tzdata

ENV TZ=Europe/Oslo

COPY constat.sh /constat.sh
COPY constat.conf /constat.conf.sample
COPY entrypoint.sh /entrypoint.sh
COPY --from=builder /build/constat-ui /constat-ui

RUN chmod +x /constat.sh /entrypoint.sh /constat-ui

HEALTHCHECK --interval=60s --timeout=5s --retries=3 \
    CMD wget -qO- http://localhost:7890/api/summary > /dev/null 2>&1 || exit 1

EXPOSE 7890

ENTRYPOINT ["/entrypoint.sh"]
