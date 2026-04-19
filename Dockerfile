# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS builder
WORKDIR /src

# Cache modules
COPY go.mod go.sum ./
RUN go mod download

# Build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" \
    -o /out/engine ./cmd/engine

# Minimal runtime
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/engine /engine
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/engine"]
