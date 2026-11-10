# waft: hosts described in puff

waft sets up the machines whiff runs on. A host is a puff value: its ssh
address and the resources it should have, such as packages, users, files,
repos and secrets. `waft plan` shows what differs on the host, and `waft
apply` changes it to match, then checks again. Checks only read, so a plan
never changes anything. It needs nothing on the host beyond ssh and an
admin with passwordless sudo. Ubuntu only, and outside whiff's releases
for now: `go install ./cmd/...` installs it with the rest.

This guide covers a first host, users and their setups, the two libraries
waft comes with, secrets and checked writes, and what waft won't do. For
every resource and its options, run `waft` with no arguments.

## A first host

Put the host in a file, `vps.puff`:

```lisp
(host "admin@203.0.113.7"
  [(pkg "git")
   (user "ana" {:keys ["~/.ssh/id_ed25519.pub"]}
     [(file "~/.gitconfig" {:from "dotfiles/gitconfig"})
      (repo "~/src/notes" "https://github.com/ana/notes.git")])])
```

`admin@203.0.113.7` is whatever ssh takes, an alias from `~/.ssh/config`
too. Local paths, as `dotfiles/gitconfig` and the key, are read on your
machine, from the file's folder or with `~` from your home.

```sh
waft plan vps.puff     # what would change
waft apply vps.puff    # change it, then check each resource again
waft plan vps.puff     # every line ok
```

A plan prints each resource as `ok` or `change`, with the commands that
would fix it; a write shows as its path and size. It exits 0 when each
host matches, 2 when one differs and 1 on an error, so a script can tell
drift from trouble. apply stops at the first resource still off after its
fix, and says why.

## Users and their setups

`(user name opts [setup])` is an account and the resources it has. Each
resource in the setup runs as that user, with `~` as their home, unless it
says `:as`. The account's own options are `:shell`, `:groups`, `:keys` (its
whole `authorized_keys`) and `:linger`, which lets its systemd units run
without a login.

A setup is a puff value, so users who share one share a definition:

```lisp
(def dev
  [(pkg "build-essential")
   (ssh-key)
   (file "~/.gitconfig" {:from "dotfiles/gitconfig"})])

(host "admin@203.0.113.7"
  [(user "ana" {:keys ["ssh-ed25519 AAAA... ana"]} dev)
   (user "ben" {:keys ["ssh-ed25519 AAAA... ben"]}
     (vector/concat dev [(repo "~/src/api" "git@github.com:ben/api.git")]))])
```

Every `pkg`, wherever it is, is installed first, in one go, as root, so a
setup can name the packages it needs. The rest runs in the order written.
`ssh-key` makes a key on the host for git, and a plan prints its public
half to add as a deploy key; the private half never leaves the host.

A plan for a user who doesn't exist yet shows their resources as `new`:
they can't be checked until apply makes the user.

## Libraries

waft knows nothing about whiff. Bigger pieces are puff functions that
return resources, in [`cmd/waft/lib`](../cmd/waft/lib). Copy one next to
your host file and use it, so the code that sets up your host is in your
folder, to read and change:

```sh
cp cmd/waft/lib/whiff.puff cmd/waft/lib/hardened.puff .
```

```lisp
(use "whiff.puff")
(use "hardened.puff")

(host "admin@203.0.113.7"
  [(hardened {})
   (pkg "git" "golang-go")
   (user "whiff" {:keys ["~/.ssh/id_ed25519.pub"] :linger #t}
     [(ssh-key)
      (whiff {:version "v0.8.1"
              :env {"OPENROUTER_API_KEY" {:cmd "op read op://dev/openrouter/key"}}
              :services ["services/pipeline.puff"]})])])
```

[`whiff.puff`](../cmd/waft/lib/whiff.puff) gives a user their own whiff:
whiff and huff installed with `go install` at a version, a systemd user
unit that loads `~/.whiff/env` and finds huff, a git identity for
pipeline's commits if the user has none (`:git {:name :email}`, `whiff`
by default), the service files, and the daemon running. Once applied,
reach it from your machine as a [peer](design.md#peers):

```sh
whiff peer add vps whiff@203.0.113.7 --whiff /home/whiff/go/bin/whiff
```

Peers need the same whiff on both sides, so give `:version` a tag or a
pushed commit that matches yours; the default, `latest`, is the newest tag.

[`hardened.puff`](../cmd/waft/lib/hardened.puff) turns off password and
keyboard-interactive logins over ssh, lets only ssh through a firewall
(plus `:allow`, as `["443/tcp"]`), and installs security upgrades daily. Try
it on a host you can afford to lose first. Its `:root-login` is
`"prohibit-password"` by default, root by key only; set `"no"` only once
waft logs in as an admin who isn't root, or the next run can't log in.

## Secrets and checked writes

`env` writes a 0600 environment file whose values are read on your machine
as you plan or apply, from an environment variable or a command:

```lisp
(env "~/.whiff/env" {"OPENROUTER_API_KEY" {:cmd "op read op://dev/openrouter/key"}
                     "GITHUB_TOKEN" {:env "GITHUB_TOKEN"}})
```

A value never goes in the puff file. It reaches the host on ssh's stdin,
not in a command line, which other users there could read; a plan names
the variables, never their values.

A file that can break a service, or lock you out, takes `:valid`, a
command with `%s` for the new file's path. waft writes the new content
beside the old file, runs the check on it, and moves it in place only if
it passes:

```lisp
(file "/etc/ssh/sshd_config.d/00-local.conf" "PasswordAuthentication no\n"
      {:valid "sshd -t -f %s" :restarts "ssh"})
```

If the check fails, apply stops with its output, before anything restarts.
`:restarts` names a systemd unit restarted once, at the end, if the
resource changed; for a resource run as a user, it's that user's unit.

## Removing things

A resource you delete from the file stays on the host: waft changes only
what the file names. To remove something, say so with `:absent #t`:

```lisp
(host "admin@203.0.113.7"
  [(pkg "telnet" {:absent #t})
   (file "/etc/motd" {:absent #t})
   (line "/etc/hosts" "10.0.0.5 old-db" {:absent #t})
   (user "ben" {:absent #t})])
```

A file, dir, repo or env needs only its path. A plan shows each as
`(absent)`, with the command that removes it. A dir and a repo go with
everything in them, unpushed work too. A user goes with its home: waft
turns off linger, ends its session and waits for its processes, as
`userdel` refuses a user still running something; an absent user can't
have a setup. `cmd` can't be absent: write the command that undoes it.

## What waft won't do

- Remove what the file doesn't name, as above.
- Pull a repo. `repo` clones a missing one and checks out `:ref`, but
  never updates one, so work on the host is safe.
- Upgrade `latest`. `go-bin` with a version that isn't a tag or a commit
  installs once; pin a version to upgrade.
- Run hosts in parallel, keep an inventory, or reach a cloud API. A vector
  of hosts runs one at a time.
