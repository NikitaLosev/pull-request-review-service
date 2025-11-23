SHELL := /bin/bash
GOLANGCI_LINT_VERSION ?= v1.62.2

.PHONY: build run test lint docker-up docker-down loadtest

build:
	go build ./...

test:
	go test ./...

lint:
	docker run --rm -v $(PWD):/app -w /app golangci/golangci-lint:$(GOLANGCI_LINT_VERSION) golangci-lint run ./...

run:
	go run ./cmd/api/main.go

docker-up:
	docker-compose up --build

docker-down:
	docker-compose down -v

# Пример запуска k6 (см. loadtest.js)
loadtest:
	@command -v k6 >/dev/null 2>&1 || (echo "k6 is not installed. Install from https://k6.io/docs/get-started/installation/"; exit 1)
	k6 run loadtest.js
