# ── Build stage ────────────────────────────────────────────────────
#
# Standalone engine build (no server wrapper).
# Context: vectorless-engine/ directory.
#
FROM golang:1.25-alpine AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src

# 1) Module cache layer — only re-runs when deps change.
COPY go.mod go.sum ./
RUN go mod download

# 2) Copy only Go source directories needed for compilation.
COPY cmd/      ./cmd/
COPY pkg/      ./pkg/
COPY internal/ ./internal/

# 3) Build a fully-static, stripped binary.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /bin/engine \
      ./cmd/engine

# ── Runtime stage ──────────────────────────────────────────────────
#
# distroless/static:nonroot = ~2MB base. No shell, no package manager.
# Final image ≈ binary size + 2MB.
#
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /bin/engine /engine

USER nonroot:nonroot
ENTRYPOINT ["/engine"]
CMD ["--config", "/etc/vectorless/config.yaml"]

EXPOSE 8080

LABEL org.opencontainers.image.title="vectorless-engine"
LABEL org.opencontainers.image.description="Vectorless retrieval engine — structure-preserving document retrieval without embeddings"
LABEL org.opencontainers.image.source="https://github.com/hallelx2/vectorless-engine"
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL org.opencontainers.image.vendor="Vectorless"
