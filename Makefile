VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS  = -s -w \
           -X github.com/B-A-M-N/portico/cmd.Version=$(VERSION) \
           -X github.com/B-A-M-N/portico/cmd.Commit=$(COMMIT) \
           -X github.com/B-A-M-N/portico/cmd.Date=$(DATE) \
           -X github.com/B-A-M-N/portico/internal/cli.Version=$(VERSION) \
           -X github.com/B-A-M-N/portico/internal/cli.Commit=$(COMMIT) \
           -X github.com/B-A-M-N/portico/internal/cli.Date=$(DATE)

SHELL := /bin/bash

.PHONY: build build-race install test test-race test-e2e vet staticcheck fmt-check validate acceptance clean

build:
	go build -ldflags '$(LDFLAGS)' -o portico .

build-race:
	go build -race -ldflags '$(LDFLAGS)' -o portico .

install: build
	mkdir -p $(HOME)/.local/bin
	cp portico $(HOME)/.local/bin/portico
	@echo "Installed portico to $(HOME)/.local/bin/portico"
	@echo "Make sure $(HOME)/.local/bin is in your PATH"

test:
	go test ./... -count=1 -v

test-race:
	go test -race ./... -count=1 -v

test-e2e:
	go test ./test/... -count=1 -v

vet:
	go vet ./...

staticcheck:
	staticcheck ./...

lint: vet staticcheck

fmt-check:
	@out="$$(gofmt -l . | grep -v '^vendor/' || true)"; \
	if [ -n "$$out" ]; then \
		echo "These files are not gofmt'd:"; echo "$$out"; exit 1; \
	fi

# validate is the gate a release must pass. It exists so the check is one
# command rather than five that have to be remembered in the right order — the
# way a step gets skipped is by it being a step someone has to remember.
validate: fmt-check
	go build ./...
	go vet ./...
	staticcheck ./...
	go test ./... -count=1
	go test -race ./... -count=1
	@echo
	@echo "All release gates passed."

# acceptance verifies the acceptance matrix against the tests it cites, rather
# than trusting the names written in it.
acceptance:
	./scripts/verify_acceptance_matrix.sh

clean:
	rm -f portico acceptance-report.txt
