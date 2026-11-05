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

The language's own forms, `def let fn if cond do and or match when loop recur -> try catch test`,
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
- `(min a b ...)`: The least of the values, as < compares them.
- `(max a b ...)`: The greatest of the values, as > compares them.
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
- `(string/replace-first s old new)`: s with the first old replaced by new.
- `(string/trim-prefix s prefix)`: s without prefix at its start; s if it doesn't start with it.
- `(string/trim-suffix s suffix)`: s without suffix at its end; s if it doesn't end with it.
- `(string/cut s sep)`: (some [before after]): s around the first sep; (none) if sep isn't in s.
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

### keyword

- `(keyword/name k)`: k's name, without its colon: "ready" for :ready.
- `(keyword/from-string s)`: (ok k): the keyword named s, :ready for "ready", as the reader reads it; (err {:kind :parse}) for a name it wouldn't read as one.

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
- `(vector/any? v f)`: Whether f is #t for some item.
- `(vector/all? v f)`: Whether f is #t for every item; #t for no items.
- `(vector/reduce v init f)`: (f (f init x1) x2) ... over the items.
- `(vector/each v f)`: Call f on each item for its effect: (ok :done), or the first (err e) f returns.

### map

- `(map/get m key)`: (some v): the value under key; (none) if there isn't one.
- `(map/get-or m key default)`: The value under key, or default.
- `(map/has? m key)`: Whether m has key.
- `(map/set m key v)`: m with key set to v.
- `(map/update m key f)`: m with key's value v replaced by (f v); a key m doesn't have faults.
- `(map/get-in m path)`: (some v): the value at path, a vector of keys, through nested maps; (none) if there isn't one.
- `(map/set-in m path v)`: m with the value at path, a vector of keys, set to v, making the maps on the way that m doesn't have.
- `(map/update-in m path f)`: m with the value v at path, a vector of keys, replaced by (f v); a path m doesn't have faults.
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

- `(os/home)`: Your home directory.
- `(os/cwd)`: The working directory.

### uuid

- `(uuid/new)`: A new random UUID, in lower case: "0b6f5f1e-...".

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

- `(proc/run cmd [opts])` `:write`: Run cmd, a vector of the program and its arguments, without a shell: (ok {:exit :stdout :stderr}), whatever the exit code. Options: :stdin, a string; :dir; :timeout, a duration. :read for a literal cmd in the table of read-only commands, such as ["git" "status"].
- `(proc/output cmd [opts])` `:write`: Run cmd as proc/run does: (ok stdout) if it exits 0, else (err {:kind :exit :exit :stdout :stderr}) with its stderr in :message. :read as proc/run is.

### http

- `(http/request url [opts])` `:write`: Send an HTTP request to url: (ok {:status :headers :body}), whatever the status, with header names in lower case. Options: :method, a keyword (:get unless given); :headers, a map of names to strings; :body, a string; :timeout, a duration (30s unless given). A request that fails to get an answer is (err {:kind :http}), or :timeout or :too-big.
- `(http/get url [opts])` `:write`: GET url as http/request does: (ok body) for a 2xx status, else (err {:kind :status :status :body}).

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

