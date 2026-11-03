# whiff in use

Things whiff is for, from small to big, each with the commands and what goes
on underneath. The services are the ones in [services/](../services/README.md);
[design.md](design.md) explains the machinery.

## A dev pipeline: a planner, a developer and a reviewer

You give a goal; a planner Claude splits it into tasks; for each task a dev
Claude writes the code in a worktree of its own, whiff runs your tests, and
a reviewer Claude reviews the diff. Until the tests pass and the reviewer
approves, the review goes back to the dev, up to three rounds. Each task
starts from the branch of the one before, so the last branch holds the
whole goal. Your own working tree is never touched.

Install the two services and start a plan in your repository:

```sh
ln -s "$PWD"/services/pipeline.puff "$PWD"/services/plan.puff ~/.whiff/services/
cd ~/src/todo
whiff call plan/start "A small todo CLI in Go ..." '["go" "test" "./..."]'
```

A real run, on an empty Go module, with the goal "A small todo CLI in Go,
run as 'todo': 'todo add <text>' adds an item, 'todo list' lists them
numbered with [ ] or [x], 'todo done <n>' ticks one. Items are kept in
todo.json in the current directory. Standard library only, with tests."
The planner read the repository and answered three tasks, each a paragraph
that says what to build and what to test:

```sh
$ whiff call plan/plans
[{:goal "A small todo CLI in Go ..." :step :done :task 2 :run 3
  :tasks ["Create the storage layer in package main at the repo root ..."
          "Add the pure list operations in todo.go (package main, no file I/O) ..."
          "Wire up the CLI in main.go ..."] ...}]
```

Each task took one round: the tests passed and the reviewer approved, with
a review like this one (abridged), ending in its verdict:

```
I found no real problems. The diff does everything the task asks:
- store.go: Load returns []Item{} with a nil error when the file is missing,
  which it checks with errors.Is(err, fs.ErrNotExist) ...
- store_test.go: all four required cases are there and use t.TempDir() ...
One optional point, not a blocker: no test checks that the output is
actually indented, but the task didn't ask for one. The check passed.

VERDICT: approve
```

Four minutes after `plan/start`, one message in your inbox:

```sh
$ whiff inbox
11  :plan -> :me/tell "plan 1, A small todo CLI in Go ...: done. All 3 tasks built; whiff/3 has them all."
$ git log --oneline whiff/3
2507c9d Wire up the CLI in main.go
54dedfe Add the pure list operations in todo.go (package main, no file I/O)
d5cd805 Create the storage layer in package main at the repo root (module examp…
274723f Start the todo CLI
$ git diff --stat main whiff/3
 7 files changed, 478 insertions(+), 1 deletion(-)
$ cd ~/.whiff/pipelines/todo-demo-3 && go build -o todo . && ./todo add buy milk && ./todo add call mom && ./todo done 2 && ./todo list
added 1. buy milk
added 2. call mom
done 2. call mom
1. [ ] buy milk
2. [x] call mom
```

The subjects in that log are shortened here: that run's commits had the
whole task as their subject. Commits now get the task's first sentence as
their subject, as shown, and the whole task in their body.

Or one task, without the planner:

```sh
whiff call pipeline/start "add a --verbose flag" '["go" "test" "./..."]'
whiff call pipeline/view | fzf | whiff act    # cancel a run
```

What happens underneath, for one task:

```
you ──call──> :plan/start ──job──> claude -p (planner: reads, can't edit)
                    │ whiff/done: ["task 1", "task 2", ...]
                    └──call──> :pipeline/start-on repo base "task 1" check
                                    │ git worktree add -b whiff/1
                                    ├──job──> claude -p (dev: edits, runs the check)
                                    ├──job──> go test ./...         (whiff runs it)
                                    ├──job──> claude -p (reviewer: no tools, gets the diff)
                                    │   └─ changes, or the check failed: dev again, same session
                                    └─ approved and passing: git commit on whiff/1
                                         publish :pipeline/done ──> :plan: task 2 from whiff/1
```

Everything is a message, so `whiff history -f` (or sniff) shows the whole
conversation as it happens, and each step is a job you can see with `whiff
call jobs/list` and kill with `jobs/cancel`. The runs and plans are service
state, so `pipeline/runs` and `plan/plans` say where each one is.

Why it is built this way:

- **The steps are code, the Claudes' output is data.** `pipeline.puff`
  decides what runs next; the dev's summary and the reviewer's review are
  text it passes on. Their jobs run as yours, not `:agent`'s: otherwise
  every step after the first would wait in your review queue, for changes
  that stay on a throwaway branch until you merge it.
- **Your check decides, not the dev.** whiff runs the tests itself; a review
  that approves failing code goes back to the dev.
- **What each Claude can do is the job's command line:** the planner reads
  (`--tools Read,Glob,Grep`), the dev edits and runs only the check
  (`--allowedTools "Bash(go test ./...)"`), the reviewer has no tools, and
  none of them loads the MCP servers of your own Claude setup
  (`--strict-mcp-config`): the first real run's dev found hiss's and tried
  to propose its edits there.
  whiff decides when and where they run, not what they do to files: the
  worktree is the fence.

Where this is going: a planner that fans tasks out in parallel and merges
them, picks reviewers by what changed, or starts whatever services it needs
as it goes. The pieces are the same (jobs, `whiff/done`, `publish`, state);
this is the first shape.

## Your TODO list from anywhere

`todo` reads the checkbox items of `TODO.md` in the directory you call it
from.

```sh
whiff call todo/open
whiff call todo/add "write the docs"
whiff call todo/view | fzf | whiff act    # tick the one you pick
```

`todo/view` answers with a view, items each with an action, which the CLI
prints as lines for fzf:

```
first thing	TODO.md:1	[:todo/done 1]
second thing	TODO.md:3  Later	[:todo/done 3]
```

`whiff act` sends the action at the end of the line you picked,
`[:todo/done 3]`, as a call from you, in your directory.

## Watching your pull requests

```sh
cd ~/src/whiff && whiff call gh/watch
whiff sub gh/changed
```

`gh` keeps the open pull requests of every repository it watches in its
state, and every 5 minutes it looks again (a `send-after` to itself) and
publishes `:gh/changed {:dir :prs}` for each that changed. Anything can
subscribe: `whiff sub`, hiss for a toast, or another service, `(on
:gh/changed (v) ...)`. The state survives restarts; `gh/unwatch` stops.

## Your dev servers, without a terminal each

The terminals you keep open for a project, a `go run` server or two and a
database in a container, as processes `procs` runs for you. Copy the
service, as you'll edit it, and name your processes in its `procs`, per
project, in the order they start:

```lisp
(def procs [:shop/db {:cmd ["docker" "run" "--rm" "--name" "shop-db" "-p" "5432:5432"
                            "-e" "POSTGRES_PASSWORD=dev" "postgres:17"]
                      :ready ["docker" "exec" "shop-db" "pg_isready" "-U" "postgres"]}
            :shop/api {:cmd ["go" "run" "./cmd/api"] :dir "~/src/shop" :restart #t
                       :ready "http://localhost:8080/health" :after [:shop/db]}
            :shop/web {:cmd ["go" "run" "./cmd/web"] :dir "~/src/shop" :after [:shop/api]}])
```

```sh
cp services/procs.puff ~/.whiff/services/
whiff call procs/up :shop               # db, api, then web
whiff call procs/view | fzf | whiff act # Stop, Restart or Logs, or Start
whiff call procs/logs :shop/api         # its output so far
curl localhost:8080/health              # each binds its own port
whiff call procs/down :shop
```

A process with `:ready` is `:starting` until its URL answers 2xx or its
command exits 0, and one with `:after` waits until those it comes after
are ready. Stopping one sends its process group SIGTERM, then SIGKILL
after 3s. Starting whiff brings back the ones you wanted up; with
`:restart #t` one that exits starts again. `up`, `down` and `restart` are
`:write`, so an agent's calls to them wait for your review; `runs`, `view`
and `logs` are `:read`.

Because the processes are whiff's, another service can bring the app up
and test it: [`examples/shop`](../examples/shop) has a Go API on postgres
and redis, a whiff service that runs an integration test once it is all
ready and tells you how it went, and the same test as a plain puff
script for CI. [procs.md](procs.md) has the table's fields, how a
process goes, crashes and strays, and that example's real run.

## Work that answers later

```sh
whiff call reviewer/review     # pending 12 (whiff wait 12)
whiff wait 12
```

`reviewer` runs `claude -p` as a job, with the diff on stdin and no tools,
and hands the job to you (`hand-on`): you get `pending` at once and the
review when it is done. Any long work fits this shape: a build, a scrape, a
model's turn.

## A resident assistant

```sh
whiff call assistant/tell "the release is on Friday; remind me what's left"
whiff inbox                       # its answer, from :assistant
whiff reply 7 "and the docs?"     # the next turn of the same conversation
```

Each message wakes Claude for one turn of one long session, days apart if
need be; its answer waits in your inbox, `:me`'s, and your reply is the next
turn. `assistant.puff` is one form, and another agent is another file:

```lisp
(agent researcher
  :prompt "Follow the BIP-360 discussion for the user."
  :model "opus"
  :can [:gh/prs :todo/add]
  :every 1d)
```

`:can` gives its turns those functions as tools: it reads pull requests
itself, and its `todo/add` waits for your review like any agent's write,
unless you `whiff trust agent/researcher todo`. Without `:can` it has no
tools. Its turns cost at most `:limits`, $1 each and $5 a day unless it
says otherwise; `whiff call researcher/usage` says what it spent, and
`researcher/stop` ends a turn that runs away.

`:every` gives it a turn of its own each day, which reaches your inbox only
if it has something to say. A service uses an agent with `ask`, whose
answer comes back to it rather than to your inbox; asks wait their turn in
the agent's inbox (`whiff inbox researcher`):

```lisp
(on (summarize) "Asks the researcher for a summary."
  (submitted (call :researcher/ask "summarize the thread so far")))
(on (whiff/done id r) "Its answer."
  (match r [(ok text) (call :me/tell text)] [e e]))
```

## Services that ask you

A service that needs a decision asks `:me`, your own address:

```lisp
(on (deploy) "Asks before deploying."
  (call :me/ask "deploy now?"))
(on (whiff/done id r) "Your answer."
  (match r [(ok "yes") (call :jobs/run ["make" "deploy"])] [_ (ok :skipped)]))
```

`(call :me/ask ...)` answers `pending` at once; the question waits in `whiff
inbox` until you `whiff reply 9 yes`, and your answer comes back to the
service as `[:whiff/done 9 (ok "yes")]`. Nothing blocks while it waits.

## Letting an agent work through whiff

```sh
claude mcp add whiff -- whiff mcp
whiff sub review/pending          # you review: follow the queue
```

The agent's session gets an address, `:agent/1`, every service function as
a tool, and `eval`, `wait` and `inbox`. What it reads goes straight through;
what it writes waits for you:

```sh
whiff pending                     # what waits
whiff accept 5                    # or: whiff reject 5 "not that file"
```

When that is too many reviews, let the session through for a while, and
read what it did at the end:

```sh
whiff trust agent/1 todo gh       # its messages to :todo and :gh go through
whiff free agent/1 --for 1h       # everything goes through, for an hour
whiff session agent/1             # every write, and how it got through
```

The gate covers what reaches whiff: an agent that also has a shell can run
`whiff` as you. See [design.md](design.md#origins-and-the-review-gate).

## Writing your own service

```lisp
; ~/src/notes.puff
(service notes :state []
  (on (add state text) :write :update "Adds a note."
    (ok [:added (vector/append state text)]))
  (on (list state) "The notes."
    (ok state))
  (on (view state) "The notes, as items."
    (ok (view/items (vector/map state (fn (n) {:label n})))))
  (test "a note is added at the end"
    (= (add ["a"] "b") (ok [:added ["a" "b"]]))))
```

```sh
whiff test notes.puff             # 1 passed, 0 failed
whiff try notes.puff list         # load it once, call list, unload it
ln -s "$PWD"/notes.puff ~/.whiff/services/
whiff call notes/add "buy milk"
whiff ls                          # its functions and their marks
whiff services                    # how each service is: failed, waiting for you, ok...
whiff services notes              # one in full: file, last message, faults, timers
```

Saving the file reloads it; a fault restarts it from the file; a file that
doesn't load keeps the old version running and says why, at file:line:col.
Its state is in `~/.whiff/whiff.db`, committed before each reply. `add` is
`:write`, so an agent's call to it waits for your review; `list` is `:read`.

## Seeing what happened

```sh
whiff history -f                  # every message as it happens
whiff history | grep agent        # what agents did
sniff                             # the same in a browser, a column per address
```

Every message is in history with its origin, sender, target, reply and time
taken, and for an agent's write how the review gate treated it.
