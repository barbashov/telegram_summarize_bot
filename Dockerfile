FROM golang:1.27.4-alpine3.24 AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o telegram_summarize_bot .

FROM alpine:3.24.4

RUN apk add --no-cache ca-certificates tzdata

RUN adduser -D -u 1000 bot

WORKDIR /app

COPY --from=builder /app/telegram_summarize_bot .

# chown so fresh named volumes inherit 1000:1000 ownership on first use
RUN mkdir -p /app/data && chown bot:bot /app/data

ENV TZ=UTC

USER bot

ENTRYPOINT ["./telegram_summarize_bot"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 \
  CMD test -n "$(find /tmp/telegram_summarize_bot.heartbeat -mmin -3 -print 2>/dev/null)"
