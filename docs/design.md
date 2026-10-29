# How whiff works

whiff is one daemon per user. Services written in puff and clients (the CLI,
hiss, sniff, agents) have addresses and talk by sending each other messages,
Erlang-style; the daemon routes them and keeps a history of every one. This
is how the pieces fit, and why they are the way they are. For what each
function does, see [stdlib.md](stdlib.md); for whiff in use,
[use-cases.md](use-cases.md).

## The shape

```
 whiff CLI    hiss    sniff    whiff mcp (agent)
     \          |       |        /
      JSON lines over /tmp/whiff-<uid>/whiff.sock
                      |
   ┌──────────────── daemon ─────────────────┐
   │ route goroutine: addresses, inboxes,    │
   │ jobs, waiters, subscriptions, history   │
   │                                         │
   │ :git  :gh  :todo  :pipeline  ...        │  a goroutine each,
   │ :jobs  :me  :review  :agent/1  :hiss    │  holding its own state
   └─────────────────────────────────────────┘
          ~/.whiff/services/*.puff   ~/.whiff/whiff.db
```

- **One owner for routing state.** A single goroutine owns the address table,
  inboxes, jobs, waiters, subscriptions and history. Everything else reaches
  it by handing it a function to run (`do`), so there are no locks around
  that state and no two places that can disagree about it.
- **A goroutine per service.** Each service handles its messages one at a
  time and holds its own state; nobody else touches it. A slow service only
  slows its own mailbox.
- **Clients are addresses too.** A connection can claim one (hiss as
  `:hiss`, an agent as `:agent/<n>`) and answer the messages sent to it, so a
  service calls `:hiss/toast` the way it calls `:gh/prs`.
- **One daemon per user, started on first use.** The socket lives in a 0700
  directory owned by you; a lock file keeps it to one daemon. Clients and
  daemon exchange a protocol version on connect; an old daemon is stopped
  with `whiff stop` and the next command starts the new one.
- **No config file.** Services are the files in `~/.whiff/services/`; their
  state is in `~/.whiff/whiff.db` (bbolt).

## Messages

A message is a target, `:address/fn`, and arguments. Arguments are puff
values, printed: nothing but data crosses an address, so a message reads the
same in history, over the socket and in a service.

| Kind | Sender gets | Use |
| --- | --- | --- |
| `call` | the target's Result, within 5s | asking for something |
| `send` | `(ok :done)` at once | telling, not waiting |
| `publish` | `(ok :done)`; every subscriber gets the value | events: `:gh/changed` |
| `send-after` | the message arrives later | timers, loops (`:tick`) |

Some answers take longer than a call waits: a job, a question to a person,
a message the review gate holds. Those answer **pending** at once, `(err
{:kind :pending :id 12})`, and the real answer comes later as a message,
`[:whiff/done 12 result]`, to the sender's `(on (whiff/done id r) ...)`, or
into history for a client, which picks it up with `whiff wait 12`. A handler
can pass its caller a pending it got (`hand-on`), so the caller waits for the
job instead of the handler.

An **ask** is a function with no body, `(ask (approve text) "...")`: its
messages wait in the address's inbox until something replies. `:me` is your
own address of asks: `(call :me/ask "go ahead?")` waits in `whiff inbox` for
`whiff reply`.

A call back into an address already waiting in the chain would wait for
itself, so it fails at once with `:cycle` instead of timing out.

## puff

puff is the language of services: a small lisp with immutable values, maps
and vectors, `match`, and Results and Options instead of nil and exceptions.
It has no tasks or channels; concurrency is the daemon's, and a service is
only ever handling one message.

- **Patterns take values apart,** in `match` arms, `let` bindings and a
  `fn`'s params: `(fn ([k {:step s}]) ...)` over a map's entries. A `match`
  with no arm that fits, or a `let` or param that doesn't, faults; `cond`
  and `match` end in a `_` arm for the rest.
- **Text for people and models reads as text.** A `"""` string starts on
  the next line and ends at a `"""` on a line of its own, without escapes;
  the closing `"""`'s indent comes off every line, so a prompt sits in a
  service file indented like the code around it.
