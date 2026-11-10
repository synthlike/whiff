# From scratch: a laptop, a VPS and a project

This guide starts with nothing installed and ends with whiff on your
laptop and on a VPS, working on one project together:

- on the laptop, the project's TODO list, its git status and pull
  requests, and one inbox and one history for both machines;
- a VPS that waft set up from a puff file, where models write code for
  the project through huff, each command in a container;
- a PR comment poller, a service of about 80 lines of puff, running on the
  VPS and leaving a heads-up in your inbox when someone comments;
- a bigger change planned on the VPS, which you edit and approve from the
  laptop.

```
laptop                                        VPS (Ubuntu, set up by waft)
──────                                        ────────────────────────────
whiff daemon ──── ssh: whiff bridge ────────> whiff daemon (systemd, user whiff)
  todo, git, gh                                 pipeline, plan: huff on Kimi,
  your inbox: both machines'                      checks in podman containers
  history: both machines'                       prnotes: gh every 5 minutes
sniff (browser)                                 ~/src/tally (a clone)
waft (sets the VPS up, then gets out of the way)
```

You never log in to the VPS: waft sets it up from the laptop, and from
then on the laptop's whiff drives it. It takes about an hour, most of it
waiting for apt and the sandbox image. The model calls cost
cents: on Kimi K2 a small task took us one to two cents a dev turn.

## What you need

- Go 1.27 and git on the laptop, and the GitHub CLI, `gh`, logged in.
- A project on GitHub. Here it is `you/tally`, a small Go CLI; any
  repository whose check is one command works.
- An Ubuntu VPS that you reach as an admin with your ssh key and
  passwordless sudo, here `admin@203.0.113.7`. On Ubuntu 26.10 apt's
  `golang-go` is the Go 1.27 whiff needs; with an older one, `go install`
  fetches 1.27 itself.
