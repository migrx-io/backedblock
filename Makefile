VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/migrx-io/backedblock/internal/cli.Version=$(VERSION)
BINDIR ?= $(HOME)/.local/bin
TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: help build install test vet fmt fmt-check check cross clean

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

build: ## build ./bbctl for this machine
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bbctl ./cmd/bbctl

install: build ## build bbctl and copy it to ~/.local/bin (BINDIR=... for another)
	install -d $(BINDIR)
	install -m 0755 bbctl $(BINDIR)/bbctl
	@echo "installed $(BINDIR)/bbctl"
	@$(BINDIR)/bbctl version

test: ## run unit tests
	go test ./...

vet: ## run go vet
	go vet ./...

fmt: ## format the code
	gofmt -w internal cmd

fmt-check: ## fail if any file needs gofmt
	@out=$$(gofmt -l internal cmd); if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

check: fmt-check vet test cross ## everything CI-worthy: gofmt, vet, tests, cross-builds

cross: ## build every release target into dist/<os>_<arch>/
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "$$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' \
			-o dist/$${os}_$${arch}/bbctl$$ext ./cmd/bbctl || exit 1; \
	done

clean: ## remove ./bbctl and dist/
	rm -rf bbctl dist
