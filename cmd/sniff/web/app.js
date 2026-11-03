// sniff's page: history from sniff's event stream, drawn as chains, each
// message under the one whose handler sent it, or as a lane per address
// and an arrow per message; details from its JSON endpoints, with
// values and views drawn by sniff (render); and views as tabs beside it,
// each its /view page drawn in this one.
"use strict";

const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
};

async function getJSON(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error((await r.text()).trim());
  return r.json();
}

// print is a tree, as sniff sends values, printed as puff prints it.
function print(t) {
  switch (t.t) {
    case "map": return "{" + t.v.map(([k, v]) => print(k) + " " + print(v)).join(" ") + "}";
    case "vec": return "[" + t.v.map(print).join(" ") + "]";
    case "tag": return "(" + [t.n, ...t.v.map(print)].join(" ") + ")";
    case "kw": return ":" + t.v;
    case "str": return JSON.stringify(t.v);
  }
  return t.v;
}

// drawn is a box that sniff fills with a tree drawn: as a reply, with the
// view it holds, if as is "reply"; else folded as puff.
function drawn(t, as) {
  const box = el("div", as === "reply" || as === "answer" ? "" : "value", "…");
  fetch("render" + (as ? "?as=" + as : ""), { method: "POST", headers: { "Content-Type": "text/plain" }, body: print(t) })
    .then(async (r) => {
      const html = await r.text();
      if (!r.ok) throw new Error(html.trim());
      box.innerHTML = html; // sniff drew it, escaping what it shows
    })
    .catch((err) => { box.textContent = err.message; });
  return box;
}

// marks is each function's mark, "read" or "write", by its target,
// "plan/board", from sniff's addresses.
let marks = new Map();

// pageOf is the page that shows what target answers to args, a tree of a
// vector, as a view: /view/pipeline/show?args=[2].
function pageOf(target, args) {
  return "/view/" + encodeURI(target) + (args.v.length ? "?args=" + encodeURIComponent(print(args)) : "");
}

let entries = []; // history, oldest first
let lanes = []; // addresses, in the order they first appear; "client" first
let selected = null; // the row selected
let bulk = false; // taking a snapshot: lanes are drawn once, after it
let shown = "chains"; // how history is drawn: "chains" or "lanes"
const folded = new Set(); // the ids of the chains folded to their heading
let only = null; // the id of the one chain shown, or null for every one

// Addresses of an entry: where it comes from and where it goes. A client has
// no address; to is address/fn, or address on :topic.
function from(e) { return e.from || "client"; }
function to(e) {
  let t = e.to;
  if (!t) return "client";
  if (t.endsWith("/whiff/done")) return t.slice(0, -"/whiff/done".length);
  const on = t.indexOf(" on :");
  if (on >= 0) return t.slice(0, on);
  return t.slice(0, t.lastIndexOf("/"));
}
function fn(e) {
  if (e.kind === "pub") return ":" + e.to.slice(e.to.indexOf(" on :") + 5);
  if (e.kind === "done") return "done";
  return e.to.slice(e.to.lastIndexOf("/") + 1);
}

// kindOf is the reply's mark: "ok", "err", "fault" or "pending".
function kindOf(reply) {
  if (!reply || reply.t !== "tag") return "";
  if (reply.n === "ok") return "ok";
  const e = reply.v[0];
  if (e && e.t === "map") {
    for (const [k, v] of e.v) {
      if (k.t === "kw" && k.v === "kind" && v.t === "kw") {
        if (v.v === "fault") return "fault";
        if (v.v === "pending") return "pending";
      }
    }
  }
  return "err";
}

function laneOf(addr) {
  let i = lanes.indexOf(addr);
  if (i < 0) {
    lanes.push(addr);
    i = lanes.length - 1;
    // A new address shows in the sidebar too.
    if (addr !== "client" && !addresses.some((a) => a.address === addr)) reloadSoon();
    if (!bulk) {
      drawLanes();
      redraw(); // every arrow moves
    }
  }
  return i;
}

function drawLanes() {
  const head = $("lanes");
  head.replaceChildren(el("span"), el("div", "names"), el("span"));
  head.style.setProperty("--lanes", lanes.length);
  for (const a of lanes) {
    const b = el("button", a === "client" ? "client" : "", a === "client" ? "client" : ":" + a);
    if (a !== "client") b.onclick = () => showAddress(a);
    head.children[1].append(b);
  }
}

// time is when at was to the second, as rows show it; fullTime to the
// millisecond, as their titles do.
function time(at) {
  return new Date(at).toTimeString().slice(0, 8);
}
function fullTime(at) {
  const d = new Date(at);
  return time(at) + "." + String(d.getMilliseconds()).padStart(3, "0");
}

function took(ms) {
  if (!ms) return "";
  if (ms < 1) return (ms * 1000).toFixed(0) + "µs";
  if (ms < 1000) return ms.toFixed(ms < 10 ? 1 : 0) + "ms";
  return (ms / 1000).toFixed(1) + "s";
}

function row(e, i) {
  const r = el("div", "row " + e.kind);
  r.dataset.i = i;
  r.append(el("span", "time", time(e.at)));
  if (e.kind === "note") {
    r.classList.add("noted");
    r.append(el("div", "note-text", e.note));
    return r;
  }
  if (e.origin === "agent") r.classList.add("agent");
  const k = kindOf(e.reply);
  if (k) r.classList.add(k);
  const a = laneOf(from(e)), b = laneOf(to(e)), n = lanes.length;
  const track = el("div", "track");
  track.style.setProperty("--lanes", n);
  const arrow = el("div", "arrow");
  const label = el("div", "label");
  let args = e.args ? print(e.args).slice(1, -1) : "";
  if (e.kind === "done" && e.args && e.args.v.length) args = print(e.args.v[0]);
  label.textContent = fn(e) + (args ? " " + args : "");
  if (e.gate) label.append(" ", el("span", "gate", "[" + e.gate + "]"));
  label.title = label.textContent;
  if (a === b) {
    arrow.classList.add("self");
    arrow.style.left = `calc(${(a + 0.5) / n * 100}%)`;
    arrow.style.width = `calc(${0.3 / n * 100}%)`;
    label.style.left = `calc(${(a + 0.5 + 0.32) / n * 100}%)`;
    label.style.maxWidth = `calc(${(n - a - 0.85) / n * 100}%)`;
  } else {
    const lo = Math.min(a, b), hi = Math.max(a, b);
    arrow.classList.add(b > a ? "right" : "left");
    arrow.style.left = `${(lo + 0.5) / n * 100}%`;
    arrow.style.width = `${(hi - lo) / n * 100}%`;
    label.style.left = `${(lo + 0.5) / n * 100}%`;
    label.style.width = `${(hi - lo) / n * 100}%`;
  }
  track.append(arrow, label);
  r.append(track, el("span", "took", took(e.took)));
  return r;
}

