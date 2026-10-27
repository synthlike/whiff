# whiff

A small daemon of puffs: little services that talk to each other, and to you,
by sending messages.

Services are written in puff, a small lisp with immutable values and Results
instead of nil and exceptions. The CLI, the [hiss](https://github.com/synthlike/hiss)
editor and AI agents connect as clients with addresses of their own, so a
service can call `:gh/prs`, publish `:git/changed` or ask you a question and
get the answer later. whiff supervises the services, reloads them when you save
and keeps a history of every message.

Agents get whiff's functions as MCP tools. Anything an agent asks to write
waits in a review queue for you; nothing waits on you.

whiff has no UI of its own. Its CLI is enough to use it; hiss is the nicer
client.

## Inspirations

- Erlang/OTP: everything that talks has an address and a mailbox, messages
  are plain data, and services are supervised and restarted from their last
  good state.
- Clojure: immutable data that compares by value, with literals for vectors
  `[...]` and maps `{k v}`, keywords like `:read`, `let` with vector
  bindings, `loop` / `recur`, and `->` to thread a value through calls.
- Rust: Result and Option instead of nil and exceptions, and `try` to pass
  an error up.
- [vrs](https://github.com/leoshimo/vrs): a lisp runtime for personal
  services, the idea whiff started from.

## Status

Not started yet. First comes puff, runnable on its own with `whiff puff`.

## License

MIT, see [LICENSE](LICENSE).