- An [OpenRouter](https://openrouter.ai) key, for the models on the VPS,
  and a GitHub token that can read the project's pull requests. The
  examples read both from 1Password with `op read`; any command that
  prints a secret does.

## 1. whiff on the laptop

Install the five binaries from a checkout of the version you'll run
everywhere:

```sh
git clone https://github.com/synthlike/whiff ~/src/whiff
cd ~/src/whiff && git checkout v0.8.1
go install ./cmd/...        # whiff, sniff, puff, huff, waft
```

whiff has no config file: it starts on first use and loads the services in
`~/.whiff/services/`. Link the ones the laptop needs:

```sh
mkdir -p ~/.whiff/services
ln -s ~/src/whiff/services/{git,gh,todo}.puff ~/.whiff/services/
whiff services
```

They act on the directory you call them from, so in the project:

```sh
cd ~/src/tally
whiff call git/status
whiff call gh/prs
whiff call todo/add "Add a 'tally sub' command, with a test"
whiff call todo/add "Write a README with an example"
whiff call todo/open
```

`TODO.md` is todo's state, so editing it by hand is fine. `whiff call
todo/view | fzf | whiff act` ticks the item you pick.

## 2. puff, in two minutes

Services, host files and the glue between them are puff, a small lisp
whose values never change, with no nil: lookups give Options, failures
give Results.

```sh
puff -e '(map/get {:a 1} :b)'          # (none)
puff -e '(string/upper "tally")'       # "TALLY"
whiff eval '(call :todo/open)'         # in the daemon: services are functions
```

`whiff eval` runs in the daemon, so it can call any service. This one
takes the first open item off the TODO list, which you'll hand to the VPS
in step 5:

```sh
cd ~/src/tally
whiff eval '(let [{:text t} (unwrap (vector/first (unwrap (call :todo/open))))] t)'
# "Add a 'tally sub' command, with a test"
```

[Learn puff](puff.md) has the rest.

## 3. Describe the VPS in puff

waft sets up a host from a puff file: `waft plan` shows what differs,
`waft apply` fixes it, over ssh, needing nothing on the host but sudo.
Copy its two libraries next to a new host file:

```sh
mkdir ~/hosts && cd ~/hosts
cp ~/src/whiff/cmd/waft/lib/{whiff,hardened}.puff .
cp ~/src/whiff/services/{pipeline,plan,prnotes}.puff .
```

and write `vps.puff`:

```lisp
(use "whiff.puff")
(use "hardened.puff")

(host "admin@203.0.113.7"
  [(hardened {})                                 ; ssh by key only, a firewall, security upgrades
   (pkg "git" "golang-go" "gh")
   (user "whiff" {:keys ["~/.ssh/id_ed25519.pub"] :linger #t}
     [(ssh-key)                                  ; a key for GitHub, made on the host
      (repo "~/src/tally" "git@github.com:you/tally.git")
      (whiff {:version "v0.8.1"
              :env {"OPENROUTER_API_KEY" {:cmd "op read op://dev/openrouter/key"}
                    "GH_TOKEN" {:cmd "op read op://dev/github-tally-read/token"}}
              :services ["pipeline.puff" "plan.puff" "prnotes.puff"]
              :sandbox #t})])])
```

What each part does:

- `hardened` turns off password logins, lets only ssh through a firewall
  and installs security upgrades daily.
- `user "whiff"` is the account whiff runs as; `:linger #t` lets its
  systemd unit run without a login, and `:keys` lets you in as it.
- `ssh-key` makes a key on the host; the private half never leaves it.
- `whiff` (from `whiff.puff`) installs whiff and huff at `:version` with
  `go install`, writes `~/.whiff/env` with the two secrets, read on your
  laptop as you apply and sent over ssh's stdin, copies the services, and
  starts the daemon as a systemd user unit. It gives the user a git
  identity for pipeline's commits, `whiff` unless you pass `:git`, and
  with `:sandbox #t` installs podman and builds the image sandboxed
  commands run in, unless `whiff sandbox check` says it's there.
- `GH_TOKEN` only needs to read pull requests: a fine-grained token for
  the one repository is enough. Pushes go through the deploy key.

```sh
waft plan vps.puff        # what would change; nothing changes
waft apply vps.puff
```

The first apply stops at the `repo`: GitHub doesn't know the host's key
yet. The plan printed its public half; add it to `you/tally` as a deploy
key, with write access so the host can push branches, and apply again.
`waft plan vps.puff` then shows every line `ok`.

The sandbox image takes a few minutes on the first apply; after that
the plan shows `whiff's sandbox image` as `ok` until a whiff version
changes the image.

## 4. Connect the two

From the laptop, the VPS becomes a peer: your whiff runs `ssh whiff@host
whiff bridge`, finding whiff in `~/go/bin` where waft put it, and keeps
it connected, reconnecting by itself after a reboot or a dropped
network. Calls to `address@vps` run there, in `--dir`:

```sh
whiff peer add vps whiff@203.0.113.7 --dir /home/whiff/src/tally
whiff peer ls
# vps  connected  whiff@203.0.113.7 · /home/whiff/src/tally
whiff ls                  # every function, with its :read or :write mark
```

Both sides must run the same whiff: `peer ls` says so if they don't.
From now on `whiff inbox`, `whiff history` and sniff show both machines.

## 5. Coding work on the VPS

`pipeline/build` runs one task: a developer model makes the change in a
worktree of its own, whiff runs your check there, and a reviewer model
reviews the diff; until both pass, the review goes back to the developer,
up to three rounds. On the VPS both roles run on huff, and `:sandbox #t`
runs the developer's commands and the check in a container that sees
only the worktree, without the network:

```sh
whiff call pipeline@vps/build '"/home/whiff/src/tally"' '"main"' \
  "\"Add a 'tally sub' command, with a test\"" '["go" "test" "./..."]' \
  '{:dev "openrouter/moonshotai/kimi-k2" :reviewer "openrouter/moonshotai/kimi-k2" :sandbox #t}'
# pending 12 (whiff wait 12)
```

Or straight from the TODO list, as step 2's eval, now handing the item
on:

```sh
cd ~/src/tally
whiff eval '(let [{:text t} (unwrap (vector/first (unwrap (call :todo/open))))]
  (call :pipeline@vps/build "/home/whiff/src/tally" "main" t ["go" "test" "./..."]
    {:dev "openrouter/moonshotai/kimi-k2" :reviewer "openrouter/moonshotai/kimi-k2" :sandbox #t}))'
```

Watch it with `whiff history -f peer:vps`, or in sniff. When it ends:

```sh
whiff wait 12
# (ok {:run 1 :status :ready :branch "whiff/1" :cost 0.004 ...
#      :note "Committed on whiff/1 in /home/whiff/.whiff/pipelines/tally-1, after 1 rounds. ..."})
whiff call pipeline@vps/show 1     # the last review and the diff it approved
```

A run that fails says why in `show`; one that gave up or failed can be
taken on with `pipeline@vps/retry 1 '""'`. If the VPS restarts mid-run,
the run says so honestly rather than guessing how far it got, and its
container is cleaned up when whiff comes back.

To review the change as a pull request, have the VPS push the run's
branch, through its deploy key, and open one from the laptop:

```sh
whiff call pipeline@vps/push 1 '""'        # "" for the run's own branch, whiff/1
gh pr create --repo you/tally --head whiff/1 \
  --title "Add a 'tally sub' command" --body "Built and reviewed by whiff on the VPS."
whiff call todo/done 3             # the item's line, as todo/open says
```

## 6. A PR comment poller

Now a service of your own, for the VPS: every 5 minutes it looks at the
project's open pull requests and leaves a heads-up in your inbox for each
new comment or review. It is
[`services/prnotes.puff`](../services/prnotes.puff), which you copied in
step 3. The parts:

```lisp
(service prnotes :state {}
  :start (fn (state) (do (send-after every :tick) (ok state)))
  (def every 5m)
  ...
```

A service has a state, here each watched directory's comment and review
ids, kept in whiff's database across restarts. `:start` sends the
service a `:tick` in 5 minutes; each tick sends the next.

```lisp
  (def (notes dir)
    (match (try (proc/run ["gh" "pr" "list" "--json" "number,title,comments,reviews"] {:dir dir}))
      [{:exit 0 :stdout out}
       (ok (vector/reduce (try (json/parse out)) [] (fn (all p) (vector/concat all (notes-of p)))))]
      [{:stderr e} (err {:kind :gh :message (string/trim e)})]))
```

`proc/run` runs gh without a shell; `match` takes the result apart, and
`try` returns early with an `(err ...)`. You don't mark functions as
reading or writing: whiff infers it from what each reaches. A command
line built at run time, or a change of state, makes these `:write`, as
`whiff ls` shows, so a resident agent that calls `prnotes/watch` waits
for your review; your own calls, and the service's timer, go through.

```lisp
  (on (tick state) :update
    "Polls, and ticks again in 5 minutes; its own loop: call poll instead."
    (do (send-after every :tick)
      (ok (look state))))
```

`look` keeps the notes whose ids it hasn't seen, and leaves each in your
inbox with `(call :me/tell (line n))`. An `:update` function answers
`(ok [value new-state])`, and whiff commits the new state.

The file carries its tests. Run them, and try a function once against a
real repository, without installing anything:

```sh
whiff test ~/src/whiff/services/prnotes.puff
# 1 passed, 0 failed
cd ~/src/tally && whiff try ~/src/whiff/services/prnotes.puff watch
# :watching
```

It is already on the VPS. Start it watching the project there, from the
laptop:

```sh
whiff call prnotes@vps/watch       # runs in the peer's --dir, the clone
```

When a reviewer comments on the pull request from step 5, within 5
minutes:

```sh
whiff inbox
# 4@vps  :prnotes@vps -> :me@vps/tell "ana commented on #7 Add a 'tally sub' command: Could it print the result with a newline?"
whiff reply 4@vps ok               # done with it
```

To change the poller, edit it on the laptop and `waft apply vps.puff`:
waft copies the file, and whiff reloads it. If the new version doesn't
load, the old one keeps running and `whiff services` says where the error
is.

The comment asks for a change. Build it as a new run from the pull
request's branch, then push that run's branch onto it, and the pull
request updates:

```sh
whiff call pipeline@vps/build '"/home/whiff/src/tally"' '"whiff/1"' \
  '"Print the result of tally sub with a trailing newline, as review asked."' '["go" "test" "./..."]' \
  '{:dev "openrouter/moonshotai/kimi-k2" :reviewer "openrouter/moonshotai/kimi-k2" :sandbox #t}'
# pending 15 (whiff wait 15); once ready, run 2 pushes onto the PR's branch:
whiff call pipeline@vps/push 2 '"whiff/1"'
```

(`pipeline/retry` is for a run that gave up, failed or was cancelled: it
goes on in the same developer session, with your note.)

## 7. Something bigger: a plan you approve

For a change too big for one task, `plan` has a planner read the
repository (it can't change it) and split the goal into a few tasks, each
built as a pipeline run from the branch of the one before. `draft` stops
before building, so you can read the tasks first:

```sh
whiff call plan@vps/draft \
  "\"A 'tally stats' command: count, sum and mean of the numbers in a file, one per line, with tests\"" \
  '["go" "test" "./..."]' \
  '{:planner "openrouter/moonshotai/kimi-k2" :dev "openrouter/moonshotai/kimi-k2" :reviewer "openrouter/moonshotai/kimi-k2" :sandbox #t}'
```

The tasks land in your inbox, numbered. Change them from the laptop:

```sh
whiff call plan@vps/edit 1 2 '"Add Mean to stats.go; an empty file is an error, not NaN"'
whiff call plan@vps/remove 1 3
whiff call plan@vps/redraft 1 '"Fewer, larger tasks"'   # or ask the planner again
whiff call plan@vps/approve 1                           # build them
```

or with the buttons on the inbox message in sniff. When the last task is
built, one message says so and names the last run's branch, which holds
the whole goal; push it as in step 5, `whiff call pipeline@vps/push <run>
'""'`. `whiff call plan@vps/plans` says
where each plan is meanwhile.

## 8. Seeing it all

```sh
sniff -open
```

opens whiff in a browser, served on 127.0.0.1 only: Activity has every
request as a card, the VPS's too, with what each step cost; the Map draws
who talks to whom, the VPS in a band of its own; Inbox and Review have
both machines' items. From the terminal:

```sh
whiff services                     # what needs you first; a peer that isn't connected, and why
whiff history -c peer:vps          # the VPS's messages as chains
whiff history 'to:pipeline@vps'
whiff peer ls
```

## What keeps it safe

- **Commands run in containers.** With `:sandbox #t`, everything a
  developer model runs, and your check, runs in a container that sees
  only its worktree, read-only `.git`, no network, no home and no whiff
  socket. A cancel or a crash ends the container; whiff fails closed if
  there's no sandbox to run in.
- **Models' file tools stay in the worktree.** huff's Read, Write and
  Edit act only there and never write in `.git`, and its Bash runs only
  the commands the role allows, without a shell.
- **Agents' writes wait for you.** A resident agent's message to a
  `:write` function waits in `whiff pending` until you accept it, on the
  VPS too; whiff infers which functions write, so there's nothing to
  label by hand. The pipeline's own steps run as yours: their changes stay
  on a branch until you push or merge them.
- **Secrets stay out of files you share.** The host file names commands
  that print them; waft reads them as it applies and writes them to a
  0600 file on the host. A turn whose daemon dies stops at once, rather
  than calling the model for nobody.
- **A peer acts as you, nothing more.** Your laptop drives the VPS over
  your ssh key; the VPS can't reach back. A push never rewinds a branch
  on GitHub: it goes as a fast-forward or not at all.

## Next

- [Use cases](use-cases.md): a team of agents on a board, dev servers,
  a resident assistant.
- [Agents](agents.md): agents on huff, budgets and the review gate.
- [waft](waft.md): every resource, secrets and removing things.
- [Design](design.md): peers, the sandbox and the review gate in detail.
