.PHONY: all build test lint fmt vulncheck

all: fmt lint test build

build:
	go build ./...

test:
	go test -v -race ./...

lint:
	golangci-lint run ./...

fmt:
	golangci-lint fmt ./...

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
