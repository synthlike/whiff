# How whiff works

whiff is one daemon per user. Services written in puff and clients (the CLI,
sniff, agents and external clients such as hiss) have addresses and talk by
sending each other messages, Erlang-style; the daemon routes them and keeps
a history of every one. This is how the pieces fit, and why they are the
way they are. For what each
function does, see [stdlib.md](stdlib.md); for whiff in use,
[use-cases.md](use-cases.md).

New to the system? Start with [Learn puff](puff.md) and
[Follow a message](walkthrough.md). [Agents](agents.md) covers running and
controlling agents, and [Troubleshooting](troubleshooting.md) explains
how to inspect and recover work.

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
   │ :jobs :me :review :tasks :agent/1 :hiss │  holding its own state
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
into history for a client, which picks it up with `whiff wait 12`. whiff
stores the result before anyone is told and keeps it 30 days, so a wait
finds it after a restart too. A service gets the result again after a
restart until it has handled it: the mark that it did commits with its
state, so its state never sees a result twice, though a handler's other
effects can happen twice. One it faults on isn't given again, and one for
a service that isn't loaded waits until it is. A failure whiff gives says
in `:outcome` what happened when a task is cancelled, expires or is interrupted:
`:not-started`, `:stopped` or `:unknown`.
`whiff cancel 12` ends one that hasn't: held or waiting, it leaves its
inbox; a running job is killed. An agent cancels only what its session
began. A task ends once: an answer after its end is dropped, and history
says so. A handler can pass its caller a pending it got (`hand-on`), so the
caller waits for the job instead of the handler. A service that keeps the
work itself turns the pending into `(ok 12)` with `(submitted r)`, and
leaves a real failure, like `(err {:kind :start})`, as it is. A held one is
submitted too: it keeps its id, so the same `whiff/done 12` brings the job's
result, or `:rejected` if you reject it.

An **ask** is a function with no body, `(ask (approve text) "...")`: its
messages wait in the address's inbox until something replies. `:me` is your
own address of asks: `(call :me/ask "go ahead?")` waits in `whiff inbox` for
`whiff reply`. A service can answer its own asks: with an `(on (whiff/asked
id) ...)`, whiff tells it of each message that reaches its inbox, again
when it loads, and again after each `whiff/done` it handles, for those still
waiting, so it takes them one at a time and skips what it has begun. Like a
`whiff/done` that isn't gated, but what its handler does is, by the chain
the ask's sender began: an agent's ask is still an agent's write. An
`(on (whiff/cancelled id) ...)` hears, the same way, of a message
cancelled while it waited (`whiff cancel`), so work begun for it can stop:
pipeline cancels the run it started for a `build`.

`:tasks` shows work whoever began it: `(call :tasks/list)` gives every
task held, waiting or running, and up to 50 of the latest that ended in
the last day, with its owner, where it is or who answered it, and how it
ended;
`:tasks/view` gives them as items, with a job's attempts and, after a
crash, whether its process may still run, and what can be done with each: Accept
or Reject one held, Cancel one that hasn't ended, see one's whole result,
and the actions that result offers, `{:actions [...]}` in its `(ok m)` or
`(err m)`, as a pipeline build's Retry; `:tasks/failed` only those that
failed, not those you cancelled or rejected. A task begun in another's
work keeps it as its `:parent`, so `:tasks/tree id` gives an agent's ask
with the turn that answered it, or a build with its dev, check and
reviewer. whiff decides none of those: an
action is a message to the service that offered it. `:tasks/cancel` is
yours, as `:review`'s functions are; an agent cancels its session's own
tasks with `whiff cancel`.

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
  unnoticed. One may offer the next step as `:actions`, the messages
  `:me/tell` and views offer, `(err {:kind :no-run :actions [{:label
  "Merge whiff/2" :action [:pipeline/merge 2]}]})`: a stale action's
  error offers the ones that hold now. `whiff call` prints them as lines
  `whiff act` takes, and sniff as buttons under what it answered, from
  a view, ⌘K or Call again; a past reply in history shows only their
  links, as what it offered may no longer hold.
