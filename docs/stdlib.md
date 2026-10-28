# puff's functions

Every function puff has, generated from its tables by
`go test ./internal/daemon -run TestStdlibDoc -update`; don't edit it by
hand. In a REPL, `(help)` lists them and `(help string/split)` shows one.

A signature names the arguments: `[b]` is optional and `...` takes any
number more of the one before it. Functions are `:read` unless marked
`:write`: a `:write` function changes something outside puff (a file, a
process, a message sent). `whiff ls` shows each handler's mark, worked out
from what it calls; whiff's review gate will go by it. A handler marked
`:read` that calls a `:write` function is an override, which `whiff ls` lists;
any other `:read` handler that reaches a `:write` function at run time faults.

Failures a caller should handle are values: `(err {:kind :message})`, or
`(none)` for a lookup that finds nothing. A wrong argument, such as a string
where an int goes, is a fault instead and stops the program.

The language's own forms, `def let fn if cond do and or match when loop recur -> try`,
aren't functions and aren't listed here.

## puff

In `puff` and in whiff alike. Names without a namespace are under core.

### core

- `(ok v)`: A successful Result.
- `(err e)`: A failed Result.
- `(some v)`: An Option with a value.
- `(none)`: An empty Option.
- `(fail e)`: Stop with a fault, e as its message.
- `(unwrap r)`: v, from (ok v) or (some v); an (err e) or (none) stops with a fault.
- `(or-else r default)`: v, from (ok v) or (some v); default for an (err e) or (none).
- `(help [f])`: Print f's arguments, mark and doc, or list every builtin.
- `(= a b)`: Whether a and b are the same value.
- `(not b)`: The other bool.
- `(< a b)`: Whether a is less than b: ints, floats, strings, durations or times.
- `(<= a b)`: Whether a is at most b.
- `(> a b)`: Whether a is greater than b.
- `(>= a b)`: Whether a is at least b.
- `(+ a b ...)`: The sum of ints, floats or durations, or a time plus durations.
- `(- a [b])`: a minus b, or minus a. A time minus a duration is a time, minus a time a duration.
- `(* a b ...)`: The product of ints or floats.
- `(str v ...)`: The values joined together, strings as they are and the rest printed: (str "line " 12) is "line 12".

### string

- `(string/length s)`: The number of characters in s.
- `(string/concat s ...)`: The strings joined together.
- `(string/split s sep)`: The parts of s around each sep.
- `(string/join parts sep)`: The strings in parts, with sep between them.
- `(string/lines s)`: The lines of s, without their line endings.
- `(string/trim s)`: s without white space at either end.
- `(string/upper s)`: s in upper case.
- `(string/lower s)`: s in lower case.
- `(string/contains? s sub)`: Whether sub is in s.
- `(string/starts-with? s prefix)`: Whether s starts with prefix.
- `(string/ends-with? s suffix)`: Whether s ends with suffix.
- `(string/replace s old new)`: s with every old replaced by new.
- `(string/index s sub)`: (some i): where sub first is in s, counting characters; (none) if it isn't.
- `(string/slice s start [end])`: The characters of s from start up to end, or to the end of s.

### int

- `(int/from-string s)`: (ok n): s read as an int.
- `(int/to-string n)`: n in decimal.
- `(int/from-float x)`: (ok n): x without its fraction.
- `(int/div a b)`: a divided by b, without the remainder.
- `(int/rem a b)`: The remainder of a divided by b.

### float

- `(float/from-string s)`: (ok x): s read as a float.
- `(float/to-string x)`: x as puff prints it.
- `(float/from-int n)`: n as a float.
- `(float/div a b)`: a divided by b.
- `(float/round x)`: x rounded to the nearest whole number, halves away from zero.
- `(float/floor x)`: The greatest whole number at most x.
- `(float/ceil x)`: The least whole number at least x.

### value

- `(value/to-string v)`: v printed as puff reads it.
- `(value/from-string s)`: (ok v): s read as a value.
- `(value/type v)`: v's type: :bool, :int, :float, :string, :keyword, :duration, :vector, :map, :tag or :function.

### vector

- `(vector/length v)`: The number of items in v.
- `(vector/empty? v)`: Whether v has no items.
- `(vector/get v i)`: (some x): the item at i, from 0; (none) if there isn't one.
- `(vector/first v)`: (some x): the first item; (none) if v is empty.
- `(vector/last v)`: (some x): the last item; (none) if v is empty.
- `(vector/append v x ...)`: v with the xs added at the end.
- `(vector/concat v ...)`: The items of every v, in order.
- `(vector/set v i x)`: v with the item at i, from 0, replaced by x.
- `(vector/slice v start [end])`: The items from start up to end, or to the end of v.
- `(vector/reverse v)`: v's items in reverse order.
- `(vector/contains? v x)`: Whether an item of v equals x.
- `(vector/range start end)`: The ints from start up to end.
- `(vector/sort v)`: v's items in ascending order: ints, floats, strings or durations.
- `(vector/map v f)`: f of each item.
- `(vector/filter v f)`: The items for which f is #t.
- `(vector/find v f)`: (some x): the first item for which f is #t; (none) if there isn't one.
- `(vector/reduce v init f)`: (f (f init x1) x2) ... over the items.
- `(vector/each v f)`: Call f on each item for its effect: (ok :done), or the first (err e) f returns.

