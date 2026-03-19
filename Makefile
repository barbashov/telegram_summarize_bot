.PHONY: lint test fmt build

lint:
	docker run --rm -v $(PWD):/app -w /app golangci/golangci-lint:v2.11.3 golangci-lint run

test:
	go test ./...

fmt:
	go fmt ./...

build:
	go build -o telegram_summarize_bot main.go
