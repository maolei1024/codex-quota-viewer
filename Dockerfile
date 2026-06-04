FROM golang:1.23-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/codex-quota-viewer .

FROM alpine:3.21

RUN apk add --no-cache tzdata && addgroup -S app && adduser -S app -G app
WORKDIR /app

COPY --from=builder /out/codex-quota-viewer /app/codex-quota-viewer

USER app
EXPOSE 8080

ENV LISTEN_ADDR=:8080
ENV DATA_DIR=/data
ENV STALE_AFTER_MINUTES=30
ENV TZ=Asia/Shanghai

HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD wget -q --spider http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/codex-quota-viewer"]
