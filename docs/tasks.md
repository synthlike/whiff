# Tasks

The spec for v0.6's durable work. Most of it isn't built: "Today" says what
is. Once built, what's here moves into [design.md](design.md).

## The idea

whiff already has one shape for work that answers later. A job, an ask and a
message the review gate holds all give a `ticket`: an id, and where the answer
goes. A **task** is that ticket made durable. There's no new thing to submit
and no new id: the `:id` in a pending is the task's id, from submission to
result, across restarts and retries.

A task records what it is, who owns it, its status, its attempts and its
result. A plan or pipeline run stays the service's own: whiff tracks the job a
run is waiting for, not the run.

## Status

```
held ───────accepted──────┐
waiting (an ask) ─────────┤
                          ├──> running ──> ended
submitted ────────────────┘
```

| Status | Means | Reached by |
| --- | --- | --- |
| `:held` | The review gate has it; nothing has happened | an agent's `:write` |
| `:waiting` | An ask waits for its reply | a message to an ask |
| `:running` | A process may be doing the work | a job starting |
| `:ended` | It has its result, which never changes | anything below |

There's no queued status: a job starts when it is accepted. If v0.7 needs a
limit on running jobs, `:queued` joins here, between accepted and running.

A pending means **accepted**, not done, and not successful: whiff has stored
the task and owes its owner a result. Submission fails only as a Result that
isn't pending, like `(err {:kind :start})`, and then there's no task.

Every result carries how it ended:

| Result | How it ended |
| --- | --- |
| `(ok ...)`, or a job's `{:exit n ...}` | It finished |
| `(err {:kind :rejected :outcome :not-started})` | You rejected it |
| `(err {:kind :expired :outcome :not-started})` | Nobody answered in time |
| `(err {:kind :cancelled :outcome :not-started})` | Cancelled before it started |
| `(err {:kind :cancelled :outcome :stopped})` | Cancelled and killed |
| `(err {:kind :cancelled :outcome :unknown})` | Your accept was cancelled while what it let through ran |
| `(err {:kind :timeout :outcome :stopped})` | Timed out and killed |
| `(err {:kind :interrupted :outcome :stopped})` | Killed by `whiff stop` |
| `(err {:kind :interrupted :outcome :unknown})` | whiff died while it ran; has `:pid` if it may still run |

`:outcome` is the one thing to read before acting on a failure:

- `:not-started`: nothing happened, so it's safe to submit again.
- `:stopped`: the process is gone, but what it had done stays done.
- `:unknown`: it may have finished, and may still be running.

None of them says anything was undone. whiff can't undo a command.

## Who gets the result

A task has one **owner**, its sender: a service, or a client. `hand-on` moves
ownership, in one transaction, to the caller of the handler. After the task
ended, as today, the first owner keeps its copy and the new one gets it too.

The result is stored **when the task ends, before anyone is told**.

- **A service** gets `[:whiff/done id result]` until it has handled it. The
  mark that it did commits with the service's new state, in one transaction,
  so a restart can only redeliver a message whose effect on state was lost:
  its state never sees a result twice. A handler's effects outside its state
  can happen twice, and whiff doesn't claim otherwise.
- **A handler that faults** on a result isn't given it again, as it would
  fault again. History says so, and `whiff wait` still has the result.
- **A service that isn't loaded** gets it when it is. Until then it waits, and
  history says so.
- **A client** has nothing to deliver to: `whiff wait id` reads the result
  whenever it asks, from any connection, after any restart.

A result is kept for 30 days after it ended, with history, then deleted at
start. Nothing needs to acknowledge it. Past that, `whiff wait` says
`:no-id`, as it does for an id that never was.

## Attempts

An attempt is one try at a task: when it started, how it ended, its pid. A
task has one until whiff retries it, and a retry keeps the task's id, so a
waiting owner sees one result, the last. whiff retries only a task that asked
to be, and only when it's safe:

| Last attempt | Read | Write |
| --- | --- | --- |
| `:not-started` | Retry | Retry |
| `:stopped`, `:unknown` | Retry | Don't: it needs a person, or a service that can check |

A job says which it is with the `:read` or `:write` mark of its submitter,
unless it's told otherwise. Corrective loops, like a dev who failed the check,
stay in services: to whiff, each of those is a new task.

## Cancelling

`whiff cancel id` works on any task that hasn't ended, and is yours as `:review`
is: an agent cancels only what its own session started.

| Status | What happens | Result |
| --- | --- | --- |
| `:held`, `:waiting` | Removed from its inbox | `:cancelled`, `:not-started` |
| `:running` | Its process group is killed | `:cancelled`, `:stopped` |
| `:ended` | Nothing | `(err {:kind :ended :result r})` to the canceller; the task keeps `r` |

The first transaction to end a task wins. A job that exits as you cancel it
keeps its real result, and a task can't end twice: an answer that comes after
the end is dropped, and history says so.

## Today

| Spec | Today |
| --- | --- |
| Pending means accepted and stored | Yes for jobs, asks and held messages (`tasks` and `inbox` buckets) |
| A result is stored before anyone is told | Yes, in `tasks`, for 30 days: `whiff wait` reads it after a restart |
| A service gets its result until handled | Yes: the mark commits with a stateful service's state; a stateless one's is set once its handler returns |
| One id from held to ended | **No.** A held job's id answers `(err {:kind :pending :id m})`, then `m` answers: two ids, two `whiff/done`s |
| `:outcome` on every failure | Yes |
| Cancelling any task | Only jobs, `:jobs/cancel`, and rejecting what's held |
| Attempts and retries | None; services retry by hand |

`whiff try` is unchanged: a trial's state, inbox and timers aren't stored. A
task a trial starts is, as it was, so `whiff wait` finds its result.

## Order to build it

Each step leaves whiff working, and closes the worst loss left:

1. A `tasks` bucket, replacing `jobs`, holding the result as a task ends.
   `whiff wait` reads it, so results survive a restart. `:outcome` everywhere.
2. One transaction per handled message, with redelivery of `whiff/done`. This
   closes the crash that strands a pipeline run.
3. One id from held to ended: a held job takes the id its sender already has.
4. `whiff cancel`, for every status.
5. Attempts, then retries.
