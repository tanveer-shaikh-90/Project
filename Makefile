# Seat Reservation service — common tasks.
# Usage: make <target>

BASE_URL ?= http://localhost:8080

.PHONY: help up down logs build run tidy test burst smoke fmt

help:
	@echo "Targets:"
	@echo "  up      - start Postgres + API via docker compose"
	@echo "  down    - stop and remove containers + volume"
	@echo "  logs    - tail API logs"
	@echo "  run     - run the API locally (needs local Postgres + DATABASE_URL)"
	@echo "  tidy    - go mod tidy"
	@echo "  burst   - run the stampede script against BASE_URL=$(BASE_URL)"
	@echo "  smoke   - quick end-to-end curl check against BASE_URL=$(BASE_URL)"

up:
	docker compose up --build -d

down:
	docker compose down -v

logs:
	docker compose logs -f api

build:
	go build ./...

run:
	go run ./cmd/api

tidy:
	go mod tidy

burst:
	python scripts/burst.py $(BASE_URL)

smoke:
	bash scripts/smoke.sh $(BASE_URL)

fmt:
	go fmt ./...
