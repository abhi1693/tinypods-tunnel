.DEFAULT_GOAL := build

export CGO_ENABLED ?= 0
PORT ?= 3000
PYTHON ?= python3
VENV ?= .venv
USERS ?= 12
SPAWN_RATE ?= 12
DURATION ?= 15s

.PHONY: build build-arm64 relay origin http https fmt vet test test-race coverage coverage-go coverage-python loadtest-direct loadtest-tunnel help

build: ## Build the CLI and relay in bin/
	go build -o ./bin/ ./cmd/...

build-arm64: ## Cross-compile both binaries for the Linux ARM64 homelab
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o ./bin/linux-arm64/ ./cmd/...

relay: ## Start the local relay
	go run ./cmd/tinypod-relay

origin: ## Start the demo HTTP app on PORT (default 3000)
	go run ./loadtest/origin -port "$(PORT)"

http: ## Connect the relay to a local HTTP app on PORT
	go run ./cmd/tinypod http "$(PORT)"

https: ## Connect the relay to a local HTTPS app on PORT
	go run ./cmd/tinypod https "$(PORT)"

fmt: ## Format Go source
	go fmt ./...

vet: ## Run Go static checks
	go vet ./...

test: ## Run unit and integration tests
	go test ./...

test-race: ## Run tests with the Go race detector (requires a C compiler)
	CGO_ENABLED=1 go test -race ./...

coverage: coverage-go coverage-python ## Verify full Go and Locust workload coverage

coverage-go: ## Check every Go package for uncovered statements
	go test -count=1 -covermode=atomic -coverpkg=./... -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
	@awk 'NR > 1 && $$2 > 0 { hits[$$1] += $$3 } \
		END { for (block in hits) if (!hits[block]) { print "Uncovered:", block; failed = 1 }; exit failed }' coverage.out

coverage-python: $(VENV)/.coverage-ready ## Check Locust workload statements and branches
	LOCUST_SKIP_MONKEY_PATCH=1 "$(VENV)/bin/python" -m coverage run --branch --source=loadtest --omit='*/test_*.py' -m unittest discover -s loadtest
	"$(VENV)/bin/python" -m coverage report --fail-under=100 --show-missing

$(VENV)/.locust-ready: loadtest/requirements.txt
	"$(PYTHON)" -m venv "$(VENV)"
	"$(VENV)/bin/python" -m pip install -r $<
	touch "$@"

$(VENV)/.coverage-ready: $(VENV)/.locust-ready Makefile
	"$(VENV)/bin/python" -m pip install "coverage==7.17.3"
	touch "$@"

loadtest-direct: LOADTEST_HOST = http://127.0.0.1:$(PORT)
loadtest-direct: ## Benchmark the running demo app directly
loadtest-tunnel: ## Benchmark the printed tunnel URL (set LOADTEST_HOST)
ifneq ($(filter loadtest-tunnel,$(MAKECMDGOALS)),)
ifeq ($(strip $(LOADTEST_HOST)),)
$(error Set LOADTEST_HOST to the tunnel URL printed by tinypod)
endif
endif
loadtest-direct loadtest-tunnel: $(VENV)/.locust-ready
	"$(VENV)/bin/locust" -f loadtest/locustfile.py --headless --only-summary \
		-u "$(USERS)" -r "$(SPAWN_RATE)" -t "$(DURATION)" --host "$(LOADTEST_HOST)"

help: ## List available targets
	@awk -F ':.*## ' '/^[a-z][a-z0-9-]*:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
