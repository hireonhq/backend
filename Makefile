.PHONY: build run test test-integration vet fmt compare import verify

build:
	go build ./...

run:
	go run ./cmd/api

test:
	go test ./...

test-integration:
	TEST_DATABASE_URL="$$TEST_DATABASE_URL" go test -race ./...

vet:
	go vet ./...
	gofmt -l .

fmt:
	gofmt -w .

import:
	go run ./cmd/import ./dump

verify:
	go run ./cmd/verify-import

compare:
	python3 tools/compare.py $(NODE_BASE) $(GO_BASE)