// Chains: each entry that began one, then what it led to, as each began,
// indented under what led to it. One whose cause isn't here, gone from
// history or left out by the filter, begins a chain of its own. A chain of
// more than one message is headed by the message that began it, whose
// fold hides the rest.
function drawChains(frag) {
  const ids = new Set(entries.map((e) => e.id).filter(Boolean));
  const led = new Map(); // the indexes of what each id led to, as they began
  const roots = [];
  entries.forEach((e, i) => {
    if (!ids.has(e.cause)) return roots.push(i);
    if (!led.has(e.cause)) led.set(e.cause, []);
    led.get(e.cause).push(i);
  });
  const walk = (i, depth, chain) => {
    chain.push([i, depth]);
    for (const j of led.get(entries[i].id) || []) walk(j, depth + 1, chain);
    return chain;
  };
  // Reads a client made one after another, as a page's polling does, fold
  // into one row, "client: 6 reads", that opens to show them. Another
  // message alone that repeats the one alone before it, the same call with
  // the same arguments and kind of reply, folds into it: the latest is
  // drawn, with how many times.
  let last = null; // {key, row, n}
  let reads = []; // the indexes of the reads in a row so far
  const flush = () => {
    if (reads.length === 1) frag.append(chainRow(entries[reads[0]], reads[0], 0));
    else if (reads.length) frag.append(readsBox(reads));
    reads = [];
  };
  for (const i of roots) {
    if (only && entries[i].id !== only) continue;
    const chain = walk(i, 0, []);
    const e = entries[i];
    if (chain.length === 1 && !e.from && e.kind === "call" && marks.get(e.to) === "read" && kindOf(e.reply) === "ok") {
      reads.push(i);
      last = null;
      continue;
    }
    flush();
    if (chain.length === 1) {
      const r = chainRow(e, i, 0);
      const key = e.kind === "note" ? "note " + e.note : [e.kind, e.from, e.to, e.args ? print(e.args) : "", kindOf(e.reply)].join(" ");
      if (key && last?.key === key) {
        last.n++;
        (r.querySelector(".args") || r.querySelector(".note-text")).append(" ", el("span", "times", "×" + last.n));
        last.row.replaceWith(r);
        last.row = r;
      } else {
        frag.append(r);
        last = { key, row: r, n: 1 };
      }
      continue;
    }
    last = null;
    // The message that began it heads it, with a fold for the rest.
    const box = el("div", "chain"), id = entries[i].id, shut = folded.has(id);
    const head = chainRow(e, i, 0);
    head.classList.add("chain-root");
    const fold = el("button", "fold", shut ? "▸" : "▾");
    fold.type = "button";
    fold.dataset.id = id;
    fold.title = (shut ? "open" : "fold") + ` what it led to: ${chain.length - 1} more`;
    fold.setAttribute("aria-expanded", String(!shut));
    // In the arrow's place, as what it began is under it.
    head.querySelector(".glyph").replaceWith(fold);
    if (shut) head.querySelector(".args").append(" ", el("span", "times", `+${chain.length - 1}`));
    box.append(head);
    if (!shut) for (const [j, depth] of chain.slice(1)) box.append(chainRow(entries[j], j, depth, true));
    frag.append(box);
  }
  flush();
}

// readsBox is a client's reads in a row, the entries at is: "client: 6
// reads", and what they read, which opens to show them.
const openReads = new Set(); // the ids of the first of the runs of reads opened
function readsBox(is) {
  const first = entries[is[0]].id, open = openReads.has(first);
  const box = el("div", "reads");
  const h = el("button", "reads-head");
  h.type = "button";
  h.dataset.id = first;
  h.setAttribute("aria-expanded", String(open));
  const what = [...new Set(is.map((i) => entries[i].to))].join(", ");
  h.title = time(entries[is[0]].at) + "–" + time(entries[is.at(-1)].at);
  h.append(el("span", "time", time(entries[is.at(-1)].at)), el("span", "fold", open ? "▾" : "▸"), `${is.length} reads`, el("span", "what-read", what));
  box.append(h);
  if (open) for (const i of is) box.append(chainRow(entries[i], i, 0));
  return box;
}

// In a chain, whose heading says when it began, a row's time is only in
// its title.
function chainRow(e, i, depth, inChain = false) {
  const r = el("div", "row " + e.kind);
  r.dataset.i = i;
  r.append(el("span", "time", inChain ? "" : time(e.at)));
  if (e.kind === "note") {
    r.classList.add("noted");
    r.append(el("div", "note-text", e.note));
    return r;
  }
  if (e.origin === "agent") r.classList.add("agent");
  const k = kindOf(e.reply);
  if (k) r.classList.add(k);
  const what = el("span", "what");
  what.style.setProperty("--depth", depth);
  what.append(el("span", "glyph", { send: "⇢", pub: "◇", done: "↩", eval: "λ" }[e.kind] || "→"), (e.from ? ":" + e.from : "client") + " → ");
  if (e.kind === "eval") what.append(el("b", "", "eval"));
  else if (!e.to) what.append("client");
  else what.append(":" + to(e) + (e.kind === "pub" ? " on " : "/"), el("b", "", fn(e)));
  let args = e.args ? print(e.args).slice(1, -1) : "";
  if (e.kind === "done" && e.args && e.args.v.length) args = print(e.args.v[0]);
  const rest = el("span", "args", args);
  if (e.gate) rest.append(" ", el("span", "gate", "[" + e.gate + "]"));
  r.title = fullTime(e.at) + "  " + what.textContent + " " + rest.textContent;
  // ok is the rule, so it goes unsaid: only what isn't ok has a word.
  r.append(what, rest, el("span", "result", e.gate?.startsWith("held") ? "held" : k === "ok" ? "" : k), el("span", "took", took(e.took)));
  return r;
}

function redraw() {
  const rows = $("rows");
  // A heading you went to, by j and k or a click, stays where you are.
  const f = rows.contains(document.activeElement) ? document.activeElement : null;
  const again = f?.dataset.id ? `.${f.classList[0]}[data-id="${f.dataset.id}"]` : null;
  const frag = document.createDocumentFragment();
  if (shown === "chains") drawChains(frag);
  else entries.forEach((e, i) => frag.append(row(e, i)));
  rows.replaceChildren(frag);
  $("empty").hidden = entries.length > 0;
  if (selected !== null) mark(selected);
  if (again) rows.querySelector(again)?.focus({ preventScroll: true });
}

// redrawSoon redraws once for the entries that come in a frame, and keeps
// the end in sight if stick was for any of them.
let soon = null, sticking = false;
function redrawSoon(stick) {
  sticking ||= stick;
  soon ??= requestAnimationFrame(() => {
    soon = null;
    redraw();
    if (sticking) $("trace").scrollTop = $("trace").scrollHeight;
    sticking = false;
  });
}

// showOnly shows only the chain that id began, as chains; or, for null,
// every chain again.
function showOnly(id) {
  only = id;
  $("only").hidden = !id;
  if (id && shown !== "chains") showAs("chains");
  else redraw();
  if (selected !== null) showEntry(selected);
}
$("only").onclick = () => showOnly(null);

// showAs draws history as chains or lanes, and remembers it.
function showAs(how) {
  shown = how;
  store.set("activity", how);
  if (how === "lanes") { // lanes show every message
    only = null;
    $("only").hidden = true;
  }
  for (const b of document.querySelectorAll("[data-show]")) b.setAttribute("aria-pressed", String(b.dataset.show === how));
  $("lanes").hidden = how !== "lanes";
  $("rows").classList.toggle("chains", how === "chains");
  redraw();
}

// append adds e in order of when it began: history records a call when it
// ends, after the calls it made.
function append(e) {
  const trace = $("trace");
  const atEnd = trace.scrollHeight - trace.scrollTop - trace.clientHeight < 40;
  let i = entries.length;
  while (i > 0 && entries[i - 1].us > e.us) i--;
  entries.splice(i, 0, e);
  if (noteFault(e)) drawAddresses();
  servicesSoon();
  if (selected !== null && selected >= i) selected++;
  $("empty").hidden = true;
  // A chain can gain a message anywhere, or one that began it after the
  // rest: it is drawn again.
  if (shown === "chains") {
    if (e.kind !== "note") { laneOf(from(e)); laneOf(to(e)); }
    redrawSoon($("follow").checked && atEnd);
    return;
  }
  const n = lanes.length;
  const r = row(e, i);
  if (lanes.length === n) { // else redraw drew it
    // Only the rows after it move: renumber them rather than draw them all.
    const rows = $("rows");
    for (let j = i; j < rows.children.length; j++) rows.children[j].dataset.i = j + 1;
    rows.insertBefore(r, rows.children[i] || null);
  }
  if ($("follow").checked && atEnd) trace.scrollTop = trace.scrollHeight;
}

function mark(i) {
  document.querySelectorAll(".row.selected").forEach((r) => r.classList.remove("selected"));
  const r = document.querySelector(`.row[data-i="${i}"]`);
  if (r) r.classList.add("selected");
}

function section(title, ...kids) {
  const f = document.createDocumentFragment();
  f.append(el("h3", "", title), ...kids);
  return f;
}

