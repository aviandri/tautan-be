export PATH := $(HOME)/.local/go/bin:$(PATH)

.PHONY: dev run build test vet fmt tidy seed-heavy seed-heavy-all

dev:            ## Run API on :8080 with embedded Postgres (data in ./.devdata)
	go run ./cmd/dev

seed-heavy:     ## (Re)create the perf test page "heavy" (baseline 13 blocks + 9 sell cards)
	go run ./cmd/seed

seed-heavy-all: ## (Re)create "heavy_all": every block type, a folder and a second tab
	go run ./cmd/seed -full

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
