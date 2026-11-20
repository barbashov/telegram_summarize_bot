.PHONY: build lint test fmt vulncheck gosec security

build:
	go build -o telegram_summarize_bot main.go

lint:
	docker run --rm -v $(PWD):/app -w /app golangci/golangci-lint:v2.15.0 golangci-lint run

test:
	go test ./...

fmt:
	go fmt ./...

# Known vulnerabilities in dependencies and the Go stdlib (reachable-call analysis).
vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...

# Static security analysis (SSRF, weak crypto, unsafe SQL, etc.).
gosec:
	go run github.com/securego/gosec/v2/cmd/gosec@v2.30.0 ./...

security: vulncheck gosec
