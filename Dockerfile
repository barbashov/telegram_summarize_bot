FROM golang:1.22-alpine AS builder

WORKDIR /app

RUN apk add --no-cache build-base

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o summary_bot ./

FROM alpine:3.19

RUN apk add --no-cache ca-certificates sqlite-libs

WORKDIR /app

COPY --from=builder /app/summary_bot /app/summary_bot

ENV LISTEN_ADDR=:8080
EXPOSE 8080

CMD ["/app/summary_bot"]
