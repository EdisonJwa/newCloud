.PHONY: test test-race build web webcheck api apicheck lint fmt clean race vuln check devidp e2e uicheck dropcheck example-config sbom release

GO ?= go

# The version a build reports. Without this every binary said "dev", so during an
# incident nobody could answer "which build is running" — and `newcloud version` is
# what tells an operator whether a rollback actually landed (2026-10-24 review, H8).
# A release build overrides it: make build VERSION=v1.2.3
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

test:
	$(GO) test -p 1 ./...

# The race detector requires cgo and a working C compiler. On a Windows
# workstation without gcc this fails; CI (Linux) runs it unconditionally.
# The check below turns "gcc not found" into an actionable message instead of a
# wall of runtime/cgo build errors.
race:
	@$(GO) env CGO_ENABLED >/dev/null 2>&1 || true
	@if command -v gcc >/dev/null 2>&1 || command -v clang >/dev/null 2>&1 || command -v cl >/dev/null 2>&1; then \
		CGO_ENABLED=1 $(GO) test -race -p 1 ./...; \
	else \
		echo "race: no C compiler found; skipping -race (CI runs this on Linux)"; \
		echo "race: install gcc (e.g. via MSYS2) to enable it locally"; \
	fi

test-race: race

# Rebuild the browser client. The output lands in internal/webassets/dist, which is committed
# and embedded, so `go build` on its own always works; this target guarantees it is current.
web:
	cd web && pnpm install --frozen-lockfile && pnpm gen:api && pnpm build

# Regenerate the client types from the OpenAPI document.
api:
	cd web && pnpm gen:api

# Client checks. Mirrors the `web` job in CI so a local `make webcheck` means the same thing.
webcheck:
	cd web && pnpm typecheck
	cd web && pnpm contrast
	cd web && pnpm gen:api:check

build: web
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o bin/newcloud ./cmd/newcloud

fmt:
	gofmt -w .

lint:
	gofmt -l .
	$(GO) vet ./...

vuln:
	govulncheck ./...

# Validate the shipped example configuration. An example that fails validation is
# worse than none: operators copy it and hit an error that is not in their own file.
example-config:
	$(GO) test ./internal/config/ -run TestExampleConfig -count=1

# Development helpers. These need a running server; they are not part of `check`.
devidp:
	$(GO) run ./cmd/devidp -addr 127.0.0.1:19000 -client-id newcloud

e2e:
	$(GO) run ./tools/e2e -app http://127.0.0.1:18500 -idp http://127.0.0.1:19000

uicheck:
	$(GO) run ./tools/uicheck -app http://127.0.0.1:18500 -public-host 127.0.0.1:18600

# Verify drag-and-drop upload in a real browser against a running instance.
#
# It needs the demo (or any instance) plus Chrome, so it is not part of `check`: a CI image
# without a browser would fail it for the wrong reason.
dropcheck:
	$(GO) run ./tools/demoshot -base https://localhost:8443 -out demo-run/dropcheck -test-drop

# Full pre-commit check.
check: lint example-config webcheck race

clean:
	rm -rf bin

# --- release artifacts -------------------------------------------------------
#
# What a self-hosted deployment needs in order to answer "which build is this, and what is in it":
# one binary per platform stamped with the version it came from, a checksum file, and an SBOM
# naming every module the binary actually imports.
#
# The version defaults to `git describe`, so a build from a dirty tree says so. A release overrides
# it: make release VERSION=v1.2.3
DIST ?= dist
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

# The SBOM is generated from the module graph of ./cmd/newcloud, not from the whole module graph:
# see tools/sbom for why a list that includes this repository's own lint plugins is the wrong answer
# to "what is in the binary".
sbom:
	@mkdir -p $(DIST)
	$(GO) run ./tools/sbom -version "$(VERSION)" -o "$(DIST)/sbom.cdx.json"
	@echo "wrote $(DIST)/sbom.cdx.json"

# Cross-compiled with CGO_ENABLED=0: the whole binary is pure Go, so a release needs no per-platform
# toolchain. -trimpath keeps the build path out of the binary, which is both a reproducibility and a
# disclosure question.
release:
	$(GO) run ./tools/release -dist "$(DIST)" -version "$(VERSION)" \
		-platforms "$(PLATFORMS)" -ldflags '$(LDFLAGS)'
	@echo "release artifacts in $(DIST)/ for $(VERSION)"
# Fuzzing the parsers that stand between user input and the filesystem.
#
# Not part of `test`: fuzzing wants minutes, and a suite that takes minutes on every push is a suite
# people disable. The seed corpus runs as an ordinary test, so CI covers the examples; this target is
# for the exploration.
fuzz:
	go test ./internal/storage/layout -run '^$$' -fuzz FuzzValidatePath -fuzztime 60s
	go test ./internal/storage/layout -run '^$$' -fuzz FuzzValidateSegment -fuzztime 60s
	go test ./internal/server -run '^$$' -fuzz FuzzParseContentRange -fuzztime 60s