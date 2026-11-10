# procs: your app's processes, run by whiff

The terminals you keep open for a project, a `go run` server, a web UI, a
database and a cache in containers, as processes whiff runs for you:
started in order, each once those it needs are ready, brought back when
whiff starts, restarted when they crash, and stopped cleanly. Because
they're whiff's, other services can use them: one can bring your app up
and run an integration test against it, and tell you how it went.

This guide covers [`services/procs.puff`](../services/procs.puff), then a
worked example, [`examples/shop`](../examples/shop): a Go API on postgres
and redis, a web UI, and two tests of it, one a whiff service and one a
plain puff script.

For interrupted jobs and stray processes, see
[Troubleshooting](troubleshooting.md#work-was-interrupted-by-a-stop-or-crash).

## Setting it up

Copy the service, as you'll edit it, and name your processes in its
`procs`, per project, in the order they start:

```sh
mkdir -p ~/.whiff/services
cp services/procs.puff ~/.whiff/services/
```

```lisp
(def procs [:shop/db {:cmd ["docker" "run" "--rm" "--name" "shop-db" "-p" "5432:5432"
                            "-e" "POSTGRES_PASSWORD=dev" "postgres:17"]
                      :ready ["docker" "exec" "shop-db" "pg_isready" "-U" "postgres"]}
            :shop/api {:cmd ["go" "run" "./cmd/api"] :dir "~/src/shop" :restart #t
                       :ready "http://localhost:8080/health" :after [:shop/db]}
            :shop/web {:cmd ["go" "run" "./cmd/web"] :dir "~/src/shop" :after [:shop/api]
                       :url "http://localhost:3000"}])
```

It is a vector, `[name spec name spec ...]`, so it keeps its order, which
a map wouldn't. A name is `:project/process`. Each spec has:

| Field | What it is |
| --- | --- |
| `:cmd` | The command, `[program args...]`, without a shell. |
| `:dir` | Where it runs; your home unless given. `~/` works. |
| `:ready` | When it's ready: a URL that must answer 2xx, or a command that must exit 0. Tried every second while it starts, then every 10s while it runs. Without one, it is ready once it starts. |
| `:after` | Processes that must be ready before it starts. `up` brings them up too. |
| `:restart` | `#t` starts it again when it exits, 2s later, up to 3 exits in a minute; and restarts it when its `:ready` fails 3 times in a row as it runs. |
| `:stop` | A command that runs after it stops, such as `["docker" "compose" "down"]`. A `docker run --name x` gets `docker stop x` unless you give one. |
| `:url` | Its web UI: a link in the view, in sniff and in the CLI's lines. |

Saving the file reloads it. Processes that are running keep running.

## Using it

```sh
whiff call procs/up :shop               # the project: db, then api, then web
whiff call procs/up :shop/web           # or one, and what it comes :after
whiff call procs/runs                   # each one and how it is
whiff call procs/view | fzf | whiff act # Start, Stop, Restart or Logs
whiff call procs/logs :shop/api         # its output so far
whiff call procs/restart :shop/api
whiff call procs/down :shop
whiff sub procs/ready                   # {:proc :shop/api} as each is ready
```

`up`, `down` and `restart` are `:write`, so an agent's calls to them wait
for your review. `runs`, `view` and `logs` are `:read`, so an agent can
look at your processes and their output freely. sniff shows the view as a
page with buttons, and each `:url` as a link.

### How a process goes

| Status | Meaning |
| --- | --- |
| `:queued` | Waiting for those it comes `:after` to be ready. |
| `:starting` | Running, and its `:ready` hasn't passed yet. |
| `:running` | Running and ready: those after it can start. It is on `:procs/ready`. |
| `:unready` | Running, but its `:ready` didn't pass within 2m, or failed 3 times in a row since, without `:restart`. You hear of it; those after it wait. It is `:running` again once its `:ready` passes. |
| `:waiting` | It exited and `:restart`s: it starts again in 2s. |
| `:failed` | It exited 3 times in a minute, or didn't start. You hear of it, with Start and Logs. |
| `:exited` | It exited and doesn't `:restart`. |
| `:stopping` | You stopped it: SIGTERM, then SIGKILL after 3s, then its `:stop`. |
| `:clearing` | A container of its, left running from before, is being stopped before it starts again. |
| `:stray` | Its process group may still run from before whiff died. It isn't started, as it would fight it for its port, and your inbox says which group to kill. |
| `:down` | Not running. |

Every change is published on `:procs/changed`, and readiness on
`:procs/ready`.

**Stopping.** Stopping a process sends its process group SIGTERM, so a
server can shut down cleanly, then SIGKILL after 3s to whatever is left;
then its `:stop` runs. A container runs in the foreground, and `docker
run` passes the SIGTERM on to it. One that takes longer than 3s keeps
running when the CLI is killed, so its `:stop` (`docker stop x` by default
for a named container) stops it.

**whiff stopping and starting.** Processes stop with whiff, as they're
its jobs. Its state keeps which ones you wanted up, so starting whiff
brings those back, in order, each after those it needs are ready again.
`down` takes one out of that.

**whiff dying.** After a crash or `kill -9`, a process may still run. Most
servers die at their next log line, as whiff read their output, but a
quiet one like the `docker run` CLI doesn't. procs asks `:jobs/process`,
and one whose group is still there is `:stray`: your inbox says to `kill
-- -<group>`, then Start. A container still running is stopped with its
`:stop`, then started again.

**Crashing.** With `:restart #t`, one that exits starts again 2s later,
through `send-after`, not in a tight loop. Its third exit in a minute
stops that, and your inbox has its last output.

## A worked example: the shop

[`examples/shop`](../examples/shop) has a small shop, standard library only:

- [`api`](../examples/shop/api/main.go): orders on `:8080`, kept in redis.
  `/health` answers 200 once postgres and redis take connections.
- [`web`](../examples/shop/web/main.go): a page on `:3000` that shows an
  order from the API.

Its procs table, with `:dir` where your whiff checkout is:

```lisp
(def procs [:shop/db {:cmd ["docker" "run" "--rm" "--name" "shop-db" "-p" "5432:5432"
                            "-e" "POSTGRES_PASSWORD=dev" "postgres:17"]
                      :ready ["docker" "exec" "shop-db" "pg_isready" "-U" "postgres"]}
            :shop/redis {:cmd ["docker" "run" "--rm" "--name" "shop-redis" "-p" "6379:6379" "redis:7-alpine"]
                         :ready ["docker" "exec" "shop-redis" "redis-cli" "ping"]}
            :shop/api {:cmd ["go" "run" "./api"] :dir "~/src/whiff/examples/shop" :restart #t
                       :ready "http://localhost:8080/health" :after [:shop/db :shop/redis]}
            :shop/web {:cmd ["go" "run" "./web"] :dir "~/src/whiff/examples/shop" :after [:shop/api]
                       :ready "http://localhost:3000/" :url "http://localhost:3000"}])
```

A real run, from nothing: `runs` a second apart.

```
$ whiff call procs/up :shop
{:shop/api :queued :shop/db :starting :shop/redis :starting :shop/web :queued}
$ whiff call procs/runs
[{:job 31 :proc :shop/db :status :running} {:job 32 :proc :shop/redis :status :running}
 {:job 33 :proc :shop/api :status :starting} {:for [:shop/api] :proc :shop/web :status :queued}]
[... {:job 33 :proc :shop/api :status :running} {:job 34 :proc :shop/web :status :starting}]
[... {:job 33 :proc :shop/api :status :running} {:job 34 :proc :shop/web :status :running}]
$ whiff call procs/view
shop/db     running, job 31  docker run --rm --name shop-db ...      Stop     [:procs/down :shop/db]
...
shop/web    running, job 34  go run ./web  http://localhost:3000    Logs     [:procs/logs :shop/web]
$ whiff call procs/logs :shop/api
shop/api, running, job 33

2026/11/02 22:41:58 api listening on localhost:8080
2026/11/02 22:41:59 GET /health
```

The databases were ready in about a second, the API a second later, and
the web UI after it: about three seconds from nothing to all four ready.

## Testing the app

### A whiff service: shop-test

[`examples/shop/shop-test.puff`](../examples/shop/shop-test.puff) brings
the shop up, waits until it's ready, runs the API's cases, and tells you
how it went. Link it next to procs; a link, not a copy, as it uses
`check.puff` and `lib/http-check.puff` beside it:

```sh
ln -s "$PWD/examples/shop/shop-test.puff" ~/.whiff/services/
whiff call shop-test/run
```

`run` calls `:procs/up :shop`. If the shop isn't ready yet, `run` answers
`:waiting` and notes it in its state. `shop-test` subscribes to
`:procs/ready`, and once every `shop/` process runs, it runs the cases.
They aren't its own: `(use "check.puff")` brings in the cases of the plain
puff script below, and the HTTP checking both share, so whiff and CI test
the same thing.

```lisp
(service shop-test :state {:waiting #f}
  (use "check.puff")
  (def api "http://localhost:8080")
  ...
  (let [results (check api)] ...))
```

The result lands in your inbox, with Run again and the API's logs, and on
`:shop-test/done` for anything else that wants it:

```
$ whiff inbox
35  :shop-test -> :me/tell "shop-test: 6 of 6 passed"
    {:actions [{:action [:shop-test/run] :label "Run again"} {:action [:procs/logs :shop/api] :label "Logs of shop/api"}]}
```

With the shop already up, `run` answers at once, each case's result, and
what it got if it failed:

```
$ whiff call shop-test/run
[{:name "it is healthy" :pass #t} {:name "an order is made" :pass #t} {:name "it reads back" :pass #t}
 {:name "an order nobody made is 404" :pass #t} {:name "an empty order is refused" :pass #t}
 {:name "five orders get rising ids" :pass #t}]
```

`whiff history` shows the whole conversation; here oldest first, and
abridged:

```
22:41:56.976  call  user   :shop-test -> :procs/up :shop  (ok {:shop/api :queued :shop/db :starting :shop/redis :starting :shop/web :queued})
22:41:56.976  call  user   client -> :shop-test/run  (ok :waiting)
22:41:58.173  pub   user   :procs -> :shop-test on :procs/ready {:proc :shop/db}  (ok :done)
22:41:58.322  pub   user   :procs -> :shop-test on :procs/ready {:proc :shop/redis}  (ok :done)
22:41:59.298  pub   user   :procs -> :shop-test on :procs/ready {:proc :shop/api}  (ok :done)
22:42:00.332  pub   user   :procs -> :shop-test on :procs/ready {:proc :shop/web}  (ok :done)
22:42:00.345  call  user   :shop-test -> :procs/runs  (ok [... each :running])
22:42:00.378  call  user   :shop-test -> :me/tell "shop-test: 6 of 6 passed" ...
```

### A plain puff script: check.puff

Puff runs without whiff too.
[`examples/shop/check.puff`](../examples/shop/check.puff) is a test of the
API as a script, `puff examples/shop/check.puff`, which exits 0 if each
case passes and 1 if not, so it fits CI as it is. Its cases are data: a
request and what the response must have, its status, its text, or fields
of its JSON at any depth. A case can `:save` a field of what it got, and a
later case's path can be a function of what was saved. The checking is in
[`lib/http-check.puff`](../examples/shop/lib/http-check.puff), which the
script uses, `(use "lib/http-check.puff")`; the script keeps the cases:

```lisp
(def cases
  [{:name "it is healthy" :req [:get "/health"] :want {:status 200 :text "ok\n"}}
   {:name "an order is made" :req [:post "/orders" {:item "tea" :qty 2}]
    :want {:status 201 :json {:item "tea" :qty 2}} :save :id}
   {:name "it reads back" :req [:get (fn (saved) (str "/orders/" (map/get-or saved :id 0)))]
    :want {:status 200 :json {:item "tea" :qty 2}}}
   {:name "an order nobody made is 404" :req [:get "/orders/999999"] :want {:status 404}}
   {:name "an empty order is refused" :req [:post "/orders" {:item "" :qty 0}] :want {:status 400}}])

; In lib/http-check.puff: has? is whether got has want: each field of a
; map, at any depth; else
; the same value.
(def (has? got want)
  (if (and (= (value/type want) :map) (= (value/type got) :map))
    (vector/all? (map/entries want)
      (fn ([k w]) (match (map/get got k) [(some g) (has? g w)] [(none) #f])))
    (= got want)))
```

Then one property beyond the table: five new orders get ids that only go
up. Against the running shop:

```
$ puff examples/shop/check.puff
✓ it is healthy
✓ an order is made
✓ it reads back
✓ an order nobody made is 404
✓ an empty order is refused
✓ five orders get rising ids
```

With redis stopped (`whiff call procs/down :shop/redis`), each case says
what it got, and the script exits 1:

```
✗ it is healthy: got (ok {:status 503 :text "dial tcp [::1]:6379: connect: connection refused\n"})
✗ an order is made: got (ok {:status 500 :text "dial tcp [::1]:6379: connect: connection refused\n"})
✗ it reads back: got (ok {:status 500 :text "dial tcp [::1]:6379: connect: connection refused\n"})
✗ an order nobody made is 404: got (ok {:status 500 :text "dial tcp [::1]:6379: connect: connection refused\n"})
✓ an empty order is refused
✗ five orders get rising ids: got (ok [0 0 0 0 0])
puff: (err {:kind :failed :message "5 of 6 cases failed"})
```

What makes this short in puff: responses are values you match on, not
objects to poke at; a failure is an `(err ...)` you can print, not an
exception; JSON parses to maps you compare with `=`; and the cases are
data, with functions in them where a case needs what an earlier one got.
The helpers have tests in the lib, which run with the script's:
`puff -t examples/shop/check.puff`.

## What else you can build on it

- **Run the tests when something changes.** A service that subscribes to
  `:gh/changed`, a pull request moved, or `:pipeline/done`, a dev Claude
  finished, and calls `shop-test/run`.
- **Check the pipeline's work against the real stack.** The pipeline's
  check can run your integration tests against the running app, not only
  unit tests. A process's `:dir` is fixed in the table, so it runs your
  checkout, not the pipeline's worktree.
- **Let an agent debug with you.** Over `whiff mcp`, an agent reads
  `procs/logs` and `procs/runs` freely; restarting or stopping waits for
  your review.
- **A smoke check.** Beyond each process's own `:ready`, a service that
  runs shop-test's cases every few minutes, with `send-after`, and tells
  you when one fails.
- **One-off setup.** A migration or seed as a process without `:restart`,
  `:after` the database: it runs once and shows `:exited 0`.

## Limits

- Processes are named in the file, not made as you go: there is no fresh
  stack per branch or worktree.
- Ports are yours: one stack per set of ports, and no isolated copies side
  by side.
- `down` doesn't stop what comes after a process; a dependent keeps
  running.
- whiff doesn't serve or proxy HTTP: you reach each process on its own
  port.
- One daemon per user, on your machine.

## Underneath: jobs

Each process is a `:jobs/run` job, so the job tools work on them too:

- `:timeout :none` runs a job until it ends or is stopped; other jobs time
  out after 1h unless their `:timeout` says otherwise.
- `:jobs/output id` is a running job's output so far: the last 64KiB of
  stdout and of stderr; a job run with `:output :file` keeps all of it in
  `~/.whiff/jobs/<id>/`. `procs/logs` reads it. A stopped job's error
  also carries `:stdout` and `:stderr`, including its shutdown output;
  procs keeps that output for its logs after the job ends.
- Cancelling a job, its timeout, or `whiff stop` sends its process group
  SIGTERM, then SIGKILL after 3s.
- `:jobs/process id` says whether a job's process group may still run,
  even after whiff died while it ran.

See [design.md](design.md) for jobs and [tasks.md](tasks.md) for how they
end.
