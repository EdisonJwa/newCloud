.PHONY: test test-race build lint fmt clean race vuln check devidp e2e uicheck dropcheck example-config

GO ?= go

test:
	$(GO) test ./...

# The race detector requires cgo and a working C compiler. On a Windows
# workstation without gcc this fails; CI (Linux) runs it unconditionally.
# The check below turns "gcc not found" into an actionable message instead of a
# wall of runtime/cgo build errors.
race:
	@$(GO) env CGO_ENABLED >/dev/null 2>&1 || true
	@if command -v gcc >/dev/null 2>&1 || command -v clang >/dev/null 2>&1 || command -v cl >/dev/null 2>&1; then \
		CGO_ENABLED=1 $(GO) test -race ./...; \
	else \
		echo "race: no C compiler found; skipping -race (CI runs this on Linux)"; \
		echo "race: install gcc (e.g. via MSYS2) to enable it locally"; \
	fi

test-race: race

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o bin/newcloud ./cmd/newcloud

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
check: lint example-config race

clean:
	rm -rf bin