function showEntry(i) {
  history.replaceState(null, "", location.pathname + location.search);
  const e = entries[i];
  selected = i;
  mark(i);
  const d = $("detail");
  if (e.kind === "note") {
    d.replaceChildren(el("h2", "", "note"), el("p", "", e.note), el("p", "meta", time(e.at)));
    return;
  }
  const head = el("h2", "", `${e.kind}  ${e.from ? ":" + e.from : "client"} → ${e.to ? ":" + e.to : "client"}`);
  const meta = el("div", "meta");
  meta.append(el("span", "", time(e.at)), el("span", "badge", e.origin), el("span", "", took(e.took)));
  if (e.gate) meta.append(gateBadge(e.gate));
  d.replaceChildren(head, meta);
  const chain = chainOf(i);
  if (chain.length > 1) d.prepend(crumbs(chain));
  if (e.args) d.append(section("Arguments", drawn(e.args)));
  if (!e.reply) d.append(section("Reply", el("p", "hint", "None: nobody handled it.")));
  else d.append(section("Reply", drawn(e.reply, "reply")));
  const view = viewOf(e);
  if (view) {
    const a = el("a", "open", "Open :" + e.to + " as a tab");
    a.href = view;
    d.append(a);
  }
  d.append(entryActions(e, chain[0]), keysLine());
}

// viewOf is the page of the view e answered, or null: a view a :read
// function answers can be a tab of its own, where its actions work.
function viewOf(e) {
  const view = e.reply?.t === "tag" && e.reply.n === "ok" && e.reply.v[0]?.t === "tag";
  return view && e.kind === "call" && e.args && marks.get(e.to) === "read" ? pageOf(e.to, e.args) : null;
}

// keysLine is the keys that work over Activity.
function keysLine() {
  const p = el("p", "keys");
  const keys = [["j", "/", "k", " next/previous"], ["enter", " open"], ["/", " filter"], ["c", " call again"], [modK, " go to or call"]];
  keys.forEach((k, n) => {
    if (n) p.append(" · ");
    k.forEach((s, m) => p.append(m % 2 ? s : el("kbd", "", s)));
  });
  return p;
}

// chainOf is the indexes of what led to entry i, from the entry that began
// its chain, and i last.
function chainOf(i) {
  const at = new Map(entries.map((e, j) => [e.id, j]));
  const path = [i];
  for (let e = entries[i]; e.cause && at.has(e.cause); e = entries[path[0]]) path.unshift(at.get(e.cause));
  return path;
}

// crumbs is where an entry sits in its chain, path: who began it, then
// what each message was for, "agent/3 › plan/draft › pipeline/run"; each
// but the last shows its message.
function crumbs(path) {
  const nav = el("nav", "crumbs");
  nav.setAttribute("aria-label", "Its chain");
  const root = entries[path[0]];
  nav.append(root.from || (root.origin === "agent" ? "client" : "you"));
  path.forEach((j, n) => {
    const e = entries[j];
    const name = e.kind === "eval" ? "eval" : e.kind === "pub" ? fn(e) : e.kind === "done" ? "done" : e.to;
    nav.append(" › ");
    if (n === path.length - 1) return nav.append(el("b", "", name));
    const b = el("button", "", name);
    b.type = "button";
    b.onclick = () => showEntry(j);
    nav.append(b);
  });
  return nav;
}

// asPuff is e as puff that sends it again: (call :plan/draft "x"),
// (publish :topic v), an eval's code, or a done's [id result].
function asPuff(e) {
  const args = e.args ? print(e.args).slice(1, -1) : "";
  const form = (head) => "(" + head + (args ? " " + args : "") + ")";
  switch (e.kind) {
    case "eval": return e.args?.v[0]?.v ?? "";
    case "done": return e.args ? print(e.args) : "";
    case "pub": return form("publish " + fn(e));
    case "send": return form("send :" + e.to);
  }
  return form("call :" + e.to);
}

// entryActions are what you can do with e, whose chain began with entry
// root: copy it as puff, show only its chain, or call it again as you.
function entryActions(e, root) {
  const box = el("div", "entry-actions");
  const copy = el("button", "", "Copy as puff");
  copy.type = "button";
  copy.onclick = async () => {
    try {
      await navigator.clipboard.writeText(asPuff(e));
      copy.textContent = "Copied";
    } catch {
      copy.textContent = "Can't copy here";
    }
    setTimeout(() => { copy.textContent = "Copy as puff"; }, 1500);
  };
  box.append(copy);
  if (entries[root].id && only !== entries[root].id) {
    const b = el("button", "", "Only this chain");
    b.type = "button";
    b.onclick = () => showOnly(entries[root].id);
    box.append(b);
  }
  if ((e.kind === "call" || e.kind === "send") && e.to) {
    const again = el("details", "again");
    again.append(el("summary", "", "Call again…"), callBox("[:" + e.to + (e.args && e.args.v.length ? " " + print(e.args).slice(1, -1) : "") + "]"));
    box.append(again);
  }
  return box;
}

// callBox is a form that calls action, [:address/fn args...], as you,
// which you can edit first, with its reply shown under it, and then calls
// then, if given. It takes a call, never code.
function callBox(action, then) {
  const box = el("div", "call-box");
  const form = el("form");
  const input = el("input");
  input.name = "action";
  input.value = action;
  input.setAttribute("aria-label", "The message, as [:address/fn args...]");
  input.spellcheck = false;
  const out = el("div", "call-reply");
  form.append(input, el("button", "", "Call as you"));
  form.onsubmit = async (ev) => {
    ev.preventDefault();
    out.replaceChildren(el("p", "hint", "…"));
    try {
      const r = await fetch("api/call", { method: "POST", body: new URLSearchParams(new FormData(form)) });
      if (!r.ok) throw new Error((await r.text()).trim());
      out.replaceChildren(drawn(await r.json(), "answer"));
      then?.();
    } catch (err) {
      out.replaceChildren(el("p", "query-error", err.message));
    }
  };
  answers(out);
  box.append(form, out);
  return box;
}

// answers has out, where a reply just drawn as an answer shows, send the
// form of an action its err offers instead, as you, and show that reply
// in its place: the page stays where it is.
function answers(out) {
  out.addEventListener("submit", async (ev) => {
    if (!ev.target.closest(".next")) return;
    ev.preventDefault();
    const body = new URLSearchParams(new FormData(ev.target));
    out.replaceChildren(el("p", "hint", "…"));
    try {
      const r = await fetch("api/call", { method: "POST", body });
      if (!r.ok) throw new Error((await r.text()).trim());
      out.replaceChildren(drawn(await r.json(), "answer"));
    } catch (err) {
      out.replaceChildren(el("p", "query-error", err.message));
    }
  });
}

// gateBadge is how the review gate treated an agent's write: held or
// refused, or let through (accepted, trusted, free, or for a session).
function gateBadge(gate) {
  const kind = gate.split(" ")[0];
  return el("span", "badge " + (kind === "held" || kind === "refused" ? "held" : "through"), gate);
}

// showSession adds to d what the agent session name did, as whiff session
// shows it: its writes, each with its gate, and their summary. A line
// shows its message.
function showSession(d, name) {
  const box = el("div", "session", "…");
  d.append(section("Session", box));
  getJSON("api/session?address=" + encodeURIComponent(name)).then((s) => {
    box.replaceChildren(el("p", "", s.summary));
    for (const e of s.entries) {
      const b = el("button", "line");
      b.append(el("span", "time", time(e.at)), " ");
      if (e.kind === "note") {
        b.append(e.note);
      } else {
        const target = e.kind === "eval" ? "eval" : e.to ? ":" + e.to : "client";
        const args = e.args ? " " + print(e.args).slice(1, -1) : "";
        b.append(`${e.from ? ":" + e.from : "client"} → ${target}${args} `);
        if (e.gate) b.append(gateBadge(e.gate));
      }
      b.title = b.textContent;
      const i = entries.findIndex((x) => x.us === e.us && x.kind === e.kind && x.from === e.from && x.to === e.to);
      if (i < 0) {
        b.disabled = true;
      } else {
        b.onclick = () => {
          showEntry(i);
          document.querySelector(`.row[data-i="${i}"]`)?.scrollIntoView({ block: "center" });
        };
      }
      box.append(b);
    }
  }, (err) => {
    box.textContent = err.message;
  });
}

let addresses = [];
let addressesErr = ""; // why the last load failed, as whiff said it
let linked = false; // #:name has been shown

