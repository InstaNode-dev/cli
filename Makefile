# Makefile — instant CLI.
# The local gate (`make test`) is IDENTICAL to the CI gate so a CLI change
# cannot pass locally and fail in CI (or vice versa).

.PHONY: all build vet test test-race test-integration test-livesmoke ci clean

# `make` with no target = the full local gate.
all: ci

build:
	go build ./...

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
