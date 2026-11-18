# TinyPods Tunnel

A Go CLI and relay for testing local web applications. Each CLI connection creates a tunnel and prints its own visitor URL. Several apps can share the same relay at the same time.

## Try it

With Go 1.26 or later and GNU Make, run `make build` to produce `bin/tinypod` and `bin/tinypod-relay`. For the homelab, `make build-arm64` writes both Linux ARM64 binaries to `bin/linux-arm64/`. Run `make help` to list the available targets.

For a standalone demo, start each command in its own terminal. To use your own app, skip the fixture and set `PORT` to the app's port.

```sh
make origin           # Start the included HTTP fixture on port 3000
make relay            # Start the relay
make http PORT=3000   # Connect the CLI to the app
```

The CLI prints a URL such as `http://<generated>.localhost:8080`. In a separate terminal, paste the actual URL at the `read` prompt and call the test app:

```sh
read -r TUNNEL_URL
curl "$TUNNEL_URL/health?id=hello"
# hello
```

For a local app serving HTTPS, use `make https PORT=8443`. The CLI verifies the certificate for `localhost` against system roots. Set `TINYPOD_CA_FILE` to a PEM file to trust a development CA. The command selects the protocol **between the CLI and the app**. The default visitor URL is HTTP on loopback for either command.

## Prototype topology

```text
<id-a>.localhost:8080 → relay → agent A → local app A
<id-b>.localhost:8080 → relay → agent B → local app B
```

The relay listens on `127.0.0.1:8080` for visitors and `127.0.0.1:8081` for agents. Each agent opens an outbound WebSocket to `/v1/connect`. The relay uses the visitor's hostname to select the agent and forwards the app's original path and query over an HTTP/2 stream. The bare relay address and unknown tunnel hostnames return 404. When an agent disconnects, its hostname is retired. Rerunning the CLI creates a new URL. Other connected tunnels keep serving requests.

The CLI only dials the selected loopback app. An unreachable app or untrusted origin certificate returns 502. The prototype allows up to 128 connected or registering tunnels, up to 128 concurrent visitor requests in total, and uploads up to 1 MiB.

For a private test deployment, set `-listen` and `-agent-listen` for the relay's network interfaces. Set `-public-url https://tunnels.example.org` to advertise URLs under `<generated>.tunnels.example.org`. Route wildcard DNS for `*.tunnels.example.org` to the visitor ingress, use a TLS certificate covering the wildcard, and preserve the requested hostname when proxying to the relay. Set `TINYPOD_RELAY` on each CLI to the full agent WebSocket URL, including `/v1/connect`. The prototype has **no authentication**. Keep the agent listener private. The relay listens over HTTP and requires upstream TLS termination for HTTPS or WSS.

## Load testing with Locust

Run the included fixture and connect the CLI as above. The first load-test target creates `.venv/` and installs the pinned Locust dependency. The workload was verified with Python 3.13 and Locust 2.47.0. Run these targets sequentially with the same settings:

```sh
make loadtest-direct
make loadtest-tunnel LOADTEST_HOST="$TUNNEL_URL"
```

Both default to 12 users, a spawn rate of 12 users/s, and 15 seconds. Override `USERS`, `SPAWN_RATE`, and `DURATION` to change the workload, or `LOADTEST_HOST` to select a different fixture address.

Locust mixes GETs with binary webhook POSTs. It checks the response status, unique request ID and payload to catch dropped bytes or responses delivered to the wrong caller. This is a closed-loop workload. Compare successful requests/s and latency percentiles on the same machine and workload. For your own app, adapt the fixture-specific paths and assertions in [the Locust workload](loadtest/locustfile.py).

### Local baseline

This baseline predates per-tunnel hostname routing. One Linux/amd64 loopback run on 2026-11-18 used 12 concurrent Locust users for 15 seconds per scenario against the included fixture. The POST body was 1,040 bytes. All three runs had zero failed requests.

| Request path | Requests | Requests/s | p95 | p99 |
| --- | ---: | ---: | ---: | ---: |
| Direct to HTTP app | 93,353 | 6,447 | 2 ms | 3 ms |
| Relay to HTTP app | 52,596 | 3,623 | 5 ms | 6 ms |
| Relay to HTTPS app | 47,084 | 3,247 | 6 ms | 8 ms |

The generator, app, CLI and relay shared one machine. Visitor traffic used HTTP in all three scenarios. WAN latency, public TLS termination and ARM64 performance require deployment-specific measurements.

## Tests

Run `make test` for Go unit and integration tests and `make test-race` to check concurrent requests and tunnel cleanup with the race detector. The race detector requires a C compiler.

Run `make coverage` to check every Go package, including both commands and the load-test origin, and the Python Locust workload. The target fails on any uncovered Go statement or Python statement or branch. It installs Python test dependencies in `.venv/` on the first run.
