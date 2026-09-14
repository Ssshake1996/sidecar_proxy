FROM golang:1.27.0-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY migrations/ ./migrations/
COPY web/ ./web/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/sidecar-proxy .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S promptaudit && adduser -S -G promptaudit promptaudit
COPY --from=builder /out/sidecar-proxy /usr/local/bin/sidecar-proxy
USER promptaudit
EXPOSE 8090 8443
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD wget -q -T 3 -O /dev/null http://127.0.0.1:8090/healthz || \
        wget -q --no-check-certificate -T 3 -O /dev/null https://127.0.0.1:8443/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/sidecar-proxy"]
