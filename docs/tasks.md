# Tasks

The spec for durable work, built in v0.6: "Today" says how whiff meets it.
[design.md](design.md) has the rest of whiff.

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
- `:stopped`: the process is gone, but what it had done stays done. A
  job's err has `:stdout` and `:stderr`, the end of what it printed, its
  shutdown included, as a finished job's result does.
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
| `:running` | Its process group gets SIGTERM, then SIGKILL after 3s | `:cancelled`, `:stopped` |
| `:ended` | Nothing | `(err {:kind :ended :result r})` to the canceller; the task keeps `r` |

The first transaction to end a task wins. A job that exits as you cancel it
keeps its real result, and a task can't end twice: an answer that comes after
the end is dropped, and history says so.

## Parents and children

An agent's turn that calls an ask, as lead's `pipeline/build`, or a `:write`
held for review, starts a task: the agent is its parent, and the service
that answers it may start children of its own, as pipeline's dev and
reviewer jobs, the dev with a session of its own, `agent/<n>`.

| Question | Rule |
| --- | --- |
| Who owns a result | The session that started the task. An agent's, `agent/<name>`, lasts its turns: a result comes back to it as `whiff/result`, a turn of its own, stored as any task's, so a restart or reload loses none. |
| Parent's turn fails | Its children run on, and their results still come. Restarting the turn tells it what it had started and how each stands, so it doesn't start them again. |
| Parent paused | Its children run on; their results wait in its inbox until it is resumed. |
| Parent cancelled | Its turn stops and each task it has open is cancelled: one waiting in a service's inbox leaves it, and the service hears `whiff/cancelled id`, so it can stop what it began, as pipeline cancels the run and kills its dev. Cancelling stops descendants; nothing detaches them. |
| Child session ends | A new session, `agent/<n>`, ends with the job it was given to: what it has open is cancelled, as nobody would hear how it ends. |
| Parent unloaded | Its results wait for it, as any service's, and come when it loads. |
| Budget | A result's `:cost`, as a pipeline build's, is the parent's spending, counted with its turns' against its `:day`, before the turn the result starts, which the budget may then refuse. `service/status` says what of the day's spending its tasks cost, `:tasks-spent`. |
| Fan-out | A session may have `:tasks` open at once, 10 unless its agent's `:limits` say; one more is `(err {:kind :too-many})`, at once. |
| Capabilities | A child's session reaches only the functions its job was given (`:session [...]`), gated as any agent's: being started by a trusted parent gives it neither your origin nor a way past the review gate. A parent's trust mode isn't its children's. |

Depth is bounded by what each session may call: a dev may ask its lead, and
nothing else, so it can't start children of its own.

## Today

| Spec | Today |
| --- | --- |
| Pending means accepted and stored | Yes for jobs, asks and held messages (`tasks` and `inbox` buckets) |
| A result is stored before anyone is told | Yes, in `tasks`, for 30 days: `whiff wait` reads it after a restart |
| A service gets its result until handled | Yes: the mark commits with a stateful service's state; a stateless one's is set once its handler returns |
| One id from held to ended | Yes: a held job keeps its id, and accepting stores it as accepted as it leaves the queue |
| `:outcome` on every failure | Yes |
| Cancelling any task | Yes, `whiff cancel` and the agent's `cancel` tool, and a task ends once; not a message you accepted while it is delivered, which ends within a call's 5s |
| Attempts and retries | Yes, for jobs: `:retry n` on `:jobs/run`, and `:idempotent #t` for a write that is safe to run twice. A job whose process group may still run after a crash isn't run again |
| Parents | Yes: a task begun in another's work, by the service its `whiff/asked` reaches or by the session of the job it runs, as an agent's turn, keeps that task as its `:parent`, stored with it; `:tasks/tree id` gives a task with what its work began, nested, and an agent's Status in sniff draws a build's jobs from it |
| Seeing every task | Yes: `:tasks/list` and `:tasks/view` give each task held, waiting or running and each that ended in the last day, with its owner, attempts, result, whether its process may still run, and what one that hasn't ended waits on (your review, its agent's queue or pause, its own work); `:tasks/failed` only those that failed. Its actions are Cancel, Accept and Reject, Watch, and those its result offers, as a build's Retry. sniff links to both |
| Watching one run | Yes: `:tasks/watch id` is a running job's output as it comes, an agent's turn or a pipeline dev's read from claude's stream-json as what it says and calls and what answers, following `:jobs/<id>`; `whiff sub jobs/<id>` gives the lines themselves |

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
