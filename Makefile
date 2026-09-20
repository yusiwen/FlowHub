NAME=flowhub
BINDIR=bin
VERSION=$(shell git --no-pager describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT_SHA=$(shell git --no-pager rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILDTIME=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GOBUILD=CGO_ENABLED=0 go build -trimpath -ldflags '-X "main.Version=$(VERSION)" \
		-X "main.CommitSHA=$(COMMIT_SHA)" \
		-X "main.BuildTime=$(BUILDTIME)" \
		-w -s -buildid='

# Tracked Go sources when the tree has an index, otherwise every Go file. The
# fallback keeps `make fmt` honest in a fresh clone or a source tarball.
GO_SOURCES=$(shell files="$$(git ls-files '*.go' 2>/dev/null)"; \
	[ -n "$$files" ] || files="$$(find . -type f -name '*.go' \
		-not -path './.git/*' -not -path './.direnv/*')"; echo "$$files")

PLATFORM_LIST = \
	linux-amd64 \
	linux-arm64 \
	darwin-arm64

.PHONY: default build run secrets test test-race test-repeat lint staticcheck \
	fmt fmt-check smoke clean all releases

default: build

build:
	@mkdir -p $(BINDIR)
	$(GOBUILD) -o $(BINDIR)/$(NAME) ./cmd/$(NAME)

# Start the receiver with the local defaults (./data). Pass configuration through
# the environment, e.g. `make run FLOWHUB_ALLOWED_SOURCES=127.0.0.1`.
run: build
	./$(BINDIR)/$(NAME) $(ARGS)

# Generate the two shared secrets. Put the key in the webhook URL and the token in
# the X-YouTrack-Token header; see README.md.
secrets:
	@echo "FLOWHUB_HOOK_KEY=$$(openssl rand -hex 32)"
	@echo "FLOWHUB_TOKEN=$$(openssl rand -hex 32)"

test:
	@go test ./... -count=1 2>&1; status=$$?; \
	if [ $$status -eq 0 ]; then \
		echo "=== ALL TESTS PASSED ==="; \
	else \
		echo "=== TESTS FAILED (exit $$status) ==="; \
	fi; \
	exit $$status

# Race detector run. Any data race fails the build. The detector needs cgo, which
# the Nix dev shell does not force off (see flake.nix).
test-race:
	CGO_ENABLED=1 go test -race ./... -count=1

# Repeat the suite in one process to catch leaked global state between runs.
test-repeat:
	go test ./... -count=3

# Blocking lint: `go vet` failures fail the build.
lint:
	go vet ./...

# Stricter linter, pinned so local runs match CI. Deliberately separate from
# `lint` so a missing tool can never turn that target into a no-op. If the
# environment cannot write staticcheck's default cache directory (for example a
# sandboxed agent), point STATICCHECK_CACHE at a writable path.
STATICCHECK_VERSION ?= v0.8.1

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

# Format the Go sources in place.
fmt:
	gofmt -w $(GO_SOURCES)

# Fail when a Go file is not gofmt-clean.
fmt-check:
	@files="$$(gofmt -l $(GO_SOURCES))"; \
	if [ -n "$$files" ]; then \
		echo "=== gofmt required for: ==="; echo "$$files"; exit 1; \
	fi

# End-to-end check against a real process: starts the receiver on a loopback
# port, posts one delivery per interesting shape, asserts the HTTP responses and
# the audit trail, and checks that no credential reaches disk. Uses a temporary
# data directory and cleans up after itself.
SMOKE_PORT ?= 18080

smoke: build
	BIN="$(CURDIR)/$(BINDIR)/$(NAME)" SMOKE_PORT="$(SMOKE_PORT)" ./scripts/smoke.sh

clean:
	rm -rf $(BINDIR)

# ---- Cross-compilation ----

linux-amd64:
	@mkdir -p $(BINDIR)
	GOARCH=amd64 GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@ ./cmd/$(NAME)

linux-arm64:
	@mkdir -p $(BINDIR)
	GOARCH=arm64 GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@ ./cmd/$(NAME)

darwin-arm64:
	@mkdir -p $(BINDIR)
	GOARCH=arm64 GOOS=darwin $(GOBUILD) -o $(BINDIR)/$(NAME)-$@ ./cmd/$(NAME)

all: $(PLATFORM_LIST)
	@echo "Built all platforms: $(PLATFORM_LIST)"

# Release builds: cross-compile all platforms and create compressed archives
gz_releases = $(addsuffix .tar.gz, $(PLATFORM_LIST))
zip_releases = $(addsuffix .zip, $(PLATFORM_LIST))

%.tar.gz: %
	tar czf $(BINDIR)/$(NAME)-$*.tar.gz -C $(BINDIR) $(NAME)-$*
	rm -f $(BINDIR)/$(NAME)-$*

%.zip: %
	cd $(BINDIR) && zip $(NAME)-$*.zip $(NAME)-$*
	rm -f $(BINDIR)/$(NAME)-$*

releases: $(gz_releases)
	@echo "Release archives: $(gz_releases)"

# Fuzz targets, as "<package>:<function>". `go test -fuzz` runs exactly one
# target per invocation, so this loops over the list. The seed corpus of the
# target already runs as part of `make test`; this explores further. The webhook
# parser is the untrusted-input boundary, so it is fuzzed first.
FUZZTIME ?= 30s
FUZZ_TARGETS = \
	./internal/webhook:FuzzParseAndSchema

fuzz:
	@for target in $(FUZZ_TARGETS); do \
		pkg=$${target%%:*}; fn=$${target##*:}; \
		echo "=== $$fn ($$pkg) for $(FUZZTIME)"; \
		go test -run=^$$ -fuzz=^$$fn$$ -fuzztime=$(FUZZTIME) $$pkg || exit 1; \
	done
