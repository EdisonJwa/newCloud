# Quick start

This guide gets a working instance running with the least configuration, then explains how to run
the pieces separately for development.

## Prerequisites

- Go 1.26 or newer
- Docker (for PostgreSQL, and for the demo's TLS proxy)
- A modern browser

The repository embeds a prebuilt web client, so `go build ./cmd/newcloud` works without Node. The
client is rebuilt from TypeScript with pnpm only when the frontend changes.

## The demo: one command

The demo exists so that evaluating newCloud does not require an identity provider, a database, or
a certificate. It starts PostgreSQL in Docker, a bundled fake IdP, a TLS proxy with a generated
certificate, and the real `newcloud` binary:

```sh
git clone https://github.com/EdisonJwa/newCloud.git
cd newCloud
go run ./tools/demo
```

Open `https://localhost:8443`. The fake IdP signs in as `alice`. The demo seeds sample files, an
open share link, a password-protected share, and a drop-box link, and prints their URLs in the
terminal.

The demo is intentionally not configurable into a production system:

- the IdP accepts every login;
- the TLS certificate is self-signed;
- PostgreSQL runs in a disposable container;
- state lives in a temporary directory unless `-keep` or `-data` is passed.

Use `-h` to see the ports and state options. `go run ./tools/demo -no-seed` starts an empty
instance.

## Running the server directly

Use this path for backend, API, or WebDAV work.

### 1. Start PostgreSQL

```sh
docker run -d --name newcloud-pg \
  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=newcloud \
  -p 15432:5432 postgres:17
```

### 2. Write a development configuration

```sh
cat > config.dev.yml <<'YAML'
server:
  addr: "127.0.0.1:18500"
  public_url: "http://127.0.0.1:18500"
  usercontent_url: "http://localhost:18500"

auth:
  issuer: "http://127.0.0.1:19000"
  client_id: "newcloud"
  client_secret: "dev-secret"
  scopes: ["openid", "profile", "email", "groups"]
  group_role_map:
    "cloud-users": admin      # the dev IdP asserts this group; admin avoids a second account
  session_secret: "dev-only-session-secret-at-least-32-bytes"

storage:
  backend: local
  layout: blob
  local:
    root: "./dev-data/blobs"
  upload_dir: "./dev-data/uploads"
YAML
```

The server requires two different origins: session cookies live on `public_url`, and uploaded bytes
are served from `usercontent_url`. They must differ in scheme, host, or port. Both URLs point at
the same process; a reverse proxy is what makes them separate public origins in production.

Session cookies carry the `__Host-` prefix and `Secure`. Chrome treats `http://localhost` and
`http://127.0.0.1` as trustworthy and will store them, but this is a development convenience, not
a production pattern. For a browser session that behaves like production, use the demo or put a TLS
proxy in front of the process.

### 3. Run the development IdP

```sh
go run ./cmd/devidp -addr 127.0.0.1:19000 -client-id newcloud
```

The development IdP asserts one identity: `alice` / `alice@example.test`, in group `cloud-users`.
It accepts any client and any password. **Never deploy it.**

### 4. Migrate and serve

```sh
export DATABASE_URL="postgres://postgres:test@127.0.0.1:15432/newcloud?sslmode=disable"
go run ./cmd/newcloud migrate
NEWCLOUD_CONFIG=./config.dev.yml go run ./cmd/newcloud serve
```

The process logs the active storage layout and checks that the blob root and its `tmp/` directory
share a filesystem, because publishing a blob depends on `rename(2)` being atomic and local.

### 5. Sign in

Open the configured `public_url` and follow the login redirect. The first identity to sign in is
provisioned according to `auth.jit.mode` (`open` by default) and receives the role mapped from its
IdP groups.

If the group claim is missing, check that `auth.scopes` includes `groups`. Pocket ID does not
release group membership unless the scope is requested, and the server refuses to start without
it rather than silently granting nobody a role.

## Working on the web client

```sh
cd web
pnpm install
pnpm dev        # proxies /api and /s to 127.0.0.1:8080
```

The production bundle is written to `internal/webassets/dist` and embedded into the binary. Build
it with:

```sh
pnpm build
```

`make web` runs the API type generation and the production build. `make webcheck` runs the
TypeScript check, the contrast check, and the generated-client drift check.

## Running the tests

Most tests need no database. The store contract suite and the migration test use PostgreSQL when
`TEST_DATABASE_URL` is set and skip otherwise:

```sh
go test ./...
TEST_DATABASE_URL="$DATABASE_URL" go test ./internal/store
```

For the full pre-commit check:

```sh
make check
```

This runs `gofmt`, `go vet`, the example-configuration validation, the web checks, and the race
detector. The race detector requires CGO and a C compiler.

## Next steps

- [Configuration](configuration.md) — every key, with defaults and validation rules
- [Deployment](deployment.md) — a production topology that does not lose data
- [Architecture](../reference/architecture.md) — how requests, storage, and audit fit together
- [Limitations](limitations.md) — what is deliberately absent