async function loadAddresses() {
  try {
    addresses = await getJSON("api/addresses");
    addressesErr = "";
  } catch (err) {
    addressesErr = err.message;
    drawAddresses();
    return;
  }
  marks = new Map();
  for (const a of addresses) for (const f of a.fns) marks.set(a.address + "/" + f.fn, f.mark);
  if (shown === "chains") redraw(); // a client's reads fold, by their marks
  drawAddresses();
  await loadServices();
  // After the statuses, so a link to an address shows its Status.
  if (!linked && location.hash.startsWith("#:")) showAddress(decodeURIComponent(location.hash.slice(2)));
  linked = true;
}

// services is each service file's line of whiff services, by name:
// {service, agent, status, note}. It is fetched again a second after
// history moves, and every 15 seconds for what changes on its own.
let services = new Map();

async function loadServices() {
  try {
    services = new Map((await getJSON("api/services")).map((s) => [s.service, s]));
  } catch {
    return; // the addresses say why
  }
  drawAddresses();
}

let servicesTimer = 0;
function servicesSoon() {
  clearTimeout(servicesTimer);
  servicesTimer = setTimeout(loadServices, 1000);
}
setInterval(loadServices, 15000);

// statusClass is the dot a service's status draws: broken, wanting you,
// working, or none for idle and ok.
function statusClass(status) {
  return { failed: "fault", faulting: "fault", "over-budget": "warn", paused: "warn", waiting: "warn", thinking: "busy", busy: "busy" }[status] || "";
}

// roughly is ms for people, in its largest unit: 40s, 12m, 3h, 2d; ago an
// ISO time as how long before now it was.
function roughly(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 172800) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}
const ago = (at) => roughly(Date.now() - Date.parse(at)) + " ago";

