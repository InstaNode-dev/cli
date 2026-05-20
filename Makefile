# Makefile — instant CLI.
# The local gate (`make test`) is IDENTICAL to the CI gate so a CLI change
# cannot pass locally and fail in CI (or vice versa).

.PHONY: all build vet test test-race test-integration test-livesmoke ci clean install version

# ── B15-P0 (2) — build-info stamping ────────────────────────────────────────
# Wired in at link time via Go's -X linker flag. CLAUDE.md rule 14 (build-SHA
# gate) requires every deploy to verify the live binary's commit matches
# `git rev-parse --short HEAD`. The `make build` target stamps real values;
# unflagged `go build` falls back to the "dev" / "unknown" defaults declared
# in main.go so `go test` and `go run` still work.
VERSION    := $(shell cat VERSION 2>/dev/null || echo dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)

# `make` with no target = the full local gate.
all: ci

# build — produces the `instant` binary with version stamping. Drop into
# bin/instant so `make install` can pick it up; `go build ./...` builds every
# package which is what CI's gate wants.
build:
	go build -ldflags "$(LDFLAGS)" ./...

# install — drop the stamped binary under bin/ for local verification of
# `instant --version`. Use `go install` semantics: produce one binary named
# `instant`.
install:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/instant .

# version — for the deploy ritual (CLAUDE.md rule 23 step 4). After building,
# `make version` prints what the binary will report so an operator can grep
# the expected SHA before shipping.
version: install
	./bin/instant --version

vet:
	go vet ./...

# test — the hermetic suite. No network, no external deps. This is the
# MANDATORY gate: every push/PR must pass it. Includes the unit tests AND the
# command-level integration suite (cmd/integration_test.go), all driven
# against the in-process mock API (cmd/testapi_test.go).
test:
	go test ./... -count=1

# test-race — same suite under the race detector (what CI runs).
test-race:
	go test ./... -v -race -count=1

# test-integration — run ONLY the integration suite, verbosely.
test-integration:
	go test ./cmd/... -run TestIntegration -v -count=1

# test-livesmoke — OPTIONAL live-prod smoke test (provision-then-teardown).
# Off by default; gated behind the `livesmoke` build tag. Hits the real API.
#   make test-livesmoke
#   INSTANT_API_URL=http://localhost:8080 make test-livesmoke
test-livesmoke:
	go test ./cmd/... -tags livesmoke -run TestLiveSmoke -v -count=1

# ci — the exact gate CI enforces: build, vet, race-tested hermetic suite.
ci: build vet test-race

clean:
	rm -rf bin/
