# whiff

A small daemon where puff services and clients (the CLI, hiss, agents) talk by
sending each other messages, Erlang-style, and that keeps a history of every
message. puff is its small lisp. Aim for 80/20: small, pleasant and useful beats
complete.

## Layout

One module, five binaries: `go install ./cmd/...` installs them all.

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
- `cmd/huff`: runs a model with tools, a turn at a time: on its own, a
  prompt and its answer; in a whiff job holding an agent's session
  (`WHIFF_SESSION`), a turn of that agent whose tools are its `:can`
  functions; with `-tools`, also file tools as `claude -p`'s. whiff's
  agent harness: an `(agent ...)`, or a pipeline developer or reviewer,
  whose model names a provider (`openrouter/...`, `openai/...`,
  `local/...`) runs on it instead of the Claude CLI.
- `cmd/waft`: provisions Ubuntu hosts over ssh from a puff file (`waft plan`,
  `waft apply`): resources checked read-only and fixed with sudo, users
  with their setups. Outside releases until it earns its keep; it borrows
  only `internal/puff`. `cmd/waft/lib/whiff.puff` sets up whiff for a user,
  copied next to a host file.
- `internal/huff`: huff's turn loop, a provider-neutral transcript (puff
  data), file tools confined to a directory (Read, Write, Edit, Glob, Grep,
  and Bash for allowed commands, without a shell) and a `net/http` client
  for OpenAI's Chat Completions format, which OpenRouter, OpenAI and local
  servers share. It knows nothing about whiff.
- `internal/tools`: whiff functions described as tools for a model, and
  reading a call's arguments and answer: shared by `whiff mcp` and huff.
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
