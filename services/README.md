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
  one you pick.
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
- `plan`: `start goal check` has a planner Claude split goal into a few
  tasks and builds them one after another as `pipeline` runs, each from the
  last one's branch. `plans` says where each is. Needs `pipeline`.
- `assistant`: a resident agent. `tell text` wakes Claude for one turn of
  the same session, with no tools; its answer lands in your inbox (`whiff
  inbox`), and replying to it is your next message. Its home is
  `~/.whiff/agents/assistant`. Needs the claude CLI.