- **Errors are values.** A failing builtin returns `(err {:kind ...})`;
  `try` passes one up from the function it is in, and `catch` handles the
  ones its arms match, `(catch (read) [{:kind :not-found} (ok "")])`, and
  lets the rest go on. An err dropped in a `do` faults rather than going
  unnoticed.
- **Faults are bugs.** Bad arguments, a non-exhaustive `match`, `(fail e)`:
  the handler stops, its caller gets `(err {:kind :fault})`, and the service
  restarts from its file. A file that doesn't load keeps the old version
  running and says why.
- **Every function has a mark,** `:read` or `:write`. A builtin's is in its
  table (`file/write` is `:write`); a handler's is inferred from what it
  reaches: a `:write` builtin, a def that does, or the function a literal
  target names. Computed targets are `:write`, and so is an `:update`
  handler or subscriber: the state it changes is yours. A program run with
  `proc/run`, `proc/output` or `:jobs/run` is `:write`, unless it is a
  literal command in the table of read-only commands
  (`internal/puff/commands.go`, which `whiff ls` names): `["git" "status"]`
  or `["gh" "pr" "list"]` with only the flags listed, and `git branch`
  without arguments, so not `git branch -D x`. A handler can claim `:read`
  over what it reaches (gh's poll only caches, and runs gh with computed
  arguments): that claim is an override, listed by `whiff ls`, and the only
  place a mark is trusted rather than inferred. A `:write` claim only
  raises a mark, so it needs no trust. At run time, a `:read` handler that
  reaches a `:write`, or runs a command outside the table, without an
  override faults, whoever called it: the backstop for what inference
  missed.

## Services

A service is a file, `(service name forms...)`: `(on (fn params...) "doc"
body...)` handlers, `(ask ...)` functions, `(on :topic (v) ...)`
subscribers and private `def`s.

- **Supervised.** The daemon looks at `~/.whiff/services/` twice a second:
  a saved file reloads, a deleted one unloads, a fault restarts the service
  from its file. `whiff try file.puff fn` loads a file under a scratch
  address, calls it once and unloads it.
- **Tested in place.** `(test "name" body...)` in a service file passes if
  its body is `#t`, or for `(= got want)`, if they are equal, and shows both
  when not. Loading a service skips its tests; `whiff test file.puff` loads
  it under a scratch address without its `:start` and runs them, as you,
  and so only from your connections. `puff -t` does the same for a plain
  puff file. Both also fail a file that isn't formatted as `puff -f`
  would, so a service an agent wrote comes back in the one layout.
- **State.** `(service gh :state {} ...)` gives every handler the state as its
  first parameter. An `:update` handler returns `(ok [reply state])`, or
  `(ok state)` for a subscriber or `whiff/done`, where nobody waits for a
  reply; the new state is committed to `whiff.db` before the reply is sent,
  so a caller never sees a reply whose state was lost. A reload or restart
  starts from the last committed state; `whiff reset` forgets it.
- **`:start`** runs after a service loads, as you: a service that polls
  sends itself its first `:tick` there.
- **Jobs.** Anything outside puff that takes a while runs as a job,
  `(call :jobs/run ["go" "test" "./..."] {:dir ... :timeout 10m})`: pending
  at once, `{:exit :stdout :stderr}` as its `whiff/done`, the last 64KiB of
  each stream kept. `:as :text` answers stdout as text, or an err unless it
  exits 0. This is how services run `claude -p`, `gh` or a build.
- **HTTP.** `(http/get url {:headers {...}})` answers the body of a 2xx, and
  `http/request` any method and the whole response. Both are `:write`: a
  request leaves the machine, and a GET can carry out in its URL whatever a
  handler read. A request blocks its service, 30s at most unless it says
  otherwise; a slow API is better polled with `send-after`.

## Views

A function can answer with a view, a plain value any client can show:
`(items [...])` to pick from, each `{:label :detail :action}` with its
action a message, `[:todo/done 12]`; `(locations [...])`, places in files;
`(text "...")`. `view/items` and friends build them and check their shape.
They are data like any other answer, so history and agents see them as
they are; the CLI prints items as lines for fzf, and `whiff act` sends the
action of the one you pick.

