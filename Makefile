.PHONY: up down deps run build test test-race vet fmt migrate-up migrate-down

N ?= 1

MIGRATE = docker compose run --rm migrate -path=/migrations \
	-database=postgres://wagering_owner:owner_local_pw@postgres:5432/wagering?sslmode=disable

up: ## Build and start everything (3 app instances included)
	docker compose up --build

down: ## Stop everything and drop the database volume
	docker compose down -v

deps: ## Start only the dependencies, for running the app on the host
	docker compose up -d postgres migrate localstack keycloak

run: ## Run one instance on the host (needs `make deps` and a .env)
	set -a && . ./.env && set +a && go run ./cmd/api

build:
	go build -o bin/api ./cmd/api

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

migrate-up:
	$(MIGRATE) up

migrate-down: ## Revert the last N migrations (default 1): make migrate-down N=2
	$(MIGRATE) down $(N)
