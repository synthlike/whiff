# whiff

A small daemon of puffs: little services that talk to each other, and to you,
by sending messages.

Services are written in puff, a small lisp with immutable values and Results
instead of nil and exceptions. The CLI, the [hiss](https://github.com/synthlike/hiss)
editor and AI agents connect as clients with addresses of their own, so a
service can call `:gh/prs`, publish `:git/changed` or ask you a question and
get the answer later. whiff supervises the services, reloads them when you save
and keeps a history of every message.

Agents get whiff's functions as MCP tools. Anything an agent asks to write
waits in a review queue for you; nothing waits on you.

whiff has no UI of its own. Its CLI is enough to use it; hiss is the nicer
client.

## Inspirations

- Erlang/OTP: everything that talks has an address and a mailbox, messages
  are plain data, and services are supervised and restarted from their last
  good state.
- Clojure: immutable data that compares by value, with literals for vectors
  `[...]` and maps `{k v}`, keywords like `:read`, `let` with vector
  bindings, `loop` / `recur`, and `->` to thread a value through calls.
- Rust: Result and Option instead of nil and exceptions, and `try` to pass
  an error up.
- [vrs](https://github.com/leoshimo/vrs): a lisp runtime for personal
  services, the idea whiff started from.

## Status

v0.3. `go install ./cmd/...` installs two binaries:

- `puff` runs puff without a daemon: `puff file.puff args...` runs a file
  (calling its `main` if it defines one), `puff -e '(...)'` evaluates an
  expression and `puff` alone starts a REPL. Try `go run ./cmd/puff
  examples/hello.puff ann`.
- `whiff` is the daemon and its CLI. The first command starts the daemon,
  which loads the services in `~/.whiff/services/`, reloads them when you
  save and restarts them when they fault. `whiff call`, `send`, `pub` and
  `sub` talk to them; `whiff inbox`, `reply` and `wait` answer the questions
  they ask you and pick up long jobs; `whiff history -f` follows every
  message; `whiff -h` lists the rest.

Services keep state across restarts (`:state`, kept in `~/.whiff/whiff.db`;
`whiff reset` forgets it), subscribe to topics, run long work as jobs and
mark each function `:read` or `:write`. A function can answer with a view, a
plain value any client can show: items to pick from, each with an action,
places in files, or text. The CLI prints items as lines for fzf, and `whiff
act` sends the action of the one you pick. [services/](services/README.md) has
the ones in daily use: `git`, `gh` (which watches your pull requests), `todo`,
`reviewer` and `assistant`, a resident Claude agent you talk to through your
inbox, plus `pipeline` and `plan`: a planner, a developer and a reviewer
Claude building a goal task by task. [docs/use-cases.md](docs/use-cases.md)
shows whiff in use, [docs/design.md](docs/design.md) how it works, and
[docs/stdlib.md](docs/stdlib.md) lists every function puff has, and whiff's
own.

Agents connect through `whiff mcp` (`claude mcp add whiff -- whiff mcp`):
each session gets an address of its own, `:agent/<n>`, every service function
as a tool, and `eval`, `wait` and `inbox`. What they write waits for your
review: follow it with `whiff sub review/pending`, list it with `whiff
pending`, and answer with `whiff accept` or `whiff reject`; or let a session
through, to the services you name (`whiff trust agent/1 todo`) or entirely
(`whiff free agent/1 --for 2h`), until `whiff gate agent/1`. `whiff session
agent/1` is the review at the end: every write the session made, and whether
it waited, you accepted it or its mode let it through.

## License

MIT, see [LICENSE](LICENSE).
