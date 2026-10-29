// sniff's page: history from sniff's event stream, drawn as a lane per
// address and an arrow per message; details from its JSON endpoints, with
// values and views drawn by sniff (render).
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
  const box = el("div", as === "reply" ? "" : "value", "…");
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

function time(at) {
  const d = new Date(at);
  return d.toTimeString().slice(0, 8) + "." + String(d.getMilliseconds()).padStart(3, "0");
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

function redraw() {
  const rows = $("rows");
  const frag = document.createDocumentFragment();
  entries.forEach((e, i) => frag.append(row(e, i)));
  rows.replaceChildren(frag);
  $("empty").hidden = entries.length > 0;
  if (selected !== null) mark(selected);
}

// append adds e in order of when it began: history records a call when it
// ends, after the calls it made.
function append(e) {
  const trace = $("trace");
  const atEnd = trace.scrollHeight - trace.scrollTop - trace.clientHeight < 40;
  let i = entries.length;
  while (i > 0 && entries[i - 1].us > e.us) i--;
  entries.splice(i, 0, e);
  if (selected !== null && selected >= i) selected++;
  const n = lanes.length;
  const r = row(e, i);
  if (lanes.length === n) { // else redraw drew it
    // Only the rows after it move: renumber them rather than draw them all.
    const rows = $("rows");
    for (let j = i; j < rows.children.length; j++) rows.children[j].dataset.i = j + 1;
    rows.insertBefore(r, rows.children[i] || null);
  }
  $("empty").hidden = true;
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
  history.replaceState(null, "", location.pathname);
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
  if (e.args) d.append(section("Arguments", drawn(e.args)));
  if (!e.reply) {
    d.append(section("Reply", el("p", "hint", "None: nobody handled it.")));
    return;
  }
  d.append(section("Reply", drawn(e.reply, "reply")));
  // A view a :read function answers can be a page of its own, where its
  // actions work.
  const view = e.reply.t === "tag" && e.reply.n === "ok" && e.reply.v[0]?.t === "tag";
  if (view && e.kind === "call" && e.args && marks.get(e.to) === "read") {
    const a = el("a", "pin", "Open :" + e.to + " as a page");
    a.href = pageOf(e.to, e.args);
    d.append(a);
  }
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

async function loadAddresses() {
  try {
    addresses = await getJSON("api/addresses");
  } catch {
    return;
  }
  marks = new Map();
  for (const a of addresses) for (const f of a.fns) marks.set(a.address + "/" + f.fn, f.mark);
  if (location.hash.startsWith("#:")) showAddress(decodeURIComponent(location.hash.slice(2)));
  const nav = $("addresses");
  nav.replaceChildren();
  for (const a of addresses) {
    const b = el("button", a.client ? "client" : "", ":" + a.address);
    b.title = a.client ? "a client's address" : "a service";
    b.onclick = () => showAddress(a.address);
    nav.append(b);
  }
}

// showAddress shows an address's functions and state; #:name links to it.
async function showAddress(name) {
  history.replaceState(null, "", "#:" + encodeURIComponent(name));
  selected = null;
  mark(-1);
  const d = $("detail");
  const a = addresses.find((x) => x.address === name);
  d.replaceChildren(el("h2", "", ":" + name));
  const agent = name.startsWith("agent/");
  if (!a) {
    d.append(el("p", "hint", "Not loaded now."));
    if (agent) showSession(d, name);
    return;
  }
  const meta = el("div", "meta");
  meta.append(el("span", "badge", a.client ? "client" : "service"));
  if (a.state) meta.append(el("span", "badge", "state"));
  d.append(meta);
  const table = el("table", "fns");
  for (const f of a.fns) {
    const tr = el("tr");
    const sig = (f.form === "ask" ? "ask " : "") + "(" + [f.fn, ...f.params].join(" ") + ")";
    const mk = el("span", "badge " + f.mark, ":" + f.mark);
    const c1 = el("td"), c2 = el("td"), c3 = el("td", "doc", f.doc);
    // A :read function of no arguments links to its answer's page.
    if (f.mark === "read" && !f.params.length && f.form !== "ask") {
      const link = el("a", "", sig);
      link.href = pageOf(name + "/" + f.fn, { t: "vec", v: [] });
      link.title = "open what :" + name + "/" + f.fn + " answers as a page";
      c1.append(link);
    } else {
      c1.textContent = sig;
    }
    c2.append(mk);
    tr.append(c1, c2, c3);
    table.append(tr);
  }
  d.append(section("Functions", a.fns.length ? table : el("p", "hint", "None.")));
  if (agent) showSession(d, name);
  if (a.state) {
    try {
      const t = await getJSON("api/state?address=" + encodeURIComponent(name));
      d.append(section("State", drawn(t.t === "tag" && t.n === "ok" ? t.v[0] : t)));
    } catch (err) {
      d.append(section("State", el("p", "hint", err.message)));
    }
  }
}

function setStatus(s) {
  const st = $("status");
  st.textContent = s.connected ? "following whiff" : s.why;
  st.classList.toggle("on", s.connected);
  if (s.connected) loadAddresses();
}

$("rows").addEventListener("click", (ev) => {
  const r = ev.target.closest(".row");
  if (r) showEntry(Number(r.dataset.i));
});

const events = new EventSource("events");
events.addEventListener("snapshot", (ev) => {
  const snap = JSON.parse(ev.data);
  entries = [];
  lanes = ["client"];
  selected = null;
  bulk = true;
  for (const e of snap.entries) {
    entries.push(e);
    if (e.kind !== "note") { laneOf(from(e)); laneOf(to(e)); }
  }
  bulk = false;
  entries.sort((a, b) => a.us - b.us);
  drawLanes();
  redraw();
  setStatus(snap.status);
  const trace = $("trace");
  trace.scrollTop = trace.scrollHeight;
});
events.addEventListener("entry", (ev) => append(JSON.parse(ev.data)));
events.addEventListener("status", (ev) => setStatus(JSON.parse(ev.data)));
events.onerror = () => setStatus({ connected: false, why: "sniff stopped" });

// waiting is how many wait for you in each queue the header links to: the
// review queue and your inbox. The tab's title shows them all.
const waiting = {};

// followQueue keeps how many items target's view has beside the header's
// link of class name, and in the tab's title, fetching it each time one
// of topics fires. Following :review/pending so, while sniff is open, it
// reviews: agents' writes wait for you rather than failing with
// :no-reviewer. A watch that sniff refuses, while whiff isn't running, is
// tried again.
function followQueue(name, target, topics) {
  const link = document.querySelector("header ." + name);
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
    link.replaceChildren(name);
    if (n) link.append(" ", el("span", "count", String(n)));
    link.classList.toggle("waiting", n > 0);
    waiting[name] = n;
    const all = Object.values(waiting).reduce((a, b) => a + b, 0);
    document.title = (all ? `(${all}) ` : "") + "sniff";
  };
  const w = new EventSource("api/watch?" + topics.map((t) => "topic=" + t).join("&"));
  w.addEventListener("open", count);
  w.addEventListener("changed", count);
  w.onerror = () => {
    if (w.readyState === EventSource.CLOSED) setTimeout(() => followQueue(name, target, topics), 2000);
  };
}
followQueue("review", "review/pending", ["review/pending", "review/done"]);
followQueue("inbox", "me/inbox", ["me/changed"]);