- **Faults are bugs.** Bad arguments, a non-exhaustive `match`, `(fail e)`:
  the handler stops, its caller gets `(err {:kind :fault})`, and the service
  restarts from its file. A file that doesn't load keeps the old version
  running and says why. A fault says where it was called from, `(called
  at f.puff:5:62, ...)`; a pattern that doesn't fit says what it wanted,
  `path: {} doesn't match {:dir d}: no :dir`; and an `unwrap` names what it
  was given, with the short locals in it, `(map/get-in state [:runs n]) is
  (none), where n is 2`, so a required lookup needs no helper of its own.
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
subscribers and private `def`s. An agent is a file too, `(agent name
:prompt "..." [:model "..."] [:can [:addr/fn ...]] [:dir path] [:limits
{:turn usd :day usd :time d :tasks n}] [:every d])`, which whiff runs as a service
of its own turn loop (`internal/daemon/agent.puff`): `tell`, whose answer
lands in `:me`'s inbox, `ask`, whose answer goes back to whoever asked,
`forget`, `usage`, `stop`, `cancel`, `restart`, `pause` and `resume`, a `wake` with
`:every`, and `later d note`, its turns' own tool: a turn of its own once
`d` has passed, with the note, kept in its home so a restart or reload
sets it again. `later` is `:read`, as all it adds is a turn, which is
gated and paid for as any. A paused agent takes no turns: what it is sent waits in its
inbox, and only one wake, as the next is set as one is taken. `stop` ends
the turn running now; `cancel` ends all it is doing: it pauses the agent,
stops its turn and cancels the tasks its turns handed out (`session/cancel`),
so their services stop the work begun for them and the cancelled results
wait for you to resume it. A turn isn't run again on its own once it ran,
as its tools may have written: one claude didn't start, as its session had
begun, is taken again at once, resuming it; one that failed after it
started leaves you why, with a Restart the turn action (`restart turn`),
a new turn of the same conversation given the message, why it failed and
the tasks it had started, so it carries on instead of doing them twice.
A task that failed is retried by its service, as pipeline's Retry. Tells, asks,
wakes and laters, and the results of what its turns started, are all asks of the agent, so they wait in its inbox while a turn
runs and take turns oldest first; your reply to an answer is a tell. Its turns are held
to `:limits`, $1 and 10 minutes a turn and $5 a day unless it says
otherwise: claude stops a turn at its dollars, whiff at its time, and once
the agent has spent its day's in the last 24 hours, its turns' and what
its tasks' results say they cost, its turns fail `:over-budget` until it
has spent less, which you hear of once. Its session may have 10 tasks
open at once, or `:tasks`. Parents and children are in
[tasks.md](tasks.md#parents-and-children).

- **Supervised.** The daemon looks at `~/.whiff/services/` twice a second:
  a saved file reloads, a deleted one unloads, a fault restarts the service
  from its file. `whiff try <file.puff> <fn>` loads a file under a scratch
  address, calls it once and unloads it. `whiff services` (`service/list`)
  says how each file is, a word and at most one note, what needs you
  first: `failed` to load and why, `faulting` (restarted in the last
  hour), an agent `over budget`, `waiting` on you (its messages in your
  inbox, its writes held for review; for an agent, its questions, as its
  reports wait for nothing), `thinking` (with the attempt, if its turn
  runs again) or `busy` with jobs, an agent `busy` waiting on the tasks
  it handed out (`waiting on :pipeline (task 178) for 4m0s`), then
  `idle` agents and `ok` services. `whiff services <service>`
  (`service/status`) is one in full: its file, its load error, its
  faults today and the last one's message, the last message it handled,
  its inbox and timers, and for an agent its turns and dollars of the
  last 24 hours against `:day`, its `:every`, its session's mode, and
  its work: what its turns handed out today, each waiting, held for your
  review, or how it came back (`ready whiff/7, $0.31` for a pipeline
  run, with what its dev turns cost). sniff's ⌘K palette shows
  the same, agents first: a service's dot and word only when it isn't
  `ok` or `idle`, a file that failed to load among the addresses, and the
  details under Status when you pick one, an agent's work as a list under
  them. Its status strip adds up the agents' spend of the last 24 hours
  against their `:day`.
- **Shared code.** `(use "lib/http-check.puff")` brings another file's
  defs and tests in, as if they were written there: a handler's mark sees
  through them, a name defined twice is an error at both places, and
  `whiff test` runs the used file's tests too. A used file holds only
  defs, tests and uses. A path starts from the using file's directory, its
  target's if it is a symlink, so a service linked into
  `~/.whiff/services/` uses the lib next to its source; a file comes in
  once however many use it, and a cycle is an error at the use that closes
  it. Changing a used file reloads the services that use it. Plain puff
  files and the REPL use files too (from the working directory); an
  agent's eval can't, since its review would show only the `use`.
- **Tested in place.** `(test "name" body...)` in a service file passes if
  its body is `#t`, or for `(= got want)`, if they are equal, and shows both
  when not. Loading a service skips its tests; `whiff test <file.puff>` loads
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
  sends itself its first `:tick` there, with `send-after`, which waits
  for the service to have its address however soon it is due.
- **Jobs.** Anything outside puff that takes a while runs as a job,
  `(call :jobs/run ["go" "test" "./..."] {:dir ... :timeout 10m})`: pending
  at once, `{:exit :stdout :stderr}` as its `whiff/done`, the last 64KiB of
  each stream kept. `:output :file` keeps all of both too, in
  `~/.whiff/jobs/<id>/stdout` and `stderr`, and its result says where, as
  `:output`: a scraped page or a long turn isn't cut. They go 30 days
  after they were last written, at start. `:as :text` answers stdout as
  text, or an err unless it exits 0. This is how services run `claude -p`, `gh` or a build.
  `:jobs/output id` is a running job's output so far. With `:stream #t`
  each line it prints is published on `:jobs/<id>` as it comes, and its
  result's stdout is the last line: an agent's turn runs `claude -p
  --output-format stream-json` so, and `:tasks/watch id` shows the turn's
  steps as they happen, what it says and calls and what answers. A job times out
  after 1h unless its `:timeout` says otherwise; `:timeout :none`, for a
  server, runs until it ends or is stopped. `:sandbox #t` runs it in a
  container that sees only its `:dir`: see [Sandbox](#sandbox).
- **HTTP.** `(http/get url {:headers {...}})` answers the body of a 2xx, and
  `http/request` any method and the whole response. Both are `:write`: a
  request leaves the machine, and a GET can carry out in its URL whatever a
  handler read. A request blocks its service, 30s at most unless it says
  otherwise; a slow API is better polled with `send-after`.

## Views

A function can answer with a view, a plain value any client can show:
`(items [...])` to pick from, each `{:label :detail :tag :actions}`, :tag a word, as a project, that sniff draws in a colour of its own;
`(locations [...])`, places in files; `(text "...")`; `(board [...])`,
columns of items. `view/items` and friends build them and check their
shape. They are data like any other answer, so history and agents see them
as they are.

A view carries what can be done next, as hypertext carries its links: an
item's `:actions` are what its service offers for it now, each a label
saying what it does and the message that does it, `{:label "Cancel the
run" :action [:pipeline/cancel 3]}` (`:action` alone is one without a
label). A client knows nothing of tickets or runs: it shows the buttons and
sends the one you pick, as an ordinary message under the ordinary rules. An
action is an offer, not a permission: its handler checks again, so one from
a stale view fails as any message would. The CLI prints a line per action,
for fzf, and `whiff act` sends the one you pick.

A view names the topic published when it changes, `(view/board cols
{:changed :plan/changed})`, so a client showing it knows when to fetch it
again.

sniff draws them, and pins one as a page of its own, `/view/plan/board`
(`?args=[2]` for a function that takes some): it calls the function as
you, only if it is `:read`, and again each time the view's topic fires.
Marks make actions links or buttons, as GET and POST: an action to a
`:read` function is a link to its answer's page, and any other a button,
which shows its target beside its label, since a label can say anything.
Clicking one sends it as you, as `whiff act` does: sniff is one of your
connections, and only its own page can send through it, as JSON from its
origin with its cookie.

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

The gate as one table, which `TestGateTable` checks row by row
(a peer's rows are `TestFar`'s):

| Who sends | To | Gate |
| --- | --- | --- |
| you, or a chain you began, until an agent's answer joins it | anything | through |
| an agent | a `:read` function, or an ask | through |
| an agent | a `:write` function, or a service that reaches one | held |
| a connection holding `:agent/*`, whatever its requests say | a `:write` | held |
| a session trusted for the target's service | a `:write` there | through |
| a session trusted for another service | a `:write` here | held |
| a free session | anything | through |
| a service, on a job's result with `:origin :agent` | a `:write` | held |
| a service, on a plain job's result | a `:write` | through |
| an agent's eval | reaching a `:write` | held, as a whole |
| an agent's eval | only reads | through |
| a service, on a task an agent answered on a peer | a `:write` | held |

Rules that keep the gate honest:

- Marks are inferred, with explicit `:read` overrides listed by
  `whiff ls`. A `:write` declaration can only raise a mark; a `:read`
  override is a trust decision, as described under puff above.
- Only your connections review or set modes; an agent's connection can't,
  and `whiff mcp` always connects as an agent. `:review`'s functions
  (`pending`, a view; `accept`; `reject`; `trust`) review for any client
  that sends messages, sniff's pages and `whiff act` too, and `:me/inbox`
  and `:me/answer` answer your inbox the same way. They are yours: they
  answer only a message from one of your connections, not an agent's, nor
  a service's, even in a chain you began. The gate never holds a message
  to them, and `whiff mcp` doesn't offer them as tools.
- An agent answers only its own inbox: it can't answer a question to you.
- Nobody reviewing (no client follows `:review/pending`) means a gated
  message fails with `:no-reviewer` at once, rather than waiting unseen.
  An open sniff follows it, showing how many wait, so it reviews.

Your attention is what the gate spends, so it holds only what something
further on won't hold anyway. An agent's reply to its own inbox goes
straight through: the answer is the agent's and unreviewed, so whatever its
sender does with it is gated there.

**Modes** let a session through for a while: `whiff trust agent/3 todo`
treats its messages to `:todo` as accepted (and so what `:todo` does for
it), and so its eval whose only writes are calls and sends to `:todo`;
`whiff free agent/3 --for 2h` treats everything it sends, publishes and
evaluates as accepted; `whiff gate agent/3` goes back. The session sees its
mode, `(session/mode)`, and `whiff mcp`'s tools say what goes through, so it doesn't
wait on what won't wait; they change at its next call. History records how
the gate treated each of an agent's writes (`held 5`, `accepted 5`,
`trusted`, `free`), and `whiff session agent/3` reads it back: the review at
the end of a run instead of one per message.

**What it doesn't cover.** The gate covers what reaches whiff. An agent
with a shell runs as your OS user and can run `whiff accept` as you: a
`claude -p` agent's does. huff has no shell, and its Bash's commands can
run in the [sandbox](#sandbox), which keeps them off the socket. A job's
program, `claude` with edit tools say, does what its own permissions let it,
unless it is sandboxed:
whiff decides when it runs and where, not what it does to files. The table
of read-only commands narrows the trust rather than removing it: a
program's config and hooks (git's `core.fsmonitor`, a diff driver) or a
different binary of the same name earlier on `PATH` can still write, and
`git status` may refresh the index.

## Agents

`whiff mcp` is an MCP server on stdin and stdout. Each session claims
`:agent/<n>` and gets every other address's functions as tools, their docs
and marks in the descriptions, plus `eval`, `wait` (by id, so it works after
a reconnect), `cancel` and `inbox`. Tool arguments are puff values written
as strings, as on the command line. A `:write` tool answers pending, like any gated
message; `wait` gives the result once you accept it.

An agent with `:can` gets those functions as its turns' tools. Its turn's
job runs with `:session can`: whiff gives the job a token in
`WHIFF_SESSION`, which its `whiff mcp` claims the agent's own session with,
`:agent/<name>`, until the job ends. So every turn is the same session,
whose mode holds from one turn to the next (`whiff trust agent/researcher
gh`), and across restarts, in whiff.db, until the agent's file goes
(`whiff services researcher` shows it), and history has its calls under it. The session may call or send to
its `:can` functions and nothing else: no eval, publish, subscribe or
other inbox, enforced by whiff, not by Claude's permissions. Its tools act
in the agent's `:dir`, `whiff mcp --dir`, while its Claude session stays in
its home. The fence
holds because a turn has no shell: `--tools ""`, and `whiff mcp` alone.
Any service's job may have such a session, as a new `:agent/<n>`: a
pipeline run an agent started gives its dev Claude the agent's `ask`,
and nothing else of whiff's, so a worker asks its lead and waits for the
answer. It reaches only what the service could call itself, and it is
gated as any agent session is.

So a turn has no effects of its own: what it does is its tools' writes,
which its session's mode gates, and its cost, which `:limits` caps. Its
job therefore passes the review gate, whoever's message it answers: an
agent may ask another (`:can [:helper/ask]`) without you reviewing each
turn either one takes. What a turn started and couldn't wait for, as a
pending write or another agent's ask, ends after it: whiff leaves the
result in the agent's inbox as `whiff/result`, which takes a turn like a
tell and answers only with news. The results of what one turn started
wait for each other and take one turn together, so a lead that starts
three runs pays for one turn on their results, not three. An ask's answer waits for what its turn
started: once the last of those tasks ends, the turn on its result answers
whoever asked, so a service that asks an agent gets the delegated answer,
not "I've asked". The turn that started a task is the last before it, as
jobs and tasks share their ids, and `session/tasks` says which of the
agent's are still open. A loop of asks between agents is bounded
by their `:limits`, each turn costing its dollars of each one's day.

## Sandbox

What runs code an agent wrote is a command: huff's Bash, and jobs such as
a pipeline's check. The sandbox runs those in a container and leaves the
agent out: huff's model loop, its key and its file tools stay on the
host, the file tools confined to their directory as before. So nothing in
the container needs whiff, and nothing of whiff's is there. A job asks
with `:sandbox #t`, huff with `-sandbox`, a pipeline run or a board team
with `:sandbox #t` (for a developer on huff); `{:net #t}` and `-net` give
it the network.

- **Runtimes.** Apple's `container` on macOS, a small Linux VM per
  container, and rootless Podman on Linux. Both run one image, from the
  Containerfile embedded in whiff: Go, git and node, no agent CLI and no
  credentials, tagged with the file's hash, so a whiff whose Containerfile
  changed asks for a new build. `whiff sandbox build` builds it, and makes
  the network commands without one run on where the runtime needs that;
  `whiff sandbox check` says what's missing.
- **What a command sees.** Its directory, read-write, at the same path,
  so paths in its output are the host's. Go's module cache read-only, if
  the host has one, so builds need no network for modules it has, and
  Go's build cache in a volume of the runtime's that only sandboxed
  commands share. In a git worktree, as a pipeline run's, the
  repository's `.git` read-only, so status, diff and log work there,
  while a commit fails and nothing in the container can write the
  repository's hooks, config or refs. Not your home, other repositories,
  whiff's socket or the agent's key; no network unless asked; `GOTOOLCHAIN=local`, so go never
  fetches a toolchain.
- **Failing closed.** A command that asks for the sandbox where there is
  no runtime, or no image, doesn't start, and says what to run.
- **Lifetime.** A container per command, removed as it ends: it starts in
  about a second on macOS, as a VM boots, and faster on Linux. A cancel
  sends the runtime's client SIGTERM, which `--init` passes on to the
  command, then ends the container by name, as a client can leave it
  running (Apple's exits without passing SIGTERM on while its VM boots).
  A container's name is random, so unlike a pid it can't be another's:
  one still running after whiff died is ended at start, and its job is
  lost as one that runs no more.

**What it doesn't cover.** With the network on, a command reaches the
internet and, under Podman, the host's servers that listen on every
interface. What a command writes in its directory is the agent's work, as
its file tools' is: the review gate and git cover that, not the sandbox,
and a repository's own hooks or config written there run on the host
when you use git in it. whiff's own git in a pipeline run's worktree
doesn't follow its `.git`, which the developer can rewrite to name a
gitdir of its own: it uses the gitdir whiff made with the worktree. A worktree's command can read all of its
repository's `.git`: other branches, and its config, with any
credential written in a remote's URL. huff's own calls to its provider leave from the host, by design.
Agents on `claude -p` aren't sandboxed: their shell still reaches the
socket.

## Peers

Another machine's whiff, a VPS say, is a peer: this whiff reaches it as
a client, in the protocol it already speaks, by running `ssh <host>
whiff bridge`, which connects ssh's stdin and stdout to that host's
daemon, starting it if need be. No listener and no new protocol: what
reaches the peer is what `whiff call` would send there, as you.

- **Peers are kept in whiff.db.** `whiff peer add vps whiff@host` (with
  `--whiff <path>` if ssh doesn't find whiff there, and `--dir <path>`
  for where its calls run), `whiff peer ls`, `whiff peer rm vps`; or
  `:peers/add`, `/list`, `/view` and `/rm`, whiff's own address. Adding
  and removing one is yours only: an agent's call is refused.
- **A peer stays connected.** A goroutine of its own connects it and
  connects it again whenever the connection goes, waiting a second, then
  twice as long each time up to a minute. `whiff peer ls` says whether
  each is connected, connecting or offline, when it was last seen, and
  why it isn't connected: what ssh said, as "Permission denied", or that
  the two whiffs speak different protocols.
- **This whiff drives.** A peer only answers what it is asked, so it
  can't act as you here. ssh doesn't ask for a password, as no one is
  there to type one: the peer needs your key.

- **A peer's address is `:address@peer`.** `(call :pipeline@vps/build
  ...)`, `whiff call pipeline@vps/build`, `send`, and `pub` to
  `:git@vps/changed` go to the peer: the peer's name after the address,
  before the last `/`. One hop only: a peer's peers aren't
  reached through it. A peer that is offline answers `(err {:kind
  :offline})` at once, saying why; one connecting, or that was connected a
  moment ago, is waited for up to 15 seconds, on top of the call's own
  time. A connection that doesn't answer within a minute, as ssh waiting
  for an agent's approval, is given up and tried again.
- **It goes in its origin.** An agent's message reaches the peer as an
  agent's, so the peer's review gate holds a write for review there, as it
  would one sent on that machine; one you let through here goes on as
  yours. An agent session's `:can` names a far function as it does any
  other, `:pipeline@vps/build`. A message from a `:read` function, as a
  view, can't reach a `:write` function there either. It comes back in
  its origin too: a task an agent answers there, or a job with `:origin
  :agent` ends, is an agent's here, so what a service does with it is
  held as an agent's would be.
- **It runs in the peer's dir,** `--dir`, never in the laptop's
  directory, or in `whiff call -C <dir>`'s, relative to it.
- **A task there is a task here.** A message that begins a task on the
  peer, an ask or a held write, gets a task here, `(err {:kind :pending
  :id n})`, which waits in `:pipeline@vps`'s inbox and is stored: `whiff
  wait n` waits for it, a service gets its `whiff/done`, and cancelling
  it cancels the one there. whiff follows the peer's `:tasks/changed`,
  checks each task again as it connects, after a restart too, and every
  five minutes should an event have been dropped.
- **A message runs there once.** Each goes with a submission key; if the
  connection goes before the peer answers, whiff sends it again with the
  same key once it is back, and the peer answers with the first one's
  result, kept a day, rather than running it twice.
- **What comes back is made far.** An `:action` in a view from the peer,
  `[:review/accept 3]`, comes back as `[:review@vps/accept 3]`, so
  `whiff act` and sniff act on the peer.

- **What happens there is in history here.** Each entry of the peer's
  history comes as it is recorded, its addresses far (`client ->
  :box@vps/put 1`) and its message given an id here, so `whiff history`,
  its queries and sniff show both machines. whiff keeps its place in the
  peer's history, so what happened there while it was away comes once as
  it connects again, while the peer still holds it, even after the peer
  is removed and added again.
- **What it publishes is published here.** `:pipeline/changed` there is
  `:pipeline@vps/changed` here, so a service follows it with `(on
  :pipeline@vps/changed (v) ...)`, and a sniff page of the peer's view,
  `/view/pipeline@vps/show`, keeps up with it. A page fetches a peer's
  function only if the peer says it is `:read`.
- **Values travel as data.** Results, Options, times to the nanosecond,
  durations, nested maps and views go to a peer and come back as they
  went, as a call's arguments, its result, or a task's kept across a
  restart; only a view's action targets change, made far.
- **Its queues are yours here.** `:review/pending` and `:me/inbox` have
  each connected peer's items after this whiff's, each label after the
  peer's name and its actions far, so sniff's Review and Inbox, and their
  counts, are every machine's; a view that asks a peer for its part
  leaves that peer's own peers out. `whiff pending` and `whiff inbox` list
  each connected peer's review queue and `:me` inbox after this whiff's,
  each id with the peer's name, `12@vps`; `whiff inbox me@vps` lists only
  that peer's. `whiff accept`, `reject`, `reply` and `cancel` take such
  an id and act there, as the peer's client, so the peer decides as it
  would on its own: an agent can't accept there either. A peer that
  isn't connected says its queue isn't shown.
- **sniff shows where things run.** A peer's addresses are in a band
  of their own on the map, one per peer, apart from what runs here; what
  its clients send is from "you on vps", `client@vps` in history, and
  `peer:vps` filters history to it. A peer that isn't connected is a pill
  in the status strip.
- **Stale is said.** A peer that isn't connected shows in `whiff
  services` and `whiff peer ls` with when it was last seen and how old the
  last of its history here is.

A peer's connection carries one request at a time, so a slow call there
holds up the next.

## History

Every message is an entry: when, kind, origin, sender, target, arguments,
reply, time taken, and for an agent's write how the gate treated it. Notes
record whiff's own events: a service that failed to load or faulted, a mode
set or run out. An entry names the one that led to it, which `whiff history
-c` shows as chains, and sniff as a card per request, from you or an
agent, with how it stands and its steps, and the background between them
(timers, reads, settles, notes) folded into one row. A card shows its
chain as steps, as lanes (an address a lane, an arrow a message), or as
every message; sniff's Map draws the same messages as boxes and arrows,
who talks to whom and how often. What an agent's session sends is caused
by the message that began the job it runs in, as an agent's turn's
`jobs/run`, so it is in the chain it is work for. A timer's message begins a chain of its own,
keeping its origin, or a service that schedules itself would be one endless
chain. A chain is in a **project** once a job in it runs in a git
repository: the repository's folder name, a worktree's being its
repository's. Its messages from then on carry it, as does the session of
an agent's turn run there, so `project:tally` finds them and sniff marks
each card with its project, a click showing only that project's. On
the Map each project is a band, holding the addresses two thirds of
whose messages are its work, under a shared band of what several use;
a band folds into one box, and an arrow from one project's band into
another's is drawn dashed, as work that crosses projects. A handler asks
its chain's project with `(project/name)`, and that of a directory with
`(project/name dir)`; `services/board.puff` puts tickets, plans' tasks and
pipeline runs each in its project so.
History is stored in whiff.db for 30 days; clients get the last
1000 entries, and `whiff history -f` and sniff follow it live, with
nothing missed between the snapshot and the stream. `whiff history` with
a query looks through all 30 days. Whatever else whiff.db holds is in
`whiff dump <bucket>` (`whiff dump` lists its buckets), its key then its
value as puff: an entry a line when piped, for grep, and on a terminal
broken over lines to fit, as `whiff state` is.

## Limits, for now

- History, service state, every inbox, `:review`'s queue too, timers,
  running jobs and results are stored. What a `whiff try` address holds
  isn't. History is stored about 100ms after it happens, in batches, so a
  message answered at once never waits for the disk: a crash loses that
  last moment of it, and if whiff.db falls 10000 entries behind, what
  doesn't fit isn't stored, and the log says so. A task does wait: it is
  committed as it begins, in an inbox or the review queue, and again as it
  ends, before anyone hears of either, so routing pauses for those two
  commits (ids are reserved 256 at a time, so taking one rarely adds a
  third). It's kept 30 days, deleted at start, and
  whiff.db is compacted then if a quarter of it is free. Clients get the
  last 1000 entries.
- A job isn't resumed. When whiff stops it stops the job, as a cancel or
  timeout does: SIGTERM to its process group, then SIGKILL for what's left
  after 3s, so a server shuts down cleanly. At the next start its sender
  gets `(err {:kind :interrupted :outcome :stopped})`, or the job's real
  answer if it finished just then. After a crash whiff can't
  know how far a job got: `:outcome :unknown`, with its `:pid` if that
  process group may still run. whiff leaves it, as a pid can be reused, so
  you may have one to kill; `:jobs/process id` says while it may still run,
  and pipeline's Retry refuses until it doesn't, so two devs never share a
  worktree. Neither outcome undoes what the job did. A job
  with `:retry n` runs again instead, under its id, up to n times, after a
  timeout, a stop or a crash, but only if it is `:read` or says
  `:idempotent #t`, and never while its process group may still run, nor
  after you cancel it. It waits 1s before its second attempt, twice as
  long before each after, up to a minute, so a failure that passes can;
  a cancel meanwhile ends it. Its record lists each attempt.
- A `send-after` survives a restart with its origin, so an agent's still meets
  the review gate, and one that came due while whiff was down fires once when
  the services are back. A service's timers to itself go with the version
  that set them: a reload drops them, and so does a restart if the service
  has a `:start`, which sets the ones it needs, so a loop doesn't double. A
  one-off timer a service with a `:start` set for itself goes too, and
  history says how many. `:timers`, whiff's own address, lists the timers
  waiting (`list`, and `view` with Cancel, under ⌘K in sniff), and
  `cancel id`, yours, deletes one: a timer fires only while it is stored.
- A message that waited through a restart keeps its origin, sender,
  session and chain, as ids, message ids in history and agent numbers are
  never given out twice. Its `:expires` counts from when it arrived. What waits for an address that
  isn't loaded stays until it is, or until you reply, and history says so.
- A message leaves its stored inbox in the transaction that stores its
  answer, whether a reply, a reject, a cancel or its expiry: if whiff dies
  before that, it is back in the inbox. A timer that fired stays stored
  until its message has been handled, in the transaction that commits the
  service's new state if it has one, as a result does: if whiff dies first,
  it fires again at start. So a stateful service's state sees it once, but a
  handler's effects outside its state can happen twice. A timer for an ask,
  or one the review gate holds, ends as it reaches the inbox. A
  message you accept is stored as accepted as it leaves the review queue,
  so if whiff dies before it ends, its sender hears `(err {:kind
  :interrupted :outcome :unknown})`. A job you accept keeps the id its
  sender already has.
- A call waits 5s; anything longer is a job or an ask.
- No calendar maths in puff: `1d` is 24 hours.
