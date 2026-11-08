# Troubleshooting

Start with what whiff knows about the service and the messages it handled:

```sh
whiff services
whiff services <service>
whiff history -c
sniff -open
```

`whiff services` puts problems first. A service's details show its file,
load error, recent fault, inbox and timers; an agent also shows turns,
spending and delegated work. sniff's Status offers the same information
and actions. `<service>` and `<id>` below are placeholders to replace.

## A service doesn't load, or edits don't take effect

Check `whiff services <service>` for a `file:line:col` error. Services load
from `~/.whiff/services/`, and the name in `(service name ...)` or
`(agent name ...)` must match the file's name without `.puff`.

An invalid edit leaves the previous version running. Fix the source and
save it; whiff watches the directory twice a second. If you linked a
service, edit its target. A `(use "...")` path is relative to the source
file's real directory, not the services directory.

```sh
puff -f path/to/service.puff
whiff test path/to/service.puff
whiff ls
```

Formatting doesn't check that names and calls are valid. `whiff test`
loads the service under a scratch address without its `:start`, checks
formatting, and runs inline tests. Tests run as you and may have effects.
`whiff ls` shows the functions in the version currently loaded.

## A handler faults repeatedly

Read the fault and its source location in the service's details. Common
causes are an unmatched pattern, a missing value passed to `unwrap`, or
an error discarded between expressions. Handle expected failures with
`match`, `try`, `catch` or `or-else`; see [Learn puff](puff.md).

A fault restarts the service from its file and last committed state.
Editing `:state` doesn't replace stored state. If the code expects a
different state shape, make it accept or migrate the stored value.
`whiff reset <service>` deliberately discards that service's saved state
and reloads its initial state; use it only when you intend to lose that
state. It isn't the usual fix for a code error.

## An agent's write doesn't proceed

```sh
whiff pending
whiff session agent/<name-or-number>
whiff ls
```

A `:held` message needs an accept or reject, or a trust mode that allows
it. The session command shows how writes were treated; `ls` shows marks
and explicit `:read` overrides.

`:no-reviewer` means nobody was watching for approvals when the write
was attempted. Open sniff or run `whiff sub review/pending` before trying
again. For a resident agent, you can grant a service-specific trust mode:
`whiff trust agent/helper todo --for 1h`. Trust doesn't add functions to
its `:can`, and it doesn't automatically trust child sessions. See
[Agents](agents.md#review-writes-or-grant-trust).

## An agent doesn't answer

```sh
whiff services helper
whiff inbox helper
whiff call helper/usage
whiff call tasks/view
```

Check whether it is paused, over budget, running a turn or waiting on
delegated work. A paused agent needs `helper/resume`. A busy one may be
waiting for every task started in one turn to finish before taking their
results as another turn. Inspect the task tree in sniff rather than
submitting the same work again.

For `:over-budget`, inspect spending and `:limits` in the agent file. The
day is a rolling 24 hours, not a midnight reset. Wait for spending to age
out, or deliberately raise the limit and save the file. Delegated task
costs count too; `forget` doesn't clear them. Spending is kept in
`~/.whiff/agents/<name>/costs`, not whiff.db, so removing whiff.db
doesn't clear it either.

If the turn couldn't start, check that the Claude CLI is installed and
authenticated and that the daemon can find `claude` and `whiff` on its
`PATH`. A daemon started before a shell environment change keeps its old
environment. Restart it with `whiff stop`, then run a whiff command from
the intended environment; stopping also stops its jobs.

For a turn that failed after starting, inspect the error and work already
done. Its Restart action, or `whiff call helper/restart <turn-id>`, starts
a new turn with that context. Use `helper/cancel` to pause and cancel its
outstanding work, or `helper/stop` to stop only the current turn.

## A task stays pending

```sh
whiff call tasks/view
whiff call tasks/tree <id>
whiff wait <id>
```

| Status | What to inspect |
| --- | --- |
| `:held` | Your review queue |
| `:waiting` | The target's inbox; it may need your reply, an agent turn, or child work |
| `:running` | The process's output with `whiff call jobs/output <id>` or its Watch action |
| `:ended` | Its saved result and any recovery actions |

`whiff inbox` shows questions addressed to you; answer with
`whiff reply <id> <value>`. For a service waiting on another service, the
task tree shows the work underneath. `whiff cancel <id>` ends held or
waiting work, or stops a running job; it doesn't undo effects already
performed. Agents can cancel only their own session's tasks.

`:tasks/list` and its view show open tasks plus up to 50 recent ended
tasks from the last day. Results remain available through `whiff wait`
for 30 days. An older result can therefore be available even when it
isn't in the task list. After retention expires, `:no-id` is expected.

## Work was interrupted by a stop or crash

Read the result's `:outcome` before retrying:

| Outcome | Meaning |
| --- | --- |
| `:not-started` | A held message wasn't delivered, or a waiting ask was removed; an ask may already have child work |
| `:stopped` | The process was stopped; its earlier effects remain |
| `:unknown` | whiff can't establish how far it got; its process group may still run |

`whiff call jobs/process <id>` checks whether a recorded process group
may still run. Inspect it before starting conflicting work. A pipeline
Retry refuses while its old group may still run, so two developers
don't share the worktree. Use the result's offered Retry action after
resolving the cause. Jobs with safe retry options can retry automatically;
the rules are in [Tasks](tasks.md#attempts).

For procs, use `procs/runs` and `procs/logs`. A `:stray` process needs
inspection; the inbox message identifies the group. A `:queued` process
is waiting for its dependencies, and `:unready` means its readiness
probe hasn't passed. The [process guide](procs.md) explains recovery and
the difference between a process and a container it started.

## sniff doesn't show the daemon

sniff connects to an existing daemon; it doesn't start one. Run
`whiff services`, then `sniff -open`. Plain `sniff` prints the URL without
opening a browser. Use the full URL with its token; it is served only on
`127.0.0.1` and checks its Host and request origin.

If a client reports a protocol mismatch, follow its message: stop an old
daemon with `whiff stop` and let the next command start the installed
version. If the client is older, reinstall all four binaries from the
same checkout with `go install ./cmd/...` and restart sniff.

## A command's argument has the wrong type

The CLI reads each argument as a puff value, falling back to a string
when it doesn't parse as one. `12` is an int, `ann` is a string, and
`:ready` is a keyword. To pass the string containing digits, include puff
quotes inside shell quotes:

```sh
whiff call greet/hello '"12"'
```

Quote maps and vectors as one shell argument, for example
`'["go" "test" "./..."]'`. A type error identifies the function and
source location. Use [the function reference](stdlib.md) and `whiff ls`
to check the expected arguments.
