# Contributing to Vectorless Engine

Thank you for your interest in contributing to the Vectorless Engine. This guide covers everything you need to get started.

## Prerequisites

Before you begin, make sure you have the following installed:

- **Go 1.25** or later
- **PostgreSQL 15+** (with the `pgvector` extension)
- **Docker** and **Docker Compose**
- **Git**
- **staticcheck** (`go install honnef.co/go/tools/cmd/staticcheck@latest`)

## Local Development Setup

1. **Clone the repository**

   ```bash
   git clone https://github.com/hallelx2/vectorless-engine.git
   cd vectorless-engine
   ```

2. **Start dependencies**

   Use Docker Compose to spin up PostgreSQL and any other required services:

   ```bash
   docker-compose up -d
   ```

3. **Set environment variables**

   Copy the example environment file and fill in your values:

   ```bash
   cp .env.example .env
   ```

   At a minimum, you need:

   ```
   VLE_DATABASE_URL=postgres://vectorless:vectorless@localhost:5432/vectorless?sslmode=disable
   VLE_ANTHROPIC_API_KEY=your-api-key-here
   ```

4. **Build the engine**

   ```bash
   go build -o bin/vectorless-engine ./cmd/engine/main.go
   ```

5. **Run the engine**

   ```bash
   ./bin/vectorless-engine
   ```

6. **Run the tests**

   ```bash
   go test ./...
   ```

## Code Style

All code must pass the following checks before being merged:

- **gofmt** -- all code must be formatted with `gofmt`. Run `gofmt -w .` to format in place.
- **go vet** -- run `go vet ./...` to catch common mistakes.
- **staticcheck** -- run `staticcheck ./...` for additional static analysis.

CI will reject any PR that fails these checks.

### General guidelines

- Follow the conventions in [Effective Go](https://go.dev/doc/effective_go).
- Keep functions short and focused.
- Prefer returning errors over panicking.
- Use meaningful variable and function names.
- Add comments for exported types and functions.

## Commit Message Conventions

We follow a simple commit message convention:

- Use the **imperative mood** in the subject line (e.g., "Add document ingestion endpoint", not "Added..." or "Adds...").
- Keep the subject line under 72 characters.
- Separate subject from body with a blank line.
- Use the body to explain *what* and *why*, not *how*.

### Examples

```
Add multi-document query endpoint

Support querying across multiple documents in a single request.
Results are merged and ranked by relevance score.
```

```
Fix chunk overlap calculation for large documents

The previous implementation could produce overlapping ranges that
exceeded the document boundary, causing an index-out-of-range panic.
```

## Pull Request Process

1. **Fork the repository** and create a feature branch from `main`:

   ```bash
   git checkout -b feat/your-feature main
   ```

2. **Make your changes** with clear, focused commits.

3. **Ensure all checks pass**:

   ```bash
   gofmt -l .
   go vet ./...
   staticcheck ./...
   go test ./...
   ```

4. **Push your branch** and open a pull request against `main`.

5. **Describe your changes** in the PR description. Include:
   - What the change does
   - Why the change is needed
   - How to test it
   - Any breaking changes

6. **Address review feedback** promptly. Push additional commits rather than force-pushing, so reviewers can see incremental changes.

7. A maintainer will merge your PR once it is approved and CI passes.

## Testing Guidelines

### Unit Tests

- Every new package or significant function should have accompanying unit tests.
- Place test files next to the code they test (e.g., `handler.go` and `handler_test.go`).
- Use table-driven tests where appropriate.
- Aim for meaningful coverage, not 100% line coverage.

### Integration Tests

Integration tests require a running PostgreSQL instance and are gated behind the `VLE_INTEGRATION` environment variable:

```bash
VLE_INTEGRATION=1 go test ./... -tags=integration
```

These tests are run in CI but are optional for local development.

### Running specific tests

```bash
# Run a single test
go test -run TestDocumentIngestion ./internal/handler/...

# Run tests with verbose output
go test -v ./...

# Run tests with race detection
go test -race ./...
```

## Architecture Overview

The codebase is organized as follows:

```
cmd/engine/         Entry point (main.go)
internal/
  handler/          HTTP handlers
  service/          Business logic
  repository/       Database access
  model/            Domain models
  config/           Configuration loading
  middleware/       HTTP middleware
docs/               API documentation and architecture notes
charts/             Helm chart for Kubernetes deployment
```

For detailed architecture documentation, see the [docs/](docs/) directory.

## License

By contributing to the Vectorless Engine, you agree that your contributions will be licensed under the same license as the project.
