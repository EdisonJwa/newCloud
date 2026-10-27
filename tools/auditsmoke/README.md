# Audit proof harnesses

Two small programs that demonstrate, on a throwaway database, the two properties the audit log
claims and that no unit test can show in *your* environment:

- `auditsmoke` appends events through the production audit writer, so the chain is built by the
  same code the server uses.
- `anchorsmoke` publishes a real checkpoint and can then delete the newest events, which is the
  one attack the hash chain cannot see on its own.

They exist because of a measured gap: `newcloud audit verify` verified the chain but never
compared the anchors, so a log with its newest events deleted reported `audit chain verified` and
exited `0`. The anchors are what make truncation detectable, and the command now compares them.
Run these once when you deploy — the operations guide's "Proving the check catches tampering"
section walks through it.

```sh
# a disposable database: these programs do not clean up after themselves on purpose
DATABASE_URL="postgres://…" newcloud migrate

go run ./tools/auditsmoke  -database "$DATABASE_URL" -events 25
go run ./tools/anchorsmoke -database "$DATABASE_URL" -dir /tmp/anchors -sign -truncate 5

newcloud audit verify -anchors /tmp/anchors -pubkey <printed by anchorsmoke>
# exit 1: "checkpoints disagree with the chain: events have been removed"
```

Never point them at a database whose contents matter: `-truncate` deletes real audit events, and
the point of the exercise is to watch the check catch it.
