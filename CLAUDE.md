# CLAUDE.md

## Project Overview

LLM routing proxy written in Go. Zero dependencies, single binary. Routes requests based on model name:

- `claude-*` / `anthropic/*` → Anthropic API (pass-through, keeps original auth headers)
- Everything else → downstream provider (Ollama, Bifrost, vLLM, etc.)

Designed to sit between Claude Code and multiple LLM backends.

## Tech Stack

- **Language:** Go (1.22+)
- **Dependencies:** None (stdlib only)
- **Architecture:** Single-file reverse proxy (`main.go`)

## Build & Run

```bash
# Build
go build -o llm-proxy .

# Run
DOWNSTREAM_URL=http://localhost:8080 PROXY_ADDR=:4000 ./llm-proxy

# Test
go test -v -race ./...
```

## Environment Variables

| Var              | Default                 | Description                 |
|------------------|-------------------------|-----------------------------|
| `DOWNSTREAM_URL` | `http://localhost:8080`  | Downstream provider address |
| `PROXY_ADDR`     | `:4000`                 | Listen address              |

## Code Structure

- `main.go` — entire application
  - `extractModel()` — parses `model` field from JSON body
  - `isAnthropicModel()` — routing decision (case-insensitive)
  - `validateURL()` — URL validation (scheme, host)
  - `sanitizeURL()` — removes credentials for safe logging
  - `proxyTo()` — reverse proxy with hop-by-hop header filtering
  - `proxyHandler.ServeHTTP()` — main HTTP handler with routing logic
  - `rateLimiter` — token bucket rate limiter (stdlib only)
  - `metrics` — in-memory request counters
  - `run()` — server lifecycle with graceful shutdown
- `main_test.go` — comprehensive test suite (20 tests)

## Endpoints

| Path       | Method | Description                              |
|------------|--------|------------------------------------------|
| `/health`  | GET    | Health check (returns `{"status":"ok"}`) |
| `/metrics` | GET    | Request counters (JSON)                  |
| `/*`       | *      | Proxy routing                            |

## Security Features

- Request body size limit (100MB)
- HTTP server timeouts (read/write/idle)
- Proxy transport timeouts (dial/TLS/idle)
- Hop-by-hop header filtering (RFC 7230)
- URL scheme validation (http/https only)
- HTTP method validation (blocks TRACE/CONNECT)
- Token bucket rate limiting
- Proxy error handler (no internal details leaked)
- Log sanitization (credentials masked)
- Graceful shutdown (SIGINT/SIGTERM)

## Conventions

- No external dependencies — keep it stdlib-only
- Single-file architecture — avoid splitting unless necessary
- Structured logging with `slog` (JSON output)
- All functions must have tests