### map

- `(map/get m key)`: (some v): the value under key; (none) if there isn't one.
- `(map/get-or m key default)`: The value under key, or default.
- `(map/has? m key)`: Whether m has key.
- `(map/set m key v)`: m with key set to v.
- `(map/update m key f)`: m with key's value v replaced by (f v); a key m doesn't have faults.
- `(map/delete m key)`: m without key.
- `(map/merge m ...)`: The maps' entries together; a later map's value wins.
- `(map/keys m)`: m's keys, in print order.
- `(map/values m)`: m's values, in the order of its keys.
- `(map/entries m)`: m's [key value] pairs, in print order.
- `(map/from-entries pairs)`: A map of [key value] pairs; a later pair's value wins.
- `(map/length m)`: The number of entries in m.
- `(map/empty? m)`: Whether m has no entries.

### regex

- `(regex/match? s re)`: Whether re matches somewhere in s. Patterns are Go's RE2 syntax.
- `(regex/find s re)`: (some m): the first match of re in s; (none) if there isn't one.
- `(regex/find-all s re)`: Every match of re in s.
- `(regex/groups s re)`: (some [g1 g2 ...]): the groups of the first match; (none) if there isn't one.
- `(regex/replace s re new)`: s with every match of re replaced by new, where $1 is a group.
- `(regex/split s re)`: The parts of s around each match of re.

### json

- `(json/parse s [opts])`: (ok v): s read as JSON. Objects are maps, with keyword keys where the key is a name; null is (null); whole numbers that fit stay ints.
- `(json/to-string v [opts])`: v as JSON; {:indent n} indents by n spaces.
- `(json/null)`: JSON's null: (null).

### io

- `(io/println v ...)`: Print the values separated by spaces, strings as they are, then a newline.

### env

- `(env/get name)`: (some s): the environment variable name; (none) if it isn't set.

### os

- `(os/cwd)`: The working directory.

### path

- `(path/join part ...)`: The parts joined with /, cleaned.
- `(path/base p)`: The last element of p.
- `(path/dir p)`: p without its last element.
- `(path/ext p)`: p's extension, with its dot: ".txt"; "" if it has none.
- `(path/abs p)`: p from the root, relative to the working directory.

### file

- `(file/read-text path [opts])`: (ok s): the file's contents.
- `(file/write path s)` `:write`: Write s to the file, replacing what it held.
- `(file/write-atomic path s)` `:write`: Write s to the file so a reader sees either the old contents or all of s.
- `(file/append path s)` `:write`: Add s to the end of the file, creating it if there isn't one.
- `(file/mkdir path)` `:write`: Make the directory at path, and any parents it needs; :done if it is already there.
- `(file/exists? path)`: Whether there is a file or directory at path.
- `(file/list dir)`: (ok names): the names in dir, sorted.

### proc

- `(proc/run cmd [opts])` `:write`: Run cmd, a vector of the program and its arguments, without a shell: (ok {:exit :stdout :stderr}), whatever the exit code. Options: :stdin, a string; :dir; :timeout, a duration.
- `(proc/output cmd [opts])` `:write`: Run cmd as proc/run does: (ok stdout) if it exits 0, else (err {:kind :exit :exit :stdout :stderr}) with its stderr in :message.

### time

- `(time/now)`: The time now: 2026-10-27T18:25:00.123456Z.
- `(time/from-string s)`: (ok t): s read as an RFC 3339 time, with any offset.
- `(time/to-string t [opts])`: t in RFC 3339, in UTC or in {:zone "Europe/Warsaw"}.
- `(time/from-unix-ms ms)`: The time ms milliseconds after 1970-01-01 UTC.
- `(time/to-unix-ms t)`: Milliseconds from 1970-01-01 UTC to t.
- `(time/since t)`: The duration from t to now.

## whiff

Only in whiff: in services, `whiff eval` and `whiff repl`. In a handler,
`call`, `send` or `send-after` with a literal target counts as that target's
mark rather than `:write`, and `publish` to a literal topic as its subscribers'.

- `(describe address)`: (ok fns): address's functions, each {:fn :params :doc :mark :form}.
- `(reset service)` `:write`: Deletes service's stored state and reloads it from its file, so it starts from its :state again: (ok :done), or (err e).
- `(inbox [fn])`: This service's messages waiting for a reply, oldest first, each {:id :fn :args :from :cwd :at}; only fn's, a keyword, if given.
- `(reply id result)` `:write`: Answer the message id in this service's inbox with result: its sender gets [:whiff/done id result]. (err {:kind :no-id}) if it isn't there.
- `(self)`: This service's address, as a keyword: :assistant, or :try/3/assistant in whiff try.
- `(hand-on r)`: r; if r is the pending of a job or ask this service started, its answer goes to the caller of the message being handled instead. (err {:kind :no-id}) for another pending.
- `(publish topic v)` `:write`: Send v to every service subscribed to topic, a keyword: :done at once; pending if the review gate holds it.
- `(call target [arg] ...)` `:write`: Send target, :address/fn, a message and wait for its Result; (err {:kind :timeout}) after 5s.
- `(send target [arg] ...)` `:write`: Send target, :address/fn, a message without waiting: :done once it is in the mailbox.
- `(send-after d target [arg] ...)` `:write`: Send target a message once d has passed; :done at once.
