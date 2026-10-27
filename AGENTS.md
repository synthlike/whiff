# whiff

A small daemon where puff services and clients (the CLI, hiss, agents) talk by
sending each other messages, Erlang-style, and that keeps a history of every
message. puff is its small lisp. Aim for 80/20: small, pleasant and useful beats
complete.

## Layout

- `cmd/whiff`: entry point (`whiff`, `whiff puff`, `whiff mcp`, ...).
- `internal/puff`: the language: reader, evaluator and the stdlib table (one entry
  per function: name, Go wrapper, `:read` or `:write`, one-line doc).
- `internal/daemon`: addresses, mailboxes, services, inboxes, pubsub, history.
  One goroutine owns the routing state; each service runs on its own goroutine and
  holds its own state. Others talk to them over channels and never touch state
  directly.
- `internal/rpc`: JSON lines over the per-user unix socket
  (`/tmp/whiff-<uid>/whiff.sock`) between the daemon and its clients.

## Rules

- Verify library APIs with `go doc` before using them; don't rely on memory.
- Ask before adding a dependency. Approved: `github.com/modelcontextprotocol/go-sdk`
  (for `whiff mcp`), `go.etcd.io/bbolt` (for durable state).
- No config file. Services live in `~/.whiff/services/`.
- puff values are immutable. There is no nil and no truthiness: lookups return
  Options, failures return Results, and messages are printed puff values.
- Errors from the reader and evaluator carry `file:line:col`.
- Work in small runnable steps: `go vet ./... && go test ./...` passes before each commit.