// serviceLines are service/status st as labelled lines, as whiff services
// name prints them: only those with something to say.
function serviceLines(st) {
  const lines = [];
  const add = (label, text) => text && lines.push([label, text]);
  add("file", st.file?.replace(/^\/(Users|home)\/[^/]+\//, "~/"));
  add("error", st["load-error"]);
  if (st.fault) add("faults", `${st.faults} today; the last ${ago(st.fault.at)}: ${st.fault.message}`);
  if (st.last) add("last", `${st.last.fn} ${ago(st.last.at)}`);
  if (st.turns !== undefined) {
    const usd = (x) => "$" + parseFloat(x).toFixed(2);
    add("turns", `${st.turns} in 24h, ${usd(st.spent)} of ${usd(st.day)} a day` + (st["last-turn"] ? `; the last ${ago(st["last-turn"])}` : ""));
  }
  if (st.mode) add("mode", [st.mode.mode, ...(st.mode.services || []).map((x) => ":" + x)].join(" ") + (st.mode.until ? ", until " + time(st.mode.until) : ""));
  add("every", st.every);
  if (st.inbox > 0) add("inbox", st.inbox + " waiting");
  if (st.timers?.length) add("timers", st.timers.map((t) => `${t.fn} in ${roughly(Date.parse(t.due) - Date.now())}`).join(", "));
  return lines;
}

// workText is a task an agent handed out, as its Status lists it: who has
// it, and where it is or how it came back, as "ready whiff/7, $0.31".
function workText(w) {
  if (w.status !== "ended") return [`:${w.to}/${w.fn}`, w.status === "held" ? "held for your review" : "waiting"];
  const r = w.result, v = r?.value;
  let how = typeof v === "string" ? v : "";
  if (r && !r.ok) how = "failed: " + (v?.message ?? JSON.stringify(v));
  else if (v && typeof v === "object") {
    how = [v.status, v.branch].filter(Boolean).join(" ");
    if (parseFloat(v.cost) > 0) how += ", $" + parseFloat(v.cost).toFixed(2);
  }
  return [`:${w.to}`, how];
}

// workList is an agent's work, newest first: one row a task.
function workList(work) {
  const ol = el("ol", "svc-work");
  for (const w of [...work].reverse()) {
    const [who, how] = workText(w);
    const bad = !w.result?.ok || ["failed", "gave-up", "cancelled"].includes(w.result?.value?.status);
    const li = el("li", w.status === "ended" ? (bad ? "failed" : "ok") : "open");
    li.append(el("span", "work-id", String(w.id)), el("span", "work-to", who), el("span", "work-how", how), el("span", "hint", ago(w.at)));
    ol.append(li);
  }
  return ol;
}

// statusBox is the Status section of an address's details: its state and
// note, then serviceLines.
// It answers at once, and fills in when the status comes, so the rest of
// the details needn't wait.
function statusBox(name) {
  const box = el("div", "svc-status", "…");
  fillStatus(box, name);
  return box;
}

async function fillStatus(box, name) {
  try {
    const st = await getJSON("api/service?address=" + encodeURIComponent(name));
    const head = el("p", "svc-head " + statusClass(st.status));
    head.append(el("span", "dot"), el("strong", "", st.status.replace("-", " ")));
    if (st.note) head.append(" ", el("span", "", st.note));
    const dl = el("dl");
    for (const [k, v] of serviceLines(st)) dl.append(el("dt", "", k), el("dd", "", v));
    box.replaceChildren(head, dl);
    if (st.work?.length) box.append(el("h4", "", "Work"), workList(st.work));
    // An agent pauses and resumes here, as you.
    if (st.agent) {
      const paused = st.status === "paused";
      const b = el("button", "svc-act", paused ? "Resume" : "Pause");
      b.type = "button";
      b.title = paused ? "take what waits in its inbox, oldest first" : "take no more turns; messages wait in its inbox";
      b.onclick = async () => {
        b.disabled = true;
        try {
          const r = await fetch("api/call", { method: "POST", body: new URLSearchParams({ action: `[:${name}/${paused ? "resume" : "pause"}]` }) });
          if (!r.ok) throw new Error((await r.text()).trim());
        } catch (err) {
          box.append(el("p", "query-error", err.message));
        }
        await loadServices();
        box.replaceWith(statusBox(name));
      };
      head.append(b);
    }
  } catch (err) {
    box.replaceChildren(el("p", "hint", err.message));
  }
}

let reloading = 0;
function reloadSoon() {
  clearTimeout(reloading);
  reloading = setTimeout(loadAddresses, 500);
}

// faulted is each address whose last call faulted, as history shows it:
// whiff restarted it, but what it last answered was its own bug.
const faulted = new Set();

// noteFault notes whether e's address faulted, and says if that changed.
function noteFault(e) {
  if (e.kind !== "call" || !e.to) return false;
  const k = kindOf(e.reply), a = to(e);
  if (k === "fault" && !faulted.has(a)) return faulted.add(a), true;
  return k === "ok" && faulted.delete(a);
}

// drawAddresses lists every address in the sidebar, with a dot: a
// service, its status when that isn't idle or ok, or a client's. A file
// that failed to load, so has no address, is there too.
function drawAddresses() {
  const box = $("addresses"), agents = $("agents");
  box.replaceChildren();
  agents.replaceChildren();
  const failed = [...services.values()].filter((s) => !addresses.some((a) => a.address === s.service)).map((s) => ({ address: s.service, fns: [] }));
  for (const a of [...addresses, ...failed]) {
    const st = services.get(a.address);
    const fault = faulted.has(a.address) && !st;
    const quiet = !st || st.status === "ok" || st.status === "idle";
    const cls = fault ? "fault" : st ? statusClass(st.status) : "";
    const b = el("button", "addr" + (a.client ? " client" : "") + (cls ? " " + cls : quiet && !fault ? " quiet" : ""));
    b.append(el("span", "dot"), el("span", "name", ":" + a.address));
    if (fault || a.client || !quiet) b.append(el("span", "note", fault ? "fault" : a.client ? "client" : st.status.replace("-", " ")));
    b.title = fault ? "a service whose last call faulted; whiff restarted it" : a.client ? "a client's address"
      : st ? (st.agent ? "an agent: " : "a service: ") + st.status.replace("-", " ") + (st.note ? ", " + st.note : "") : "a service";
    b.onclick = () => {
      activate(null);
      showAddress(a.address);
    };
    (st?.agent ? agents : box).append(b);
  }
  $("agents-group").hidden = !agents.childElementCount;
  if (addressesErr) box.append(el("p", "hint", addressesErr));
  else if (!addresses.length) box.append(el("p", "hint", "None while whiff isn't running."));
}

// showAddress shows an address's functions and state; #:name links to it.
// talksWith is who sent name messages and whom it sent them, by how many,
// in the history shown; each filters history to those messages.
function talksWith(name) {
  const ins = new Map(), outs = new Map();
  for (const e of entries) {
    if (e.kind === "note") continue;
    const a = from(e), b = to(e);
    if (b === name && a !== name) ins.set(a, (ins.get(a) || 0) + 1);
    if (a === name && b !== name) outs.set(b, (outs.get(b) || 0) + 1);
  }
  const box = el("div", "talks");
  const line = (label, counts, query) => {
    if (!counts.size) return;
    const p = el("p", "", label + " ");
    for (const [x, n] of [...counts].sort((p, q) => q[1] - p[1])) {
      const who = x === "client" ? "client" : ":" + x;
      const b = el("button", "", `${who} ${n}`);
      b.type = "button";
      b.title = "show these messages: " + query(x);
      b.onclick = () => setQuery(query(x));
      p.append(b, " ");
    }
    box.append(p);
  };
  line("From", ins, (x) => `from:${x} to:${name}`);
  line("To", [...outs.keys()].includes("client") ? new Map([...outs].filter(([x]) => x !== "client")) : outs, (x) => `from:${name} to:${x}`);
  if (!box.childElementCount) box.append(el("p", "hint", "No messages in the history shown."));
  return box;
}

async function showAddress(name) {
  history.replaceState(null, "", "#:" + encodeURIComponent(name));
  selected = null;
  mark(-1);
  const d = $("detail");
  const a = addresses.find((x) => x.address === name);
  d.replaceChildren(el("h2", "", ":" + name));
  const agent = name.startsWith("agent/");
  if (!a) {
    if (services.has(name)) d.append(section("Status", statusBox(name)));
    else d.append(el("p", "hint", "Not loaded now."));
    if (agent) showSession(d, name);
    return;
  }
  const meta = el("div", "meta");
  meta.append(el("span", "badge", a.client ? "client" : services.get(name)?.agent ? "agent" : "service"));
  if (a.state) meta.append(el("span", "badge", "state"));
  d.append(meta);
  if (services.has(name)) d.append(section("Status", statusBox(name)));
  // Its state, fetched again after you call one of its functions.
  const stateBox = el("div", "", "…");
  const loadState = async () => {
    try {
      const t = await getJSON("api/state?address=" + encodeURIComponent(name));
      stateBox.replaceChildren(drawn(t.t === "tag" && t.n === "ok" ? t.v[0] : t));
    } catch (err) {
      stateBox.replaceChildren(el("p", "hint", err.message));
    }
  };
  const table = el("table", "fns");
  for (const f of a.fns) {
    const tr = el("tr");
    const sig = (f.form === "ask" ? "ask " : "") + "(" + [f.fn, ...f.params].join(" ") + ")";
    const mk = el("span", "badge " + f.mark, ":" + f.mark);
    const c1 = el("td"), c2 = el("td"), c3 = el("td", "doc", f.doc);
    // Two lines of a doc, and all of it on a click.
    if (f.doc.length > 90) {
      c3.classList.add("more");
      c3.title = "click for the whole doc";
      c3.onclick = () => c3.classList.toggle("whole");
    }
    // A :read function of no arguments links to its answer's page, a tab;
    // any other opens a form under it that calls it as you.
    if (f.mark === "read" && !f.params.length && f.form !== "ask") {
      const link = el("a", "", sig);
      link.href = pageOf(name + "/" + f.fn, { t: "vec", v: [] });
      link.title = "open what :" + name + "/" + f.fn + " answers as a tab";
      c1.append(link);
    } else {
      const b = el("button", "fn-call", sig);
      b.type = "button";
      b.title = "call :" + name + "/" + f.fn + " as you";
      b.setAttribute("aria-expanded", "false");
      b.onclick = () => {
        const open = tr.nextElementSibling?.classList.contains("call-row");
        b.setAttribute("aria-expanded", String(!open));
        if (open) return tr.nextElementSibling.remove();
        const row = el("tr", "call-row"), td = el("td");
        td.colSpan = 3;
        const box = callBox("[:" + name + "/" + f.fn + f.params.map(() => ' "…"').join("") + "]", a.state ? loadState : null);
        td.append(box);
        row.append(td);
        tr.after(row);
        // The first "…" selected, so typing replaces it.
        const input = box.querySelector("input"), at = input.value.indexOf("…");
        input.focus();
        if (at >= 0) input.setSelectionRange(at, at + 1);
      };
      c1.append(b);
    }
    c2.append(mk);
    tr.append(c1, c2, c3);
    table.append(tr);
  }
  d.append(section("Functions", a.fns.length ? table : el("p", "hint", "None.")));
  d.append(section("Talks with", talksWith(name)));
  if (agent) showSession(d, name);
  if (a.state) {
    d.append(section("State", stateBox));
    await loadState();
  }
}

function setStatus(s) {
  const st = $("status");
  st.textContent = s.connected ? "following whiff" : s.why;
  st.classList.toggle("on", s.connected);
  if (s.connected) loadAddresses();
}

$("rows").addEventListener("click", (ev) => {
  const h = ev.target.closest(".fold[data-id]");
  if (h) {
    const id = Number(h.dataset.id);
    if (!folded.delete(id)) folded.add(id);
    redraw();
    return;
  }
  const rh = ev.target.closest(".reads-head");
  if (rh) {
    const id = Number(rh.dataset.id);
    if (!openReads.delete(id)) openReads.add(id);
    redraw();
    return;
  }
  const r = ev.target.closest(".row");
  if (r) showEntry(Number(r.dataset.i));
});

// The filter: a query, as whiff history takes it, that sniff matches
// history against, so only what it matches comes; kept in the URL, ?q=,
// so a filter is a link. Its chips add or take out a term of it.
let query = new URLSearchParams(location.search).get("q") || "";
let events = null;

// follow opens history's events for the query, again each time it changes.
function follow() {
  if (events) events.close();
  events = new EventSource("events" + (query ? "?q=" + encodeURIComponent(query) : ""));
  events.addEventListener("snapshot", (ev) => {
    const snap = JSON.parse(ev.data);
    entries = [];
    lanes = ["client"];
    selected = null;
    bulk = true;
    // Faults are known from all of history; a filter shows only some.
    if (!query) faulted.clear();
    for (const e of snap.entries) {
      entries.push(e);
      if (e.kind !== "note") { laneOf(from(e)); laneOf(to(e)); }
    }
    bulk = false;
    entries.sort((a, b) => a.us - b.us);
    entries.forEach(noteFault);
    drawAddresses();
    drawLanes();
    redraw();
    $("query-error").textContent = snap.error || "";
    $("query").classList.toggle("bad", !!snap.error);
    $("empty").textContent = snap.error ? "Fix the filter to see messages."
      : query ? "Nothing matches this filter yet."
      : "No messages yet. Call a service and it shows here.";
    setStatus(snap.status);
    const trace = $("trace");
    trace.scrollTop = trace.scrollHeight;
  });
  events.addEventListener("entry", (ev) => append(JSON.parse(ev.data)));
  events.addEventListener("status", (ev) => setStatus(JSON.parse(ev.data)));
  events.onerror = () => setStatus({ connected: false, why: "sniff stopped" });
}

// setQuery filters history by q, if it changed.
function setQuery(q) {
  q = q.trim().split(/\s+/).filter(Boolean).join(" ");
  const box = $("query");
  if (box.value.trim() !== q) box.value = q;
  drawChips(q);
  if (q === query) return;
  query = q;
  history.replaceState(null, "", location.pathname + (q ? "?q=" + encodeURIComponent(q) : "") + location.hash);
  follow();
}

function drawChips(q) {
  const terms = q.split(" ");
  for (const c of document.querySelectorAll(".filters .chip[data-term]")) {
    c.setAttribute("aria-pressed", String(terms.includes(c.dataset.term)));
  }
}

for (const c of document.querySelectorAll(".filters .chip[data-term]")) {
  c.onclick = () => {
    const terms = query.split(" ").filter(Boolean);
    const i = terms.indexOf(c.dataset.term);
    if (i >= 0) terms.splice(i, 1);
    else terms.push(c.dataset.term);
    setQuery(terms.join(" "));
  };
}
let typed = 0;
$("query").addEventListener("input", (ev) => {
  clearTimeout(typed);
  typed = setTimeout(() => setQuery(ev.target.value), 500);
});
$("filters").addEventListener("submit", (ev) => {
  ev.preventDefault();
  clearTimeout(typed);
  setQuery($("query").value);
});
$("query").addEventListener("keydown", (ev) => {
  if (ev.key === "Escape") ev.target.blur();
});
// / goes to the filter, from anywhere but a box you type in.
document.addEventListener("keydown", (ev) => {
  if (ev.key !== "/" || active || ev.metaKey || ev.ctrlKey || ev.altKey) return;
  if (ev.target.closest?.("input, textarea, select, [contenteditable]")) return;
  ev.preventDefault();
  $("query").focus();
  $("query").select();
});
$("query").value = query;
drawChips(query);
follow();

// waiting is how many wait for you in each queue the sidebar links to: the
// review queue and your inbox. The browser tab's title shows them all.
const waiting = {};

// followQueue keeps how many items target's view has beside the sidebar's
// link of class name, and in the browser tab's title, fetching it, and
// calling then if given, each time one of topics fires. Following :review/pending so, while sniff is
// open, it reviews: agents' writes wait for you rather than failing with
// :no-reviewer. A watch that sniff refuses, while whiff isn't running, is
// tried again.
function followQueue(name, target, topics, then) {
  const link = document.querySelector(".side ." + name);
  const label = link.textContent;
  const count = async () => {
    let n = 0;
    try {
      // (ok (items [...] opts)): how many items.
      const r = await getJSON("api/view?target=" + target);
      const v = r.t === "tag" && r.n === "ok" ? r.v[0] : null;
      n = v?.t === "tag" && v.v[0]?.t === "vec" ? v.v[0].v.length : 0;
    } catch {
      return;
    }
    link.replaceChildren(el("span", "name", label));
    if (n) link.append(el("span", "count", String(n)));
    link.classList.toggle("waiting", n > 0);
    waiting[name] = n;
    const all = Object.values(waiting).reduce((a, b) => a + b, 0);
    document.title = (all ? `(${all}) ` : "") + "sniff";
  };
  const changed = () => {
    count();
    if (then) then();
  };
  const w = new EventSource("api/watch?" + topics.map((t) => "topic=" + t).join("&"));
  w.addEventListener("open", changed);
  w.addEventListener("changed", changed);
  w.onerror = () => {
    if (w.readyState === EventSource.CLOSED) setTimeout(() => followQueue(name, target, topics, then), 2000);
  };
}
followQueue("review", "review/pending", ["review/pending", "review/done"], () => loadBanner());
followQueue("inbox", "me/inbox", ["me/changed"]);

// Tabs: Activity, then a view per tab, each its /view page's href, drawn
// in this page as that page draws it. Only the tab shown follows its
// topics, so a browser's few connections to sniff go further; one shown
// again fetches its view then. Open tabs and pinned views are kept in this
// browser, as conveniences: losing them loses nothing else.
const store = {
  get(k, none) {
    try { return JSON.parse(localStorage.getItem("sniff." + k)) ?? none; } catch { return none; }
  },
  set(k, v) {
    try { localStorage.setItem("sniff." + k, JSON.stringify(v)); } catch {}
  },
};

// Activity draws history as you last chose, chains or lanes, kept too.
for (const b of document.querySelectorAll("[data-show]")) b.onclick = () => showAs(b.dataset.show);
showAs(store.get("activity", "chains") === "lanes" ? "lanes" : "chains");

let tabs = []; // the view tabs, in order
let active = null; // the view tab shown, or null for Activity
let pins = store.get("pins", []);

// hrefOf is a view page's href without what only one request asks for:
// what a tab and a pin are known by.
function hrefOf(href) {
  const u = new URL(href, location.origin);
  u.searchParams.delete("said");
  u.searchParams.delete("part");
  return u.pathname + u.search;
}

// titleOf is how a view's href reads: plan/board, pipeline/show 2.
function titleOf(href) {
  const u = new URL(href, location.origin);
  const args = (u.searchParams.get("args") || "").slice(1, -1).trim();
  return decodeURIComponent(u.pathname.slice("/view/".length)) + (args ? " " + args : "");
}

// part is href's URL for part=panel: the view with its topics.
function part(href) {
  return href + (href.includes("?") ? "&" : "?") + "part=panel";
}

function openTab(href, show = true) {
  href = hrefOf(href);
  let t = tabs.find((t) => t.href === href);
  if (!t) {
    t = makeTab(href);
    tabs.push(t);
    store.set("tabs", tabs.map((t) => t.href));
  }
  if (show) activate(t);
  return t;
}

function makeTab(href) {
  const title = titleOf(href);
  const t = { href, topics: [], watch: null, stale: false };
  t.el = el("div", "tab");
  t.button = el("button", "", title);
  t.button.setAttribute("role", "tab");
  t.button.title = ":" + title;
  t.button.onclick = () => activate(t);
  const close = el("button", "close", "×");
  close.setAttribute("aria-label", "Close " + title);
  close.onclick = () => closeTab(t);
  t.el.append(t.button, close);
  t.el.addEventListener("auxclick", (ev) => { if (ev.button === 1) closeTab(t); });
  $("tabs").append(t.el);

  t.panel = el("div", "panel");
  t.panel.setAttribute("role", "tabpanel");
  t.panel.hidden = true;
  const head = el("div", "panel-head");
  t.status = el("span", "status", "…");
  t.pin = el("button", "toggle");
  t.pin.onclick = () => togglePin(href);
  const page = el("a", "", "Open as a page");
  page.href = href;
  page.target = "_blank";
  page.rel = "noopener";
  page.dataset.page = "";
  page.title = "a page of its own, to keep open or share";
  head.append(el("span", "target", ":" + title), t.status, el("span", "grow"), t.pin, page);
  t.said = el("div");
  t.box = el("div", "panel-body");
  t.panel.append(head, t.said, t.box);
  t.box.addEventListener("submit", (ev) => send(t, ev));
  // A change that came while you typed shows once you're done.
  t.box.addEventListener("focusout", () => setTimeout(() => { if (t.stale) load(t); }, 0));
  document.querySelector(".work").append(t.panel);
  drawPin(t);
  return t;
}

function closeTab(t) {
  stopWatch(t);
  const i = tabs.indexOf(t);
  tabs.splice(i, 1);
  t.el.remove();
  t.panel.remove();
  store.set("tabs", tabs.map((t) => t.href));
  if (active === t) activate(tabs[i] || tabs[i - 1] || null);
}

// activate shows t, or Activity for null.
function activate(t) {
  if (active && active !== t) stopWatch(active);
  active = t;
  $("activity").hidden = !!t;
  $("tab-activity").setAttribute("aria-selected", String(!t));
  for (const x of tabs) {
    x.panel.hidden = x !== t;
    x.button.setAttribute("aria-selected", String(x === t));
  }
  store.set("active", t ? t.href : "");
  markSide();
  showBanner();
  $("filters").hidden = !!t;
  if (!t) return;
  if (t.topics.length) watch(t); // it loads when the watch opens
  else load(t);
}
$("tab-activity").onclick = () => activate(null);

// load fetches t's view again, unless you are typing in it.
async function load(t) {
  if (t.box.contains(document.activeElement) && document.activeElement.tagName === "INPUT") {
    t.stale = true;
    return;
  }
  t.stale = false;
  try {
    const r = await fetch(part(t.href));
    const html = await r.text();
    if (!r.ok) throw new Error(html.trim());
    fill(t, html);
  } catch (err) {
    setTabStatus(t, err.message, false);
    // Not drawn, as whiff isn't running: try again while it shows.
    if (!t.watch) setTimeout(() => { if (active === t && !t.watch) load(t); }, 2000);
  }
}

// fill shows html, a panel sniff drew, in t, and follows the topics it
// names while t shows.
function fill(t, html) {
  t.box.innerHTML = html; // sniff drew it, escaping what it shows
  const said = t.box.querySelector(":scope > .said");
  if (said) t.said.replaceChildren(said); // it stays as the view changes
  const topics = (t.box.querySelector(".shown")?.dataset.topics || "").split(" ").filter(Boolean);
  setTabStatus(t, "fetched again on " + topics.map((x) => ":" + x).join(", "), true);
  if (active === t && topics.join(" ") !== t.topics.join(" ")) {
    t.topics = topics;
    watch(t);
  }
}

function setTabStatus(t, text, on) {
  t.status.textContent = text;
  t.status.classList.toggle("on", on);
}

// watch follows t's topics, fetching its view each time one fires and
// when the watch opens, so what changed meanwhile shows too. One that
// sniff refuses, while whiff isn't running, is tried again.
function watch(t) {
  stopWatch(t);
  if (!t.topics.length) return;
  const w = new EventSource("api/watch?" + t.topics.map((x) => "topic=" + encodeURIComponent(x)).join("&"));
  t.watch = w;
  w.addEventListener("open", () => load(t));
  w.addEventListener("changed", () => load(t));
  w.onerror = () => {
    setTabStatus(t, "not following " + t.topics.map((x) => ":" + x).join(", ") + ": is whiff running?", false);
    if (w.readyState === EventSource.CLOSED) setTimeout(() => { if (t.watch === w) watch(t); }, 2000);
  };
}

function stopWatch(t) {
  if (t.watch) t.watch.close();
  t.watch = null;
}

// send sends what a form in t's view picked, as its page's form would, and
// shows the view drawn again with what it answered.
async function send(t, ev) {
  ev.preventDefault();
  try {
    fill(t, await act(ev.target, part(t.href)));
  } catch (err) {
    setTabStatus(t, err.message, false);
  }
}

// act sends what form picked, as its page's form would, and answers the
// part of a page it goes back to, back, which shows what it answered.
async function act(form, back) {
  const body = new URLSearchParams(new FormData(form));
  body.set("back", back);
  const r = await fetch("/act", { method: "POST", body });
  const html = await r.text();
  if (!r.ok) throw new Error(html.trim());
  return html;
}

// The review banner: over Activity, the oldest write that waits for your
// review, with its actions and a link to the rest, fetched each time the
// queue changes. Being where you look is what keeps an open sniff a
// reviewer.
const banner = $("banner");
const bannerBody = $("banner-body");
const queue = "/view/review/pending?part=first";
let bannerStale = false; // a change came while you typed why

async function loadBanner() {
  if (bannerBody.contains(document.activeElement) && document.activeElement.tagName === "INPUT") {
    bannerStale = true;
    return;
  }
  bannerStale = false;
  try {
    const r = await fetch(queue);
    if (!r.ok) throw new Error(await r.text());
    fillBanner(await r.text());
  } catch {
    fillBanner(""); // the sidebar says whiff isn't running
  }
}

function fillBanner(html) {
  bannerBody.innerHTML = html; // sniff drew it, escaping what it shows
  // What the last one answered shows a moment, if none waits after it.
  if (!bannerBody.querySelector(".item") && bannerBody.querySelector(".said")) setTimeout(loadBanner, 3000);
  showBanner();
}

function showBanner() {
  banner.hidden = active !== null || !bannerBody.querySelector(".item, .said");
}

bannerBody.addEventListener("focusout", () => setTimeout(() => { if (bannerStale) loadBanner(); }, 0));
bannerBody.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    fillBanner(await act(ev.target, queue));
  } catch (err) {
    bannerBody.prepend(el("p", "said err", err.message));
  }
});

