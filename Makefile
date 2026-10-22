NAME=flowhub
BINDIR=bin
VERSION=$(shell git --no-pager describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT_SHA=$(shell git --no-pager rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILDTIME=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# Never trust an inherited GOROOT. Another Go on the host (mise, a /opt/go
# tarball) exports its own, and cmd/go would then run that tree's compiler
# against the `go` binary this shell resolves: "compile: version ... does not
# match go tool version ...". Empty means "derive it from the go binary", so
# every target below is consistent whichever toolchain PATH points at.
export GOROOT :=

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

.PHONY: default build run secrets test test-race test-live test-repeat e2e e2e-live e2e-turn e2e-flow e2e-comments e2e-soak e2e-restart lint staticcheck \
	fmt fmt-check smoke clean all releases \
	desktop-install desktop-dev desktop-app desktop-build desktop-check desktop-clean

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

# Live integration check against a running `opencode serve` (default
# http://127.0.0.1:4096). Skipped by the normal suite because it spends tokens and
# creates real sessions. Point it at a repository to verify the routing table:
#   OPENCODE_LIVE_DIR=/path/to/repo make test-live
# Real E2E: starts a disposable FlowHub process against the self-hosted YouTrack
# TEST project and the local opencode server. The token comes from a 0600 file;
# this target is never part of `make test` or CI.
FLOWHUB_E2E_ENV ?= /tmp/flowhub-e2e.env
e2e: build
	@set -a; . "$(FLOWHUB_E2E_ENV)"; . "$(HOME)/.config/flowhub/.env"; set +a; \
		FLOWHUB_E2E=1 go test -tags=e2e ./e2e -count=1 -v

# Requires the test FlowHub to own the address the YouTrack app posts to; set
# E2E_ADDR to that address and stop any real receiver on that port first.
e2e-live: build
	@set -a; . "$(FLOWHUB_E2E_ENV)"; . "$(HOME)/.config/flowhub/.env"; set +a; \
		FLOWHUB_E2E=1 FLOWHUB_E2E_LIVE=1 E2E_ADDR=$${E2E_ADDR:?set E2E_ADDR to the webhook target} \
		go test -tags=e2e ./e2e -count=1 -v

# Opt-in: shadow comparison and a real on-mode turn, then rollback to off.
e2e-flow: build
	@set -a; . "$(FLOWHUB_E2E_ENV)"; . "$(HOME)/.config/flowhub/.env"; set +a; \
		FLOWHUB_E2E=1 E2E_FLOW=1 go test -tags=e2e ./e2e -count=1 -v

# Opt-in: a real /opencode start comment turn.
e2e-comments: build
	@set -a; . "$(FLOWHUB_E2E_ENV)"; . "$(HOME)/.config/flowhub/.env"; set +a; \
		FLOWHUB_E2E=1 E2E_COMMENT=1 go test -tags=e2e ./e2e -count=1 -v

# Opt-in: N direct issueUpdated deliveries through shadow (default 10).
e2e-soak: build
	@set -a; . "$(FLOWHUB_E2E_ENV)"; . "$(HOME)/.config/flowhub/.env"; set +a; \
		FLOWHUB_E2E=1 E2E_SOAK=$${E2E_SOAK:-10} go test -tags=e2e ./e2e -count=1 -v

# Opt-in: stops and restarts the test receiver, then checks compare state.
e2e-restart: build
	@set -a; . "$(FLOWHUB_E2E_ENV)"; . "$(HOME)/.config/flowhub/.env"; set +a; \
		FLOWHUB_E2E=1 E2E_RESTART=1 go test -tags=e2e ./e2e -count=1 -v

# Opt-in: spends one real opencode turn and creates one test-project issue.
e2e-turn: build
	@set -a; . "$(FLOWHUB_E2E_ENV)"; . "$(HOME)/.config/flowhub/.env"; set +a; \
		FLOWHUB_E2E=1 E2E_TURN=1 go test -tags=e2e ./e2e -count=1 -v

test-live:
	OPENCODE_LIVE=1 go test -count=1 -timeout 5m -run TestLiveTask ./internal/opencode/

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

# ---- Desktop app (desktop/) ----
# Independent Rust/Node toolchain. These targets never feed the Go build; they
# exist so the desktop slice has the same one-command entry points as the
# service. See docs/adr/0008-desktop-agent-hub.md.

desktop-install:
	cd desktop && pnpm install --frozen-lockfile

desktop-dev:
	cd desktop && pnpm tauri dev

desktop-build:
	cd desktop && pnpm tauri build

# Run the app as an app. `tauri dev` launches a bare executable, so macOS has no
# application record to take the name and the icon from — Stage Manager draws a
# placeholder for it. This bundles the debug build and opens the bundle instead.
desktop-app:
	cd desktop && pnpm tauri build --debug --bundles app
	open "$(CURDIR)/desktop/src-tauri/target/debug/bundle/macos/FlowHub.app"

desktop-check:
	cd desktop && pnpm exec tsc --noEmit
	cd desktop && pnpm test
	cd desktop/src-tauri && cargo fmt --check
	cd desktop/src-tauri && cargo clippy --all-targets -- -D warnings
	cd desktop/src-tauri && cargo test

desktop-clean:
	cd desktop && rm -rf node_modules dist src-tauri/target src-tauri/gen
