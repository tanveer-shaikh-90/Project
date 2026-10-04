FROM golang:1.27.1-bookworm AS source
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .

FROM source AS test
CMD ["go", "test", "-mod=readonly", "-race", "-count=1", "-v", "-timeout=5m", "./..."]

FROM source AS build
RUN CGO_ENABLED=0 GOOS=linux go build -mod=readonly -trimpath -ldflags="-s -w" -o /app ./cmd/api

FROM python:3.12-slim AS load
WORKDIR /work
COPY scripts/requirements.txt scripts/requirements.txt
RUN pip install --no-cache-dir -r scripts/requirements.txt
COPY scripts/ scripts/
CMD ["python", "scripts/burst.py", "http://api:8080"]

FROM alpine:3.23 AS runtime
RUN apk add --no-cache ca-certificates wget && adduser -D -u 10001 appuser
COPY --from=build /app /app
USER appuser
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --retries=3 CMD wget -q -O /dev/null "http://127.0.0.1:${PORT:-8080}/health/ready" || exit 1
ENTRYPOINT ["/app"]
