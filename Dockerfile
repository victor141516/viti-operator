# syntax=docker/dockerfile:1.7

FROM node:22-alpine AS web-build
WORKDIR /src
COPY web-src/package.json web-src/package-lock.json ./web-src/
RUN npm ci --prefix web-src
COPY web-src/voice.js ./web-src/voice.js
COPY internal/app/web/ ./internal/app/web/
RUN npm --prefix web-src run build

FROM golang:1.26-alpine AS go-build
WORKDIR /src
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY --from=web-build /src/internal/app/web/voice.bundle.js ./internal/app/web/voice.bundle.js
RUN CGO_ENABLED=0 go test ./... && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/viti-operator ./cmd/viti-operator && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/viti-loopback-proxy ./cmd/viti-loopback-proxy

FROM alpine:3.22
RUN apk add --no-cache android-tools ca-certificates && \
    addgroup -S -g 10001 viti && adduser -S -D -h /home/viti -u 10001 -G viti viti
COPY --from=go-build /out/viti-operator /usr/local/bin/viti-operator
COPY --from=go-build /out/viti-loopback-proxy /usr/local/bin/viti-loopback-proxy
RUN mkdir -p /data /config /home/viti/.android && chown -R viti:viti /data /home/viti
USER viti
ENV HOME=/home/viti
VOLUME ["/data"]
EXPOSE 8787/tcp
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=6 \
  CMD wget -q -O /dev/null http://127.0.0.1:8787/api/v1/health || exit 1
ENTRYPOINT ["/usr/local/bin/viti-operator", "-config", "/config/viti.json"]
