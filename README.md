# llm-proxy

Minimal LLM routing proxy. Zero dependencies, single binary.

## Why not LiteLLM?

LiteLLM's PyPI package was hit by a supply chain attack in March 2026 (TeamPCP, CVE-2025-26399) — versions containing a credential stealer were published. Not usable in pentest environments.

Any OpenAI-compatible provider can be used as the downstream target: Bifrost, Ollama, vLLM, etc.

```
Claude Code → llm-proxy → claude-*      → Anthropic API
                        → other models  → downstream → Ollama (local)
```

## Routing logic

Model matching is case-insensitive.

| Model prefix    | Backend       |
|-----------------|---------------|
| `claude-*`      | Anthropic API |
| `anthropic/*`   | Anthropic API |
| everything else | downstream    |

## Security features

- Request body size limit (100MB)
- HTTP server timeouts (read: 30s, write: 120s, idle: 60s)
- Proxy transport timeouts (dial: 10s, TLS handshake: 5s)
- Hop-by-hop header filtering (RFC 7230)
- URL scheme validation (http/https only)
- HTTP method validation (blocks TRACE/CONNECT)
- Token bucket rate limiting (100 req/s, burst 20)
- Proxy error handler (no internal details leaked)
- Log sanitization (credentials masked)
- Graceful shutdown (SIGINT/SIGTERM)
- Structured JSON logging (slog)

## Endpoints

| Path       | Method | Description                              |
|------------|--------|------------------------------------------|
| `/health`  | GET    | Health check (returns `{"status":"ok"}`) |
| `/metrics` | GET    | Request counters (JSON)                  |
| `/*`       | *      | Proxy routing                            |

## Build

Requires Go 1.22+.

```bash
# Quick build
make build

# Cross-compile for all platforms
make release

# Or manually
GOOS=darwin GOARCH=arm64 go build -o llm-proxy .
```

## Test

```bash
# Via Makefile
make test

# Or directly
go test -v -race ./...
```

## Makefile targets

| Target    | Description                                                            |
|-----------|------------------------------------------------------------------------|
| `build`   | Build binary for current platform                                      |
| `release` | Cross-compile for darwin/arm64, darwin/amd64, linux/amd64, linux/arm64 |
| `test`    | Run all tests with race detector                                       |
| `vet`     | Run go vet                                                             |
| `fmt`     | Format code with gofmt                                                 |
| `run`     | Build and run                                                          |
| `clean`   | Remove build artifacts                                                 |

## Usage

```bash
# Run
DOWNSTREAM_URL=http://localhost:8080 \
PROXY_ADDR=:4000 \
./llm-proxy

# Point Claude Code to the proxy
export ANTHROPIC_BASE_URL=http://localhost:4000
export ANTHROPIC_API_KEY=sk-ant-xxx   # Claude Code sends this in the header, proxy passes it through
export ANTHROPIC_AUTH_TOKEN=sk-ant-xxx

# Subagent model examples
# claude-opus-4-6         → Anthropic
# claude-sonnet-4-6       → Anthropic
# CLAUDE-haiku-4-5        → Anthropic (case-insensitive)
# qwen2.5-coder:32b       → downstream → Ollama
# openai/gpt-4o           → downstream
```

## Docker

```bash
docker build -t llm-proxy .
docker run -e DOWNSTREAM_URL=http://host.docker.internal:8080 \
           -p 4000:4000 llm-proxy
```

## Releases

Binaries are automatically built and published via GitHub Actions when a tag is pushed:

```bash
git tag v1.0.0
git push origin v1.0.0
```

Pre-built binaries for macOS (Apple Silicon, Intel) and Linux (amd64, arm64) will be available on the [Releases](../../releases) page with SHA256 checksums.

## Env vars

| Var              | Default                | Description                                            |
|------------------|------------------------|--------------------------------------------------------|
| `DOWNSTREAM_URL` | `http://localhost:8080` | Downstream provider address (Ollama, Bifrost, vLLM...) |
| `PROXY_ADDR`     | `:4000`                | Listen port                                            |