- `(address/describe address)`: (ok fns): address's functions, each {:fn :params :doc :mark :form}, and :yours #t on one only your connections may use.
- `(service/reset service)` `:write`: Deletes service's stored state and reloads it from its file, so it starts from its :state again: (ok :done), or (err e).
- `(address/list)`: (ok addrs): every address, sorted, each {:address :client :state}: :client for a client's, :state for a service with :state.
- `(service/list)`: (ok services): each service file, what needs you first, as {:service :agent :status :note}: :status one of :failed, :faulting, :over-budget, :paused, :waiting, :thinking, :busy, :idle or :ok, and :note what is most worth knowing, or "".
- `(service/status service)`: (ok s): service's line of service/list, {:service :agent :status :note}, and what else whiff knows of it: :file, :load-error, :last {:fn :at} handled, :faults today and the last :fault {:at :message}, :inbox and :timers [{:fn :due}]; for an agent :spent, :turns and :day of the last 24 hours, and :tasks-spent, what of :spent its tasks cost, as a build's dev, :last-turn, :every, its session's :mode, as (session/mode) gives it, if it isn't gated, and its :work, the tasks its turns started in the last day, at most 20: [{:id :to :status :at :children}], :waiting or :held with their :fn and :why, what each waits on, or :ended with their :result cut short, and :children, what each one's work began, as :tasks/tree gives them, and :actions, as :tasks/view has them. (err {:kind :no-address}) if there is none.
- `(service/state service)`: (ok state): service's state as it last committed it; (err {:kind :stateless}) for a service without :state, (err {:kind :no-address}) if there is none.
- `(inbox [fn])`: This service's messages waiting for a reply, oldest first, each {:id :fn :args :from :cwd :at}, and :session, the agent session whose work it is, if one's; only fn's, a keyword, if given.
- `(reply id result)`: Answer the message id in this service's inbox with result: its sender gets [:whiff/done id result]. (err {:kind :no-id}) if it isn't there.
- `(self)`: This service's address, as a keyword: :assistant, or :try/3/assistant in whiff try.
- `(project/name [dir])`: (some name): the project of the work this message is in, once a job in its chain ran in a git repository; given dir, the project of work there, the name of the repository it is in, a worktree's being its repository's. (none) outside one.
- `(session/tasks)`: (ok ids): for an agent, the tasks its turns' session started that haven't ended, oldest first: its messages waiting in an inbox or for your review. (err {:kind :no-session}) for a service that isn't an agent.
- `(session/cancel)` `:write`: (ok ids): for an agent, cancels each task its turns' session started that hasn't ended, as whiff cancel does, and gives their ids, oldest first; a service whose inbox one waited in hears whiff/cancelled. (err {:kind :no-session}) for a service that isn't an agent.
- `(session/mode)`: (ok m): this agent session's mode, {:mode :gated}, {:mode :free} or {:mode :trusted :services [:todo ...]}, with :until, a time, if it runs out. (err {:kind :no-session}) for anyone else.
- `(hand-on r)`: r; if r is the pending of a job or ask this service started, its answer goes to the caller of the message being handled instead. (err {:kind :no-id}) for another pending.
- `(submitted r)`: (ok id) if r is a pending, of a job, an ask or a message the review gate holds: it was accepted, and [:whiff/done id result] answers it; else r, so a failure to submit stays an err. hand-on wants the pending itself.
- `(publish topic v)` `:write`: Send v to every service subscribed to topic, a keyword: :done at once; pending if the review gate holds it.
- `(call target [arg] ...)` `:write`: Send target, :address/fn, a message and wait for its Result; (err {:kind :timeout}) after 5s.
- `(send target [arg] ...)` `:write`: Send target, :address/fn, a message without waiting: :done once it is in the mailbox.
- `(send-after d target [arg] ...)` `:write`: Send target a message once d has passed; :done at once.
- `(view/items items [opts])`: (items items): a view of items to pick from, each {:label "..."} with an optional :detail string, :tag, a word that sets it apart, as its project, which sniff shows in a colour of its own, :url, an http or https link to its page, which a client shows, and :actions, what can be done with it, each {:label "Move to Done" :action [:address/fn args...]}, the message picking it sends, and an optional :ask, a question whose answer, text, a client adds as its last argument, "" if you give none; :action alone is one action without a label. opts, {:changed :address/topic}, names the topic published when it changes, or a vector of them.
- `(view/text text [opts])`: (text text): a view of text, shown as it is. opts, {:changed :address/topic}, names the topic published when it changes, or a vector of them.
- `(view/locations locations [opts])`: (locations locations): a view of places in files, each {:file "..."} with an optional :line and :col, counting from 1, and :note string. opts, {:changed :address/topic}, names the topic published when it changes, or a vector of them.
- `(view/board columns [opts])`: (board columns): a view of items in columns, each {:title "..." :items [...]}, its items as view/items has them. opts, {:changed :address/topic}, names the topic published when it changes, or a vector of them.
