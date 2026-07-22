.PHONY: build run fmt lint test check css tools

build:
	CGO_ENABLED=0 go build -o miranda ./cmd/miranda

run: build
	./miranda

fmt:
	gofmt -w .
	goimports -w .

lint:
	golangci-lint run ./...

test:
	go test ./... -race

check: fmt lint test

css:
	./scripts/build-css.sh

tools:
	./scripts/download-tailwindcli.sh
	go install golang.org/x/tools/cmd/goimports@latest
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