## Origins and the review gate

Every message belongs to a chain, and every chain has an **origin**: `:user`
if it began with you, `:agent` if it began with an agent, a job run with
`:origin :agent`, or an agent's answer. A message keeps its chain's origin
through every call, send, publish and timer it leads to, and an answer
continues the chain of the message it answers: the answer to a question you
asked isn't yours if an agent gave it.

The **review gate** is a routing rule on top: a message whose origin is
`:agent` to a `:write` function goes into `:review`'s inbox instead of its
target, and its sender gets `pending` at once. `whiff accept` delivers
exactly what was held, and from then on its chain counts as reviewed, so
what it leads to goes through; `whiff reject` answers it `(err {:kind
:rejected})`. An agent's eval that reaches `:write` is held as a whole, and
one that doesn't runs under `:read`, where the backstop catches what
inference missed; a publish to a topic with `:write` subscribers is held too.
Asks and `whiff/done`s aren't gated: a person answers an ask anyway, and a
`whiff/done` tells of something already started.

Rules that keep the gate honest:

- Marks are inferred, not declared, so the gate can rely on a target's mark.
- Only your connections review or set modes; an agent's connection can't,
  and `whiff mcp` always connects as an agent.
- An agent answers only its own inbox: it can't answer a question to you.
- Nobody reviewing (no client follows `:review/pending`) means a gated
  message fails with `:no-reviewer` at once, rather than waiting unseen.

Your attention is what the gate spends, so it holds only what something
further on won't hold anyway. An agent's reply to its own inbox goes
straight through: the answer is the agent's and unreviewed, so whatever its
sender does with it is gated there.

**Modes** let a session through for a while: `whiff trust agent/3 todo`
treats its messages to `:todo` as accepted (and so what `:todo` does for
it), and so its eval whose only writes are calls and sends to `:todo`;
`whiff free agent/3 --for 2h` treats everything it sends, publishes and
evaluates as accepted; `whiff gate agent/3` goes back. The session sees its
mode, `(mode)`, and `whiff mcp`'s tools say what goes through, so it doesn't
wait on what won't wait; they change at its next call. History records how
the gate treated each of an agent's writes (`held 5`, `accepted 5`,
`trusted`, `free`), and `whiff session agent/3` reads it back: the review at
the end of a run instead of one per message.

**What it doesn't cover.** The gate covers what reaches whiff. An agent
with a shell runs as your OS user and can run `whiff accept` as you; what
closes that is a sandbox that keeps its shell off the socket. A job's
program, `claude` with edit tools say, does what its own permissions let it:
whiff decides when it runs and where, not what it does to files. The table
of read-only commands narrows the trust rather than removing it: a
program's config and hooks (git's `core.fsmonitor`, a diff driver) or a
different binary of the same name earlier on `PATH` can still write, and
`git status` may refresh the index.

## Agents

`whiff mcp` is an MCP server on stdin and stdout. Each session claims
`:agent/<n>` and gets every other address's functions as tools, their docs
and marks in the descriptions, plus `eval`, `wait` (by id, so it works after
a reconnect) and `inbox`. Tool arguments are puff values written as strings,
as on the command line. A `:write` tool answers pending, like any gated
message; `wait` gives the result once you accept it.

## History

Every message is an entry: when, kind, origin, sender, target, arguments,
reply, time taken, and for an agent's write how the gate treated it. Notes
record whiff's own events: a service that failed to load or faulted, a mode
set or run out. History is in memory, the last 1000 entries; `whiff history
-f` and sniff follow it live, with nothing missed between the snapshot and
the stream.

## Limits, for now

- History, inboxes, `:review`'s queue and timers are in memory: a restart
  loses them, and a job running when whiff stops never answers. Service
  state is stored. (v0.5 stores the rest.)
- `whiff wait` finds an answer among the last 1000, like history: a busy
  daemon can push one out before a client that went away asks for it.
- A call waits 5s; anything longer is a job or an ask.
- No calendar maths in puff: `1d` is 24 hours.
- One machine. Peers over SSH are an idea, not a plan yet.
