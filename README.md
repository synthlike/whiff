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
  in daily use: `git`, `gh`, `todo`, an `assistant` agent, `plan` +
  `pipeline`, Claude building a goal task by task, and `board` + `lead`, an
  agent that splits a goal into tickets and has a team build them.
- **Views**: a function can answer with items, a board or file locations.
  The CLI prints them for fzf (`whiff call todo/view | fzf | whiff act`);
  sniff shows them as pages with buttons.

Docs: [use cases](docs/use-cases.md) · [procs](docs/procs.md) ·
[design](docs/design.md) · [tasks](docs/tasks.md) · [stdlib](docs/stdlib.md)

## sniff

`sniff` opens whiff in a browser. Activity shows each request as a card:
what it asked, who answered, what it cost, and every message it caused, as
steps or as lanes:

![Activity: a lead's build of a small Go tool, in lanes](docs/images/sniff-activity.png)

The Map draws who talks to whom, one band a project, with the project's team
and what it spent today:

![Map: the wordy project's band](docs/images/sniff-map.png)

The Board shows what is being built in a project, ticket by ticket, beside
the team that builds it:

![Board: wordy's tickets and team](docs/images/sniff-board.png)

## Status

v0.7. Agents live in whiff: `(agent name :prompt :model :can [...])` gives
one a mailbox, whiff's functions as tools and `:limits` in dollars and time.
An agent can queue work, pause, set itself a turn for later, and hand parts
of a goal to others; their results come back to it as one turn, also after
a restart. Work belongs to a project, the repository it runs in: `:board`
plans a project's tickets and builds them through `pipeline`, with a team
of models per role, a check, a cap on runs at once and a budget a day. sniff
shows it all as Activity, the Map and the Board.

Next: a lead that plans the whole goal before building, a budget that counts
the lead's own turns, and asking the lead from sniff.

## Inspirations

- **Erlang/OTP**: addresses, mailboxes, plain-data messages, supervision.
- **Clojure**: immutable values, `[...]` and `{k v}` literals, keywords, `->`.
- **Rust**: Result and Option, `try` and `catch`.
- **[vrs](https://github.com/leoshimo/vrs)**: a lisp runtime for personal
  services, where whiff started.

## License

MIT, see [LICENSE](LICENSE).
