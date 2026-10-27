# services

whiff's first real services, kept here to dogfood them. whiff loads services
from `~/.whiff/services/`; link the ones you want there, and saving a file
here reloads it:

    mkdir -p ~/.whiff/services
    ln -s "$PWD"/services/git.puff ~/.whiff/services/

Or try one without installing it: `whiff try services/git.puff status`.

- `git`: `status` and `branch` of the repository in the caller's directory.