// a accepts the write the banner shows, r opens Reject's box for why, and
// Esc there leaves it unsent; never while you type elsewhere.
document.addEventListener("keydown", (ev) => {
  if (banner.hidden || ev.metaKey || ev.ctrlKey || ev.altKey) return;
  const typing = ev.target.closest?.("input, textarea, select, [contenteditable]");
  if (typing) {
    if (ev.key === "Escape" && bannerBody.contains(typing)) {
      typing.closest("details")?.removeAttribute("open");
      typing.blur();
    }
    return;
  }
  const action = (fn) => bannerBody.querySelector(`input[name=action][value^="[:review/${fn} "]`);
  if (ev.key === "a" && action("accept")) {
    ev.preventDefault();
    action("accept").form.requestSubmit();
  } else if (ev.key === "r" && action("reject")) {
    ev.preventDefault();
    const asks = action("reject").closest("details");
    asks.open = true;
    asks.querySelector("input[name=answer]").focus();
  }
});

function togglePin(href) {
  pins = pins.includes(href) ? pins.filter((p) => p !== href) : [...pins, href];
  store.set("pins", pins);
  drawPins();
  for (const t of tabs) drawPin(t);
}

function drawPin(t) {
  const on = pins.includes(t.href);
  t.pin.textContent = on ? "Unpin" : "Pin";
  t.pin.title = on ? "take it off the sidebar" : "keep it in the sidebar";
}

