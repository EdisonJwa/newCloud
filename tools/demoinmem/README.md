# Running the demo

There are two ways to see newCloud running. This page covers the one that needs no container runtime.

## `tools/demo` — the full demo (needs Docker)

The shipped demo starts PostgreSQL in a container, a fake identity provider, a TLS proxy and the real
binary, all from one command:

```sh
go run ./tools/demo
# open https://localhost:8443
```

Use this when Docker is available. It is the more representative setup because metadata lives in
PostgreSQL.

## `tools/demoinmem` — no Docker, no PostgreSQL

```sh
go run ./tools/demoinmem -data .demo-inmem
# open https://localhost:18443
```

Use this when Docker is unavailable or blocked. The server supports an in-memory metadata store — it
falls back to one when `DATABASE_URL` is unset, warning loudly — so a complete instance needs no
database at all. What this tool still makes real is everything a browser talks to:

- the project's own fake IdP, in-process;
- a TLS listener on `https://localhost:18443` whose certificate covers `localhost` **and**
  `content.localhost`, because `server.usercontent_url` now requires a different *hostname* from the
  app origin (a different port is a different browser origin but the same cookie jar);
- the real `newcloud` binary, built from this tree;
- seeded files and three shares, created by logging in through the real OIDC flow.

Sign in as `demo@example.test` with any password. The store is in memory, so a restart forgets
everything — that is the one meaningful difference from a real deployment.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data` | `.demo-inmem` | State directory: config, blobs, uploads, server log. |
| `-app-port` | `18080` | Loopback port the server listens on. |
| `-tls-port` | `18443` | Port the TLS proxy listens on. |
| `-idp-port` | `19201` | Port the in-process fake IdP listens on. |
| `-no-seed` | `false` | Start with an empty account. |
| `-bin` | empty | Use an existing binary instead of building one. |

The certificate is self-signed, so the browser will ask you to continue past a warning.

## What the demo does not demonstrate

- **Durability.** The metadata store is memory-only. Uploads resume across a *server process* restart
  (`WC-GAP-002`), but not across a demo restart, because the account and file rows vanish.
- **PostgreSQL behaviour.** Rows, constraints and the insert-only audit role are exercised by the
  contract suite against a real database, not here.
- **Real object storage.** `storage.backend` is `local`. S3 is covered by tests against a fake and
  moto; no real provider was exercised.

## A note on `demo-capture.html`

`demo-capture.html` in the repository root is a **static capture**, not the application. It renders
the JSON that a live instance returned, embedded verbatim. It exists because headless Chrome cannot
start in some sandboxes (Chrome's named-pipe IPC is blocked, so every `--headless` run dies during
startup), which makes a real screenshot impossible there. It is evidence, not a mock-up: regenerating
it means re-running the instance and re-fetching the endpoints.
