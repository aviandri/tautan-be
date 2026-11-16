export PATH := $(HOME)/.local/go/bin:$(PATH)

.PHONY: dev run build test vet fmt tidy

dev:            ## Run API on :8080 with embedded Postgres (data in ./.devdata)
	go run ./cmd/dev

run:            ## Run API against $$DATABASE_URL
	go run ./cmd/api

build:
	CGO_ENABLED=0 go build -o bin/api ./cmd/api

test:           ## Tests boot their own embedded Postgres
	go test -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy
