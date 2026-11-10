# Agents in whiff

Agents use the same addresses, functions and messages as other clients.
There are two ways to use them: connect an agent you already run over
MCP, or give an agent a home and mailbox in whiff.

## Choose how the agent runs

| | MCP client | Resident agent |
| --- | --- | --- |
| Starts the model | Your MCP client | whiff, through the Claude CLI, or huff for other providers' models |
| Session address | `:agent/<n>` | `:agent/<name>` for its tools; `:<name>` for its mailbox |
| Tools | Exposed address functions, except those reserved for you; eval, wait, cancel and inbox | Functions in `:can`, plus its own `later` |
| Conversation | Managed by your client | Kept in the agent's home across turns |
| Model spending | Managed by your client | Agent `:limits`, with reported delegated costs |

Both meet the review gate. A resident agent's allowed tools and trust
mode are separate decisions: `:can` says which functions it may reach;
trust says which writes may proceed without individual review.

## Connect an MCP client

For Claude Code:

```sh
claude mcp add whiff -- whiff mcp
```

Each session gets an address such as `:agent/1`. Service functions become
tools, with their docs and `:read` or `:write` marks. Arguments are strings
containing puff values: `"12"` as a tool argument represents the int 12;
a puff string containing those digits is written as `"\"12\""` in JSON.
The tool descriptions show their arguments.

An MCP client should use its tools to call whiff. Running the normal
`whiff` CLI through a shell acts as you and has your permissions.

## Give an agent a mailbox

A resident agent on a Claude model needs the Claude CLI installed and
authenticated, with `claude` and `whiff` on the daemon's `PATH`; one on
another provider's model needs `huff` there and that provider's key (see
[On other models, with huff](#on-other-models-with-huff)). The included
`assistant` runs on Kimi K2 through OpenRouter. Create
`~/.whiff/services/helper.puff`:

```lisp
(agent helper
  :prompt "Help the user track work. Read open tasks before answering."
  :model "sonnet"
  :can [:todo/open :todo/add]
  :limits {:turn 0.5 :day 2 :time 5m :tasks 3})
```

This example needs the [todo service](../services/README.md). Its tools
operate in `:dir` if supplied; without one they operate in the agent's
home. Add an absolute `:dir` pointing at the repository containing your
`TODO.md` before using it for project work. Relative `:dir` paths resolve
from the agent file's directory, following symlinks.

```sh
whiff call helper/tell "What is still open?"
whiff inbox
whiff reply <message-id> "Add a task to write the release notes."
```

`tell` queues a message. When its turn finishes, the answer lands in your
inbox; replying continues the same conversation. `ask` queues work whose
answer belongs to the caller instead:

```sh
whiff call helper/ask "Summarize the open work."
# pending <id>
whiff wait <id>
```

An agent takes one model turn at a time. Messages received while it works
wait in its inbox. A turn runs as a job, with the same Claude conversation
resumed for later turns. Conversation and accounting files live in
`~/.whiff/agents/helper/`; durable messages and task results live in
`~/.whiff/whiff.db`.

### On other models, with huff

An agent whose `:model` names its provider runs its turns on `huff`,
whiff's own harness, instead of the Claude CLI:

```lisp
(agent helper
  :prompt "Help the user track work."
  :model "openrouter/moonshotai/kimi-k2"
  :can [:todo/open :todo/add])
```

| `:model` | Provider | Key, in the daemon's environment |
| --- | --- | --- |
| `openrouter/<model>` | OpenRouter (Kimi, GLM, DeepSeek, ...) | `OPENROUTER_API_KEY` |
| `openai/<model>` | OpenAI | `OPENAI_API_KEY` |
| `local/<model>` | A server that speaks Chat Completions, at `WHIFF_LOCAL_URL` | none; Ollama's `http://localhost:11434/v1` if unset |

It bills per token to those keys, not to a Claude subscription. Its tools
are the same `:can` functions, called as the agent's session, so writes
meet the review gate and history has each call. The conversation is kept
in `~/.whiff/agents/<name>/huff-<session>.puff`, puff data you can read.
Once a model call would send about 80,000 tokens (`huff -compact`), the
model summarizes the older part of the conversation and the summary takes
its place, so a long-lived agent stays cheap and within its model's
context; the transcript as it was is kept beside it, as
`huff-<session>.before-<time>.puff`, and the turn's steps say
"compacted". The summary's cost is the turn's.
A turn stops at 20 model calls, or at `:turn` dollars where the provider
reports cost, as OpenRouter does. A turn whose daemon dies stops within
a second, dropping the call it waits on, rather than going on, billed,
for nobody; its conversation is kept. `huff` needs to be on the daemon's
`PATH`, as `go install ./cmd/...` puts it.

The pipeline's developers and reviewers, plan's planner and the
reviewer service run on huff the same way, when their model names a
provider: a team set to
`{:dev "openrouter/moonshotai/kimi-k2" :reviewer "openrouter/z-ai/glm-4.6"}`
builds its runs without Claude. A developer gets huff's file tools,
named and used as Claude's (Read, Write, Edit, Glob, Grep and Bash),
acting only in its worktree and writing nothing in `.git`, whose hooks
and config git runs on the host, and Bash runs only what its Claude could:
the check and go vet, go build, gofmt and git status, diff and log,
without a shell. Its conversation is kept in `~/.whiff/huff/`, so a
retry goes on from it. Everything an agent's turn, a developer, a
reviewer or the check prints is kept in `~/.whiff/jobs/<id>/` for 30
days, not only the last 64KiB: `tasks/tree` names the dir, and sniff
says it when you hover a step of the work. With `:sandbox #t` in the team, or in the map
`build` takes, the developer's Bash commands and the check run in a
container that sees only the worktree, without the network unless
`{:net #t}`; a run that asks for it needs its developer on huff.

huff also runs on its own, as `claude -p` does:

```sh
huff -m openrouter/moonshotai/kimi-k2 "a question"
huff -m local/qwen3 -tools Read,Edit,Bash -allow "go test:*" "fix the failing test"
huff -m local/qwen3 -tools Read,Edit,Bash -allow "go test:*" -sandbox "fix the failing test"
```

With `-sandbox`, Bash's commands run in a container that sees only the
directory, without the network unless `-net`; `whiff sandbox build`
makes its image, and `whiff sandbox check` says what's missing.

## Review writes or grant trust

Reads go through. A gated write waits for you only while someone follows
the review queue. Open sniff, or leave this running in another terminal:

```sh
whiff sub review/pending
```

Then inspect and decide:

```sh
whiff pending
whiff accept <id>
whiff reject <id> "Please explain the change first."
```

Without a reviewer, a gated write fails with `:no-reviewer` rather than
waiting unseen. To let a session write to a service without each review:

```sh
whiff trust agent/helper todo --for 1h
whiff session agent/helper
whiff gate agent/helper
```

Trust applies to the named services and the work they do for the accepted
message. `whiff free agent/helper --for 1h` allows all its writes, still
within its capabilities. `gate` restores individual review. A timed mode
expires automatically; an untimed resident agent's mode lasts across
turns and daemon restarts until changed or its file is removed.

Origins follow message chains: a service doing work for an agent doesn't
turn that work into your own. Accepting a held message permits the work
it causes. History records whether a write was held, accepted, trusted
or free. See the [review gate design](design.md#origins-and-the-review-gate)
for the rules and the boundary with other tools and OS permissions.

## Delegation and scheduled turns

Adding `:helper/ask` to another agent's `:can` lets it delegate to helper.
An agent ends its turn after starting asynchronous work; it doesn't need
to poll for the answer. Completed results wait in its inbox as
`whiff/result`. The tasks started by one turn are collected into one
result turn once all have ended. An ask's final answer waits for work
its turns delegated, rather than returning an interim "I've asked".

Each child has its own capabilities and review rules; a parent's trust
mode isn't inherited. Pausing a parent lets its children continue.
Cancelling the agent cancels its outstanding tasks, and the services
handling them are notified so they can stop the work they started.

`:every 1d` in the agent file schedules a turn every 24 hours. The agent
also has a `later` tool to schedule a note for a later turn. Scheduled
turns report to your inbox only when they have something to say. Paused
agents keep messages waiting, rather than accumulating repeated wakes.
Inspect schedules with `whiff call timers/view`.

## Budgets and controls

| Limit | Default | Meaning |
| --- | --- | --- |
| `:turn` | $1 | Model budget for one turn |
| `:time` | 10 minutes | Time allowed for its turn job |
| `:day` | $5 | Spending over the last 24 hours, including costs reported by delegated tasks |
| `:tasks` | 10 | Outstanding tasks its session may have at once |

The day limit checks recorded spending before another turn; it doesn't
reserve future costs for work already running. Delegated costs depend on
what results report, such as a pipeline build's `:cost`. A board project's
`:day` is a separate admission check over its pipeline runs and the
lead's turns for that project; `usage` says the latter as `:projects`. See the
[team example](use-cases.md#a-team-of-agents-on-a-project).

```sh
whiff services helper
whiff call helper/usage
whiff inbox helper
whiff history -c from:agent/helper
```

Use the task tree linked from the agent's Status in sniff to follow
delegated work. A tool call can have finished while the agent is waiting
for another task from the same turn.

| Control | Effect |
| --- | --- |
| `whiff call helper/pause` | Let the current turn finish; hold subsequent turns |
| `whiff call helper/resume` | Take queued messages again |
| `whiff call helper/stop` | Stop the current turn; delegated work can continue |
| `whiff call helper/cancel` | Pause, stop the current turn and cancel outstanding delegated tasks |
| `whiff call helper/restart <turn-id>` | Start a new turn with a failed turn's message, failure and task context |
| `whiff call helper/forget` | Start a new conversation on the next message |

`forget` isn't a cancellation or budget reset. A failed turn isn't
blindly replayed after it ran, since its tools may have written. Use its
Restart action after inspecting the work; retry a failed delegated build
through the pipeline's own Retry action. The
[troubleshooting guide](troubleshooting.md) covers stuck queues, budgets
and recovery after a crash.
