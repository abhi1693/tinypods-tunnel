# Repository Guidelines

## Project structure

TinyPods is a single Go module. The CLI opens an outbound WebSocket to the relay, which forwards concurrent HTTP requests over HTTP/2 streams. Keep argument parsing and process startup in `cmd/tinypod/` and `cmd/tinypod-relay/`. Local HTTP and HTTPS forwarding belongs in `internal/agent/`. Visitor handling and agent session management belong in `internal/relay/`.

The [README](README.md) describes the running prototype. The files in `docs/` describe plans for hosting, authentication, and ingress policies.

## Build and run locally

Use Go 1.26 or later. Build both binaries with `go build -o ./bin/ ./cmd/...`. For a local smoke test, run these commands in separate terminals:

```sh
go run ./loadtest/origin -port 3000
go run ./cmd/tinypod-relay
go run ./cmd/tinypod http 3000
```

Set `TUNNEL_URL` to the URL printed by the CLI. Run `curl "$TUNNEL_URL/health?id=hello"` and expect `hello`. Each new connection receives a distinct hostname. Keep the unauthenticated agent listener on loopback for local development. For an HTTPS origin, use `tinypod https <port>` and set `TINYPOD_CA_FILE` if the app uses a development CA.

## Coding style

Run `gofmt` on changed Go files and `go vet ./...` for static checks. Follow Go conventions for tab indentation, lowercase package names, and mixedCase identifiers. Use four spaces and snake_case function names in the Python Locust workload.

## Testing

Run `go test ./...`. Use `go test -race ./...` when changing concurrent forwarding or session cleanup. Put package tests in adjacent `*_test.go` files and cross-component tests in `tests/`. Name tests after observable behavior, such as `TestHTTPSOriginTrust`. There is no configured coverage threshold.

For performance changes, run `loadtest/locustfile.py` against both the fixture and the relay using the same user count and duration. Report throughput, failures, and p95 latency. The README includes the headless commands.

## Commits and pull requests

Git history currently contains only `first commit`. Use a short, imperative subject describing one change. In a pull request, explain the observable effect, link a related issue when applicable, and list the checks you ran. Include the direct and tunneled measurements when claiming a performance improvement.
