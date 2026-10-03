# ---- Build stage ----
FROM golang:1.23-alpine AS build
WORKDIR /src

# Resolve dependencies first for better layer caching. go mod tidy also
# generates go.sum, so a clean checkout (without go.sum) still builds.
COPY go.mod ./
COPY go.sum* ./
RUN go mod download || true

COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app ./cmd/api

# ---- Runtime stage ----
FROM alpine:3.20
RUN adduser -D -u 10001 appuser
COPY --from=build /app /app
USER appuser
EXPOSE 8080
ENTRYPOINT ["/app"]
