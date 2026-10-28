# services

whiff's first real services, kept here to dogfood them. whiff loads services
from `~/.whiff/services/`; link the ones you want there, and saving a file
here reloads it:

    mkdir -p ~/.whiff/services
    ln -s "$PWD"/services/git.puff "$PWD"/services/gh.puff "$PWD"/services/todo.puff ~/.whiff/services/

Or try one without installing it: `whiff try services/git.puff status`.

- `git`: `status` and `branch` of the repository in the caller's directory.
- `gh`: open pull requests (`prs`) and their checks (`checks` for the current
  branch's, `checks-of 12`), through the gh CLI, as you're logged in to it.
  `watch` the repository you're in, and every 5 minutes it publishes
  `:gh/changed {:dir :prs}` when its open pull requests change (`whiff sub
  gh/changed`); `unwatch`, `watching`, and `poll` to look now. What it
  watches and last saw is its state, kept across restarts.
- `todo`: the checkbox items in `TODO.md` in the caller's directory (`items`,
  `open`), `add text` and `done line`. The file is its state.
- `reviewer`: `review` has Claude review the uncommitted changes (`git diff
  HEAD`) with no tools; it answers at once with a pending id, and `whiff wait
  <id>` gives the review. Needs the claude CLI.
- `assistant`: a resident agent. `tell text` wakes Claude for one turn of
  the same session, with no tools; its answer lands in your inbox (`whiff
  inbox`), and replying to it is your next message. Its home is
  `~/.whiff/agents/assistant`. Needs the claude CLI.
