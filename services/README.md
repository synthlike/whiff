# services

whiff's first real services, kept here to dogfood them. whiff loads services
from `~/.whiff/services/`; link the ones you want there, and saving a file
here reloads it:

    mkdir -p ~/.whiff/services
    ln -s "$PWD"/services/git.puff "$PWD"/services/gh.puff "$PWD"/services/todo.puff ~/.whiff/services/

Or try one without installing it: `whiff try services/git.puff status`.

- `git`: `status` and `branch` of the repository in the caller's directory,
  and `changes`, its changed files as a view of locations.
- `gh`: open pull requests (`prs`) and their checks (`checks` for the current
  branch's, `checks-of 12`), through the gh CLI, as you're logged in to it.
  `watch` the repository you're in, and every 5 minutes it publishes
  `:gh/changed {:dir :prs}` when its open pull requests change (`whiff sub
  gh/changed`); `unwatch`, `watching`, and `poll` to look now. What it
  watches and last saw is its state, kept across restarts.
- `todo`: the checkbox items in `TODO.md` in the caller's directory (`items`,
  `open`), `add text` and `done line`. The file is its state. `view` is the
  open items as a view: `whiff call todo/view | fzf | whiff act` ticks the
  one you pick; `board` is the same items as a board, a column per heading.
- `reviewer`: `review` has Claude review the uncommitted changes (`git diff
  HEAD`) with no tools; it answers at once with a pending id, and `whiff wait
  <id>` gives the review. Needs the claude CLI.
- `pipeline`: a dev pipeline of two Claudes. `start task check` has a dev
  Claude make the change in a git worktree of its own
  (`~/.whiff/pipelines/<repo>-<n>`, branch `whiff/<n>`), runs `check` (say
  `'["go" "test" "./..."]'`) there, and has a reviewer Claude review the
  diff; until both pass, the review goes back to the dev, up to 3 rounds,
  then it is committed on the branch and you hear in your inbox. `runs`,
  `view`, `cancel n`; `:pipeline/done` as each run ends. Needs the claude CLI.
- `lead`: a Sonnet agent that builds software with `board/build`, which
  hands it `pipeline/build`, an ask that answers when a run ends: a scaffold run, a few parts at once on a
  model you name (haiku), then one run from all their branches merged. Its
  workers may ask it questions with `:lead/ask`, their only whiff tool, and
  you hear only what matters. Its plan is on `board`, a ticket a run, its
  runs' models, check, count and spend as the project's team says. `whiff trust agent/lead board
  pipeline` first.
- `plan`: `start goal check` has a planner Claude split goal into a few
  tasks and builds them one after another as `pipeline` runs, each from the
  last one's branch. `plans` says where each is. Needs `pipeline`.
- `board`: what is being built, by project, as tickets in Todo, Doing,
  Review, Done and Failed: its own (`add project title`, `move id step`,
  `assign id who`; `plan repo title task` for a run, which `build id base
  check model` starts, the ticket then following its run), plans' tasks
  and pipeline runs, each in the project of its repository. Each project
  has a team, `set-team project {:dev "haiku" :most 3}`: the `:lead`
  `tell project text` tells, in its `:repo`; the `:dev`'s and
  `:reviewer`'s models and the `:models` runs may use; the `:check`; how
  many runs go at once, `:most` (4); and what its runs and its lead's
  turns for it may spend a `:day`.
  `:default` puts a role back, `reset-team project` all of them. `team project` says it, and the
  board shows it first. `show` is every project's, `for project` one's;
  sniff's sidebar opens it, for the project shown if one is.
- `assistant`: a resident agent, one `(agent assistant :prompt "...")`
  form. `tell text` wakes Claude for one turn of the same session, with no
  tools; its answer lands in your inbox (`whiff inbox`), and replying to it
  is your next message; `ask text` answers whoever asked instead, so a
  service can use it. Its home is `~/.whiff/agents/assistant`. Another
  agent is another file, with its own `:prompt`, and `:model`, `:can
  [:gh/prs]` for the functions it may use as tools, `:limits {:day 2}` and
  `:every 1d` if you like; `:can [:assistant/ask]` lets it delegate to
  another agent, whose answer comes back as a turn of its own (one turn for all
  that a turn started). `usage` says what it spent, `stop` ends the
  turn running now, and `pause`/`resume` hold its turns while messages wait. Needs the claude CLI.
- `digest`: an agent that briefs you on this repository once a day: open
  pull requests and their checks, open TODO items and uncommitted work, in
  your inbox, and only what changed. `ask "brief me"` for one now. Its
  tools are `:read` functions of `gh`, `todo` and `git`, so it needs those.
- `procs`: your dev servers and containers, named per project in the file
  (copy it and fill in `procs`): `up :shop` starts them in order, `down`,
  `restart`, `runs`, `logs :shop/api`, and `view` with Start, Stop, Restart
  and Logs. Starting whiff brings back the ones you wanted up; with
  `:restart #t` one that exits is started again, until its third exit in a
  minute, which lands in your inbox. See [docs/procs.md](../docs/procs.md).
