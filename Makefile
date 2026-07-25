VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS  = -s -w \
           -X github.com/paoloanzn/portico/cmd.Version=$(VERSION) \
           -X github.com/paoloanzn/portico/cmd.Commit=$(COMMIT) \
           -X github.com/paoloanzn/portico/cmd.Date=$(DATE)

SHELL := /bin/bash

.PHONY: build build-race install test test-race test-e2e vet staticcheck clean

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

clean:
	rm -f portico
