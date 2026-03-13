FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o telegram_summarize_bot .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/telegram_summarize_bot .

RUN mkdir -p /app/data

ENV TZ=UTC

ENTRYPOINT ["./telegram_summarize_bot"]
