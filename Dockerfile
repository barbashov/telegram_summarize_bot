FROM golang:1.21-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o telegram_summarize_bot .

FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/telegram_summarize_bot .
COPY --from=builder /app/.env.example .

RUN mkdir -p /app/data

ENV TZ=UTC

ENTRYPOINT ["./telegram_summarize_bot"]
