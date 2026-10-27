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
- `todo`: the checkbox items in `TODO.md` in the caller's directory (`items`,
  `open`), `add text` and `done line`. The file is its state.
