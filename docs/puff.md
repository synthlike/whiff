# Learn puff

puff is whiff's small lisp. You can use it for scripts without a daemon,
then use the same language to write services. This guide covers the
language; the [walkthrough](walkthrough.md) introduces service state and
messages, and the [standard library](stdlib.md) lists the functions.

## Run a few expressions

Install the binaries from the whiff checkout with `go install ./cmd/...`.
Run `puff` for a REPL, or evaluate an expression:

```sh
puff -e '(+ 1 2)'
# 3
```

Parentheses call a function: `(+ 1 2)` calls `+` with two arguments.
`;` starts a comment that runs to the end of the line.

## Values and names

```lisp
(def person {:name "Ann" :roles [:dev :reviewer] :active #t})
(def delay 5m)
(def release 2026-11-05T12:00:00Z)
```

Strings use double quotes, keywords start with `:`, and booleans are `#t`
and `#f`. Numbers include ints and floats; durations such as `250ms`, `2h`
and `1d` and RFC 3339 times are values too. `1d` means 24 hours.
Vectors use `[...]`; maps use `{key value ...}`. Collections can contain
other collections. Commas aren't needed.

`def` names a value or function at the top level. `let` names local values,
in order, and evaluates its body with those names available:

```lisp
(let [name "Ann"
      greeting (str "hello, " name)]
  greeting)
; "hello, Ann"
```

Values are immutable. Updating a map or vector makes a new value:

```lisp
(let [before {:count 1}
      after (map/update before :count (fn (n) (+ n 1)))]
  [before after])
; [{:count 1} {:count 2}]
```

## Functions and collection operations

```lisp
(def (greet name)
  (str "hello, " name))

(vector/map ["Ann" "Bo"] greet)
; ["hello, Ann" "hello, Bo"]
```

`fn` makes a function without naming it. `vector/filter` keeps values
whose function returns `#t`; `vector/reduce` carries an accumulator:

```lisp
(let [numbers [1 2 3 4]
      large (vector/filter numbers (fn (n) (> n 2)))]
  (vector/reduce large 0 (fn (sum n) (+ sum n))))
; 7
```

`->` puts the preceding value in the first argument position of each call:

```lisp
(-> "  hello  "
  (string/trim)
  (string/upper))
; "HELLO"
```

## Conditions and patterns

Conditions must be booleans. There is no truthiness: `0`, `""` and `[]`
aren't substitutes for `#f`. Use `if` for two branches and `cond` for a
sequence of tests:

```lisp
(def (size n)
  (cond
    [(< n 0) :negative]
    [(= n 0) :zero]
    [_ :positive]))
```

`match` chooses a branch by the shape of a value. Names bind matching
parts, literals must match, and `_` ignores a value:

```lisp
(def (describe task)
  (match task
    [{:done #t :title title} (str "done: " title)]
    [{:title title} (str "open: " title)]
    [_ "not a task"]))
```

A map pattern requires the named fields and allows additional ones. A
vector pattern matches its elements; `[first & rest]` captures the tail.
An arm can add a guard: `[n when (> n 0) :positive]`. `when` only appears
in match arms; it isn't a standalone conditional.

Patterns also work in `let` bindings and function parameters:

```lisp
(vector/map [{:title "docs" :done #f} {:title "tests" :done #t}]
  (fn ({:title title}) title))
; ["docs" "tests"]
```

A pattern that doesn't fit in a binding or parameter is a fault, as is a
`match` with no matching arm. Use a final `_` arm when other inputs are
expected.

## Options and Results

There is no nil. A lookup returns an Option: `(some value)` or `(none)`.
An operation that can fail returns a Result: `(ok value)` or `(err error)`.

```lisp
(match (map/get {:port 8080} :port)
  [(some port) (str "listen on " port)]
  [(none) "no port configured"])
```

`or-else` supplies a default for an empty Option or failed Result.
`unwrap` extracts a value and faults if there isn't one; use it for an
invariant, rather than an expected missing value.

`try` unwraps an `(ok value)` or returns an `(err error)` from the
enclosing function. It lets successful work read in order:

```lisp
(def (read-json path)
  (let [text (try (file/read-text path))
        value (try (json/parse text))]
    (ok value)))
```

`catch` handles errors by matching their contents and leaves unmatched
errors alone:

```lisp
(def (read-or-empty path)
  (catch (file/read-text path)
    [{:kind :not-found} (ok "")]))
```

Errors are values, but silently dropping one between expressions is a
fault. In `do`, use `try` to propagate a failure, `match` or `catch` to
handle it, or `or-else` to choose a fallback. The last expression is the
body's result. Reader and evaluator errors name their `file:line:col`.

## A script with tests

Save this as `tasks.puff`:

```lisp
(def (open-titles tasks)
  (vector/map (vector/filter tasks (fn ({:done done}) (not done)))
    (fn ({:title title}) title)))

(test "only open tasks are listed"
  (= (open-titles [{:title "docs" :done #f} {:title "tests" :done #t}])
    ["docs"]))

(def (main args)
  (match args
    [[path]
     (let [tasks (try (json/parse (try (file/read-text path))))]
       (do (io/println (open-titles tasks))
         (ok :done)))]
    [_ (err {:kind :usage :message "puff tasks.puff tasks.json"})]))
```

Create some input, format the script, run its test, then run it:

```sh
printf '%s\n' '[{"title":"docs","done":false},{"title":"tests","done":true}]' > tasks.json
puff -f tasks.puff
puff -t tasks.puff
puff tasks.puff tasks.json
# ["docs"]
```

A script's `main` receives a vector of command-line strings. An `(ok ...)`
result exits successfully, an `(err ...)` exits 1, and a fault exits 70.
`-t` runs tests without calling `main` and also checks formatting. Tests
pass when their result is `#t`; equality tests show both values on failure.

## Share code, then add messages

Move reusable definitions and tests into a file, then load it with
`(use "lib/tasks.puff")`. A used file contains only definitions, tests
and other uses. Paths are relative to the source file, following its
symlink if it has one. Names share a namespace; duplicate definitions
and circular uses are errors. Tests in used files run too.

Standalone puff has file, process, HTTP and collection functions. whiff
adds service forms, addresses, messages, jobs and views. A service handles
one message at a time; concurrency belongs to the daemon. Continue with
the [message walkthrough](walkthrough.md) to turn these values and
functions into a running service.
