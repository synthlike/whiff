# whiff

A small daemon where puff services and clients (the CLI, hiss, agents) talk by
sending each other messages, Erlang-style, and that keeps a history of every
message. puff is its small lisp. Aim for 80/20: small, pleasant and useful beats
complete.

## Layout

One module, three binaries: `go install ./cmd/...` installs them all.

- `cmd/puff`: runs puff without a daemon (`puff file.puff args...`, `puff -e`,
  `puff` for a REPL), with the base stdlib only.
- `cmd/whiff`: the daemon and its clients (`whiff`, `whiff call`, `whiff eval`,
  `whiff mcp`, ...).
- `cmd/sniff`: whiff's web UI, a client of the daemon like the CLI: `net/http`
  on 127.0.0.1 with a token and a Host check, and an embedded page in plain
  JS (`cmd/sniff/web`), with no framework and no build step. The page gets
  history as server-sent events and asks sniff for the rest as JSON; puff
  values reach it as typed trees, so it shows them as puff.
- `internal/puff`: the language: reader, printer, evaluator and the base stdlib
  table (one entry per function: name, Go wrapper, `:read` or `:write`, one-line
  doc). It knows nothing about whiff; `internal/daemon` adds its own functions
  (`call`, `send`, `inbox`, `reply`, ...) on top of the base table.
- `internal/daemon`: addresses, mailboxes, services, inboxes, pubsub, history.
  One goroutine owns the routing state; each service runs on its own goroutine and
  holds its own state. Others talk to them over channels and never touch state
  directly.
- `internal/rpc`: JSON lines over the per-user unix socket
  (`/tmp/whiff-<uid>/whiff.sock`) between the daemon and its clients.

## Rules

- Verify library APIs with `go doc` before using them; don't rely on memory.
- Ask before adding a dependency. Approved: `github.com/modelcontextprotocol/go-sdk`
  (for `whiff mcp`), `go.etcd.io/bbolt` (for durable state), `golang.org/x/term`
  (to tell a terminal from /dev/null).
- No config file. Services live in `~/.whiff/services/`.
- puff values are immutable. There is no nil and no truthiness: lookups return
  Options, failures return Results, and messages are printed puff values.
- Errors from the reader and evaluator carry `file:line:col`.
- Work in small runnable steps: `go vet ./... && go test ./...` passes before each commit.