function drawPins() {
  const box = $("pins");
  box.replaceChildren();
  for (const p of pins) {
    const a = el("a", "view-link", titleOf(p));
    a.href = p;
    box.append(a);
  }
  if (!pins.length) box.append(el("p", "hint", "Pin a view from its tab."));
  markSide();
}

// markSide marks the sidebar's link to the tab shown.
function markSide() {
  for (const a of document.querySelectorAll(".side a[href]")) {
    if (active && hrefOf(a.getAttribute("href")) === active.href) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

// A link to a view opens it as a tab; with a modifier key, or "Open as a
// page", as the browser opens links.
document.addEventListener("click", (ev) => {
  const a = ev.target.closest('a[href^="/view/"]');
  if (!a || "page" in a.dataset || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
  ev.preventDefault();
  openTab(a.getAttribute("href"));
});

// A location copies itself, file:line:col, for hiss or a terminal: a page
// can't open a file.
document.addEventListener("click", (ev) => {
  const b = ev.target.closest("[data-copy]");
  if (!b) return;
  navigator.clipboard.writeText(b.dataset.copy).then(() => {
    b.classList.add("copied");
    setTimeout(() => b.classList.remove("copied"), 1200);
  }, (err) => { b.title = err.message; });
});

// Keys over Activity: j and k go to the next and previous message or
// chain heading, Enter opens it (a heading folds or unfolds, a message
// answered with a view opens it as a tab), c opens Call again for the
// message shown, and Esc there leaves it uncalled; never while you type,
// or over a tab.
document.addEventListener("keydown", (ev) => {
  if (active || palette.open || ev.metaKey || ev.ctrlKey || ev.altKey) return;
  const typing = ev.target.closest?.("input, textarea, select, [contenteditable]");
  if (typing) {
    if (ev.key === "Escape" && $("detail").contains(typing)) {
      typing.closest("details")?.removeAttribute("open");
      typing.blur();
    }
    return;
  }
  if (ev.key === "j" || ev.key === "k") {
    ev.preventDefault();
    step(ev.key === "j" ? 1 : -1);
  } else if (ev.key === "Enter" && selected !== null && !ev.target.closest?.("button, a, summary")) {
    const view = viewOf(entries[selected]);
    if (view) {
      ev.preventDefault();
      openTab(view);
    }
  } else if (ev.key === "c" && selected !== null) {
    const again = $("detail").querySelector("details.again");
    if (!again) return;
    ev.preventDefault();
    again.open = true;
    again.querySelector("input").focus();
  }
});

// step goes d messages or headings on from where you are, the heading
// focused or the message selected; from neither, to the first or last.
function step(d) {
  const all = [...$("rows").querySelectorAll(".reads-head, .row")];
  let at = all.indexOf(document.activeElement);
  if (at < 0 && selected !== null) at = all.indexOf(document.querySelector(`.row[data-i="${selected}"]`));
  const next = all[at < 0 ? (d > 0 ? 0 : all.length - 1) : Math.max(0, Math.min(all.length - 1, at + d))];
  if (!next) return;
  if (next.classList.contains("row")) {
    if ($("rows").contains(document.activeElement)) document.activeElement.blur();
    showEntry(Number(next.dataset.i));
  } else {
    next.focus({ preventScroll: true });
  }
  next.scrollIntoView({ block: "nearest" });
}

// The palette, ⌘K: go to an address, a view or a function, or call one,
// with the filters ready for what you typed. A whole form, (todo/add "x"),
// is called as you, as Call again calls: a call, never code.
const palette = $("palette");
const paletteQ = $("palette-q");
const modK = /Mac|iPhone|iPad/.test(navigator.userAgent) ? "⌘K" : "Ctrl K";
let picks = []; // what Enter can do, in the order shown
let picked = 0; // the pick Enter does

function openPalette() {
  if (palette.open) return paletteQ.select();
  paletteQ.value = "";
  $("palette-reply").replaceChildren();
  drawPicks();
  palette.showModal();
}

// drawPicks lists what matches the palette's text: each word of it is in
// a name, names that begin with the first word first.
function drawPicks() {
  const text = paletteQ.value.trim();
  const words = text.toLowerCase().split(/\s+/).filter(Boolean).map((w) => w.replace(/^:/, ""));
  const hits = (name) => words.every((w) => name.toLowerCase().includes(w));
  const first = (xs, name) => xs.filter((x) => hits(name(x)))
    .sort((x, y) => Number(!name(x).toLowerCase().startsWith(words[0] || "")) - Number(!name(y).toLowerCase().startsWith(words[0] || "")))
    .slice(0, 8);
  const groups = [];
  if (text.startsWith("(")) {
    groups.push(["Call", [pick([el("code", "", text)], "↵ call as you", () => callForm(text), true)]]);
  } else {
    const fns = first(addresses.flatMap((a) => a.fns.map((f) => ({ a, f, name: a.address + "/" + f.fn }))), (x) => x.name);
    groups.push(["Functions", fns.map(({ f, name }) => {
      const tab = f.mark === "read" && !f.params.length && f.form !== "ask";
      return pick([el("code", "", name), el("span", "badge " + f.mark, f.mark), el("span", "doc", f.doc)],
        tab ? "opens a tab" : "↵ fill in args",
        tab ? () => go(() => openTab(pageOf(name, { t: "vec", v: [] }))) : () => fillIn(name, f.params));
    })]);
    // todo/ matches :todo too, as it begins its functions.
    const addrs = first(addresses, (a) => a.address + "/");
    groups.push(["Addresses", addrs.map((a) => {
      const fault = faulted.has(a.address);
      const what = el("span", "pick-line addr" + (a.client ? " client" : "") + (fault ? " fault" : ""));
      what.append(el("span", "dot"), el("code", "", ":" + a.address), el("span", "doc",
        [`${a.fns.length} function${a.fns.length === 1 ? "" : "s"}`, a.state && "state", a.client && "client", fault && "fault"].filter(Boolean).join(" · ")));
      return pick([what], "", () => go(() => { activate(null); showAddress(a.address); }), true);
    })]);
    const views = [...new Set(["/view/review/pending", "/view/me/inbox", ...pins, ...tabs.map((t) => t.href)])];
    groups.push(["Views", [
      ...(hits("activity") ? [pick([el("span", "", "Activity")], "", () => go(() => activate(null)), true)] : []),
      ...first(views, titleOf).map((href) => pick([el("code", "", titleOf(href))], "opens a tab", () => go(() => openTab(href)), true)),
    ]]);
    // The filters for the address or function matched first, and for
    // what you typed, if it reads as a query.
    const show = [];
    const filter = (label, q) => {
      const what = el("span", "pick-line");
      what.append(...label, el("code", "doc", q));
      show.push(pick([what], "", () => go(() => { activate(null); setQuery(q); }), true));
    };
    if (words.length && text.includes("/") && fns[0]) filter(["Messages to ", el("code", "", ":" + fns[0].name)], "to:" + fns[0].name);
    if (words.length && addrs[0]) {
      const x = addrs[0].address, at = () => el("code", "", ":" + x);
      filter(["Messages to ", at()], "to:" + x);
      filter(["Messages from ", at()], "from:" + x);
      filter(["Failed calls to ", at()], `to:${x} kind:err`);
    }
    if (words.some((w) => /^-?[a-z]+:./.test(w))) filter(["Filter history by "], text);
    groups.push(["Show", show]);
  }
  const list = $("palette-list");
  list.replaceChildren();
  picks = [];
  for (const [title, items] of groups) {
    if (!items.length) continue;
    list.append(el("h3", "", title));
    for (const p of items) {
      const n = picks.length;
      p.id = "pick-" + n;
      p.addEventListener("mousemove", () => { if (picked !== n) markPick(n); });
      p.addEventListener("click", () => { markPick(n); p.run(); });
      list.append(p);
      picks.push(p);
    }
  }
  if (!picks.length) list.append(el("p", "hint", addresses.length ? "Nothing matches." : "Nothing to go to while whiff isn't running."));
  markPick(0);
}

// pick is a row of the palette, of nodes, that run does; then says what it
// does; a wide one has no mark or doc columns.
function pick(nodes, then, run, wide) {
  const p = el("div", "pick" + (wide ? " wide" : ""));
  p.setAttribute("role", "option");
  p.append(...nodes, el("span", "then", then));
  p.run = run;
  return p;
}

function markPick(n) {
  picked = Math.max(0, Math.min(picks.length - 1, n));
  picks.forEach((p, i) => p.setAttribute("aria-selected", String(i === picked)));
  if (picks.length) {
    paletteQ.setAttribute("aria-activedescendant", picks[picked].id);
    picks[picked].scrollIntoView({ block: "nearest" });
  } else {
    paletteQ.removeAttribute("aria-activedescendant");
  }
}

// go closes the palette and does f.
function go(f) {
  palette.close();
  f();
}

// fillIn puts a call of name as a form in the palette, its first "…"
// selected, so typing replaces it.
function fillIn(name, params) {
  paletteQ.value = "(" + [name, ...params.map(() => '"…"')].join(" ") + ")";
  drawPicks();
  const at = paletteQ.value.indexOf("…");
  if (at >= 0) paletteQ.setSelectionRange(at, at + 1);
  else paletteQ.setSelectionRange(paletteQ.value.length - 1, paletteQ.value.length - 1);
}

// callForm calls text, a form, (todo/add "x"), as you, as [:todo/add "x"],
// and shows its reply in the palette.
async function callForm(text) {
  const out = $("palette-reply");
  const action = "[:" + text.slice(1).replace(/\)\s*$/, "").trim().replace(/^:/, "") + "]";
  out.replaceChildren(el("p", "hint", "…"));
  try {
    const r = await fetch("api/call", { method: "POST", body: new URLSearchParams({ action }) });
    if (!r.ok) throw new Error((await r.text()).trim());
    out.replaceChildren(drawn(await r.json(), "answer"));
  } catch (err) {
    out.replaceChildren(el("p", "query-error", err.message));
  }
}
answers($("palette-reply"));

paletteQ.addEventListener("input", () => {
  $("palette-reply").replaceChildren();
  drawPicks();
});
paletteQ.addEventListener("keydown", (ev) => {
  if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
    ev.preventDefault();
    markPick(picked + (ev.key === "ArrowDown" ? 1 : -1));
  } else if (ev.key === "Enter" && !ev.isComposing) {
    ev.preventDefault();
    picks[picked]?.run();
  }
});
// A click outside it closes it.
palette.addEventListener("click", (ev) => { if (ev.target === palette) palette.close(); });
$("goto").onclick = openPalette;
document.querySelector("#goto .mod-k").textContent = modK;
// ⌘K, or Ctrl K, opens it from anywhere, and closes it again.
document.addEventListener("keydown", (ev) => {
  if (!(ev.metaKey || ev.ctrlKey) || ev.altKey || ev.shiftKey || ev.key?.toLowerCase() !== "k") return;
  ev.preventDefault();
  if (palette.open) palette.close();
  else openPalette();
});

$("detail").append(keysLine());
drawPins();
for (const href of store.get("tabs", [])) openTab(href, false);
activate(tabs.find((t) => t.href === store.get("active", "")) || null);
