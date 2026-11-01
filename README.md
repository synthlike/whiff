# whiff

whiff runs puffs: small services that talk to each other, and to you, by
sending messages. Agents can use them too, and what they write waits for your
review.

```lisp
; ~/.whiff/services/greet.puff
(service greet
  (on (hello name) "Greets name."
    (ok (string/concat "hello, " name))))
```

```sh
$ whiff call greet/hello ann
hello, ann
```

Services are written in puff, a small lisp with immutable values and Results
instead of nil and exceptions. whiff supervises them, reloads them when you
save, keeps their state across restarts, and keeps a history of every message.

## Install

```sh
go install ./cmd/...
```

| Binary  | What it is |
| ------- | ---------- |
| `whiff` | The daemon and its CLI: `call`, `send`, `sub`, `inbox`, `history -f`, … (`whiff -h`) |
| `sniff` | The web UI: watch messages live, see services' state, review and answer |
| `puff`  | puff without a daemon: run a file, `-e` an expression, a REPL |

Try puff on its own: `go run ./cmd/puff examples/hello.puff ann`.

## Agents, safely

```sh
claude mcp add whiff -- whiff mcp
```

Each agent session gets an address (`:agent/1`) and every service function
as a tool. Reads go straight through; writes wait for you:

```sh
whiff pending              # what waits for review
whiff accept 12            # or: whiff reject 12 "why"
whiff trust agent/1 todo   # let its writes to :todo through
whiff session agent/1      # everything the session wrote, and how
```

## What's in it

- **Services** keep state, subscribe to topics, run long jobs, and mark each
  function `:read` or `:write`. [services/](services/README.md) has the ones
  in daily use: `git`, `gh`, `todo`, an `assistant` agent, and `plan` +
  `pipeline`, Claude building a goal task by task.
- **Views**: a function can answer with items, a board or file locations.
  The CLI prints them for fzf (`whiff call todo/view | fzf | whiff act`);
  sniff shows them as pages with buttons.

Docs: [use cases](docs/use-cases.md) · [design](docs/design.md) ·
[stdlib](docs/stdlib.md)

## Status

v0.5. sniff brings views into one window, with a review banner, searchable
activity, message chains and address details, and a ⌘K palette that goes to
or calls any address, view or function, with keys to move through activity.
The CLI shares its history filters and can show message chains with `whiff
history -c`.

v0.6 will focus on reliable work and recovery: durable inboxes, timers and
task results, explicit interruption handling and safe retries. Service state
and inboxes, the review queue too, already survive restarts; timers and history
are still in memory.

## Inspirations

- **Erlang/OTP**: addresses, mailboxes, plain-data messages, supervision.
- **Clojure**: immutable values, `[...]` and `{k v}` literals, keywords, `->`.
- **Rust**: Result and Option, `try` and `catch`.
- **[vrs](https://github.com/leoshimo/vrs)**: a lisp runtime for personal
  services, where whiff started.

## License

MIT, see [LICENSE](LICENSE).
