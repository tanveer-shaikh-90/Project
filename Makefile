# Seat Reservation service — common tasks.
# Usage: make <target>

BASE_URL ?= http://localhost:8080

.PHONY: help up down logs build run tidy test burst smoke fmt observe

help:
	@echo "Targets:"
	@echo "  up      - start Postgres + API via docker compose"
	@echo "  down    - stop containers, retain database volume"
	@echo "  logs    - tail API logs"
	@echo "  run     - run the API locally (needs local Postgres + DATABASE_URL)"
	@echo "  tidy    - go mod tidy"
	@echo "  test    - database-backed Go tests in Docker"
	@echo "  burst   - stampede against BASE_URL (export ADMIN_TOKEN first)"
	@echo "  smoke   - small Docker-based API contract check"
	@echo "  observe - start Prometheus at http://localhost:9090"

up:
	docker compose up --build --wait

down:
	docker compose down

logs:
	docker compose logs -f api

build:
	go build ./...

run:
	go run ./cmd/api

tidy:
	go mod tidy

burst:
	python3 scripts/burst.py $(BASE_URL)

smoke:
	docker compose --profile tools run --build --rm load python scripts/burst.py http://api:8080 --smoke

test:
	docker compose --profile tools run --build --rm test

observe:
	docker compose --profile observe up -d prometheus

fmt:
	go fmt ./...
