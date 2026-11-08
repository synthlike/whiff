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

// What sniff keeps in this browser, as conveniences: losing it loses
// nothing else.
const store = {
  get(k, none) {
    try { return JSON.parse(localStorage.getItem("sniff." + k)) ?? none; } catch { return none; }
  },
  set(k, v) {
    try { localStorage.setItem("sniff." + k, JSON.stringify(v)); } catch {}
  },
};

// marks is each function's mark, "read" or "write", by its target,
// "plan/board", from sniff's addresses; params its parameters' names.
let marks = new Map();
let params = new Map();
let asks = new Set(); // the targets of ask functions, answered later

// isRead says whether target only reads: a :read function that answers at
// once. An ask is :read too, but what it asks for is work.
const isRead = (target) => marks.get(target) === "read" && !asks.has(target);

// pageOf is the page that shows what target answers to args, a tree of a
// vector, as a view: /view/pipeline/show?args=[2].
function pageOf(target, args) {
  return "/view/" + encodeURI(target) + (args.v.length ? "?args=" + encodeURIComponent(print(args)) : "");
}

let entries = []; // history, oldest first
let selected = null; // the index of the entry the detail pane shows
let only = null; // the id of the one chain shown, or null for every one
// The project shown, or "" for every one: in the URL, ?p=, so it's a link.
let project = new URLSearchParams(location.search).get("p") || "";

// projectIn is the project of chain's work: the first its messages name,
// as the daemon tags a chain once a job in it runs in a repository, so
// what came before, the request itself, has none yet.
// hueOf is the class a project, or a view item's tag, is drawn in, hue-0
// to hue-7, as sniff's render.go gives it: FNV-1a, so each keeps its
// colour.
function hueOf(tag) {
  let h = 2166136261;
  for (const b of new TextEncoder().encode(tag)) h = Math.imul(h ^ b, 16777619) >>> 0;
  return "hue-" + (h % 8);
}

function projectIn(chain) {
  for (const [i] of chain) if (entries[i].project) return entries[i].project;
  return "";
}
const opened = new Set(); // the keys of the cards opened
const cardView = new Map(); // how each open card shows its chain, by key: "steps" (the default), "lanes" or "all"
let layout = store.get("layout", "cards") === "map" ? "map" : "cards"; // Activity as cards, or as a map of who talks to whom
const openBg = new Set(); // the keys of the runs of background events opened
let showBg = store.get("background", false) === true; // every background event shown

// counts is what the status strip says: what waits in each queue, and
// how many requests are running.
const counts = { review: 0, inbox: 0, failed: 0, running: 0 };

// Addresses of an entry: where it comes from and where it goes. A client has
// no address; to is address/fn, or address on :topic.
function from(e) { return e.from || "client"; }
function to(e) {
  let t = e.to;
  if (!t) return "client";
  // whiff's notices to a service, whiff/done, whiff/result, whiff/asked,
  // go to the service.
  const w = t.indexOf("/whiff/");
  if (w >= 0) return t.slice(0, w);
  const on = t.indexOf(" on :");
  if (on >= 0) return t.slice(0, on);
  return t.slice(0, t.lastIndexOf("/"));
}
function fn(e) {
  if (e.kind === "pub") return ":" + e.to.slice(e.to.indexOf(" on :") + 5);
  if (e.kind === "done") return "done";
  const w = e.to.indexOf("/whiff/");
  return w >= 0 ? e.to.slice(w + 1) : e.to.slice(e.to.lastIndexOf("/") + 1);
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

// errText is what an err or fault says: its :message, or itself printed.
function errText(r) {
  const v = r?.v?.[0];
  if (v?.t === "str") return v.v;
  return field(v, "message")?.v ?? print(r);
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

const home = (s) => s.replace(/\/(Users|home)\/[^/\s"]+\//g, "~/");
const plural = (n, what, many = what + "s") => `${n} ${n === 1 ? what : many}`;

// isDone says whether e answers a pending, [id result]: a done, or the
// whiff/result an agent hears of what its turn started.
const isDone = (e) => (e.kind === "done" || !!e.to?.endsWith("/whiff/result")) && e.args?.v?.length === 2;

// chains is history as causal chains, oldest first: each the entry that
// began it, whose cause isn't here, gone from history or left out by the
// filter, then what it led to, depth first, as [index, depth]. What an
// agent's session sends has no cause: it goes under the last message to
// the agent before it, the one its turn is for.
function chains() {
  const at = new Map();
  entries.forEach((e, i) => { if (e.id) at.set(e.id, i); });
  const led = new Map(), roots = [], lastTo = new Map();
  entries.forEach((e, i) => {
    let p = e.cause ? at.get(e.cause) : undefined;
    if (p === undefined && e.from?.startsWith("agent/")) p = lastTo.get(e.from.slice("agent/".length));
    if ((e.kind === "call" || e.kind === "send") && e.to && !e.to.endsWith("/whiff/asked") && !isRead(e.to)) lastTo.set(to(e), i);
    if (p === undefined || p >= i) return roots.push(i);
    if (!led.has(p)) led.set(p, []);
    led.get(p).push(i);
  });
  return roots.map((r) => {
    const chain = [];
    const walk = (i, depth) => {
      chain.push([i, depth]);
      for (const j of led.get(i) || []) walk(j, depth + 1);
    };
    walk(r, 0);
    return chain;
  });
}

// keyOf is what a chain is known by across redraws: its first entry's id.
const keyOf = (e) => String(e.id || "@" + e.us);

const isAgent = (a) => !!a && (a.startsWith("agent/") || !!services.get(a)?.agent);

// isBackground says whether a chain is bookkeeping rather than a request:
// a service's own ticks and settles, an agent waking itself, your page's
// reads, and whiff's notes. A chain with a fault in it is never one.
function isBackground(chain) {
  const e = entries[chain[0][0]];
  if (chain.some(([i]) => kindOf(entries[i].reply) === "fault")) return false;
  if (e.kind === "note") return true;
  // whiff's notice or an answer whose start is gone from history.
  if (isDone(e) || e.to?.endsWith("/whiff/asked")) return true;
  if (e.from) return !isAgent(e.from) || (chain.length === 1 && to(e) === e.from);
  if (e.origin === "agent") return false;
  return e.kind === "call" && isRead(e.to);
}

// cardOf is a request's chain as its card says it: who asked whom what,
// how it stands, state "ok", "running", "waiting", "failed" or "quiet",
// with label, and what it cost.
function cardOf(chain) {
  const is = chain.map(([i]) => i).sort((a, b) => a - b);
  const root = entries[is[0]];
  const dones = new Map(); // a pending's id → the done that answered it
  for (const i of is) {
    const e = entries[i];
    if (isDone(e)) dones.set(String(e.args.v[0].v), e);
  }
  const c = { chain, is, root, dones, key: keyOf(root), project: projectIn(chain), state: "ok", label: "Done", run: null, rounds: 0, step: "", job: "", cost: 0, said: null };
  let jobCost = 0, resultCost = 0, status = null, branch = "", note = "", fault = null, open = null;
  for (const i of is) {
    const e = entries[i], k = kindOf(e.reply);
    if (k === "fault") fault ??= errText(e.reply);
    if (k === "pending" && !dones.has(String(pendingOf(e.reply)))) {
      open = e;
      if (e.to === "jobs/run") c.job = jobName(e.args.v[0]);
    }
    if (e.kind === "pub") {
      const v = e.args?.v?.[0], s = field(v, "step"), n = field(v, "run");
      if (n) c.run = n.v;
      if (s?.t === "kw") {
        c.step = s.v;
        if (s.v === "review") c.rounds++;
      }
    }
    if (isDone(e)) {
      const v = e.args.v[1]?.v?.[0];
      const st = field(v, "status");
      if (st?.t === "kw") {
        status = st.v;
        branch = field(v, "branch")?.v || "";
        note = field(v, "note")?.v || "";
      }
      const n = field(v, "run");
      if (n) c.run = n.v;
      resultCost += parseFloat(field(v, "cost")?.v) || 0;
      const out = v?.t === "str" ? v.v : field(v, "stdout")?.v || "";
      jobCost += parseFloat(/"total_cost_usd":\s*([0-9.]+)/.exec(out)?.[1]) || 0;
    }
    if (to(e) === "me" && !isDone(e) && e.kind !== "pub" && e.args?.v?.[0]?.t === "str") c.said = e;
  }
  // A pipeline's :cost is its jobs': the larger is never counted twice.
  c.cost = Math.max(jobCost, resultCost);
  const set = (state, label) => { c.state = state; c.label = label; };
  const rk = kindOf(root.reply);
  const end = rk === "pending" ? dones.get(String(pendingOf(root.reply))) : null;
  if (fault) set("failed", "Faulted: " + fault);
  else if (open) {
    if (open.gate?.startsWith("held")) set("waiting", "Held for your review");
    else if (to(open) === "me") set("waiting", "Waits for your answer");
    else set("running", "Running" + (c.step ? " · " + c.step : ""));
  } else if (rk === "err") set("failed", "Failed: " + errText(root.reply));
  else if (end && kindOf(end.args.v[1]) !== "ok") set("failed", "Failed: " + errText(end.args.v[1]));
  else if (status) {
    const word = status[0].toUpperCase() + status.slice(1).replace("-", " ");
    if (status === "ready") set("ok", "Ready" + (branch ? " on " + branch : ""));
    else if (status === "failed" || status === "gave-up") set("failed", word + (note ? ": " + note : ""));
    else if (status === "cancelled" || status === "discarded") set("quiet", word);
    else set("ok", word + (branch ? " " + branch : ""));
  }
  return c;
}

// askText is what a message asks, as words: its strings as they are, the
// rest printed; a job as its command.
function askText(e) {
  if (e.kind === "note") return e.note;
  if (e.kind === "eval") return e.args?.v[0]?.v ?? "";
  if (e.to === "jobs/run" && e.args?.v?.length) return jobName(e.args.v[0]);
  let vs = e.args?.v || [];
  if (e.kind === "done" && vs.length === 2) vs = [vs[1]];
  return home(vs.map((v) => (v.t === "str" ? v.v : print(v))).join(" "));
}

// whoOf is who sent e: you, an agent's session by the agent's name, or
// the address.
const whoOf = (e) => {
  if (!e.from) return e.origin === "agent" ? "an agent" : "you";
  const name = e.from.replace(/^agent\//, "");
  return name !== e.from && services.get(name)?.agent ? name : e.from;
};

// resultText is a result in a few words: a run's status and branch, a
// text's first line, or it printed.
function resultText(r) {
  const v = r?.v?.[0];
  const st = field(v, "status");
  if (st?.t === "kw") {
    const cost = parseFloat(field(v, "cost")?.v);
    return [st.v, field(v, "branch")?.v].filter(Boolean).join(" ") + (cost > 0 ? " · $" + cost.toFixed(2) : "");
  }
  if (kindOf(r) !== "ok") return errText(r);
  // A job's: how it exited, or what a claude's turn cost.
  const exit = field(v, "exit");
  const out = v?.t === "str" ? v.v : field(v, "stdout")?.v || "";
  const usd = /"total_cost_usd":\s*([0-9.]+)/.exec(out)?.[1];
  if (usd) return "turn ended · $" + parseFloat(usd).toFixed(2);
  if (exit) return exit.v === "0" ? "exit 0" : "exit " + exit.v;
  if (v?.t === "str") return v.v.split("\n")[0];
  return r ? print(r) : "";
}

// stepsOfCard is the messages a card's timeline shows, in the order they
// began: what was asked, sent and run, not the reads, answers, notes or
// publishes along the way. "Show all" shows those too.
function stepsOfCard(c) {
  return c.is.filter((i, n) => {
    const e = entries[i];
    if (n === 0) return true;
    if (e.kind === "note" || e.kind === "pub" || isDone(e) || e.to?.endsWith("/whiff/asked")) return false;
    return !(e.kind === "call" && isRead(e.to));
  });
}

// stepRow is a message of a card's timeline: a dot for how it went, who
// sent it where, what it asked or how it came back, and how long it took,
// to its answer for one answered later.
function stepRow(c, i) {
  const e = entries[i];
  let k = kindOf(e.reply), end = null;
  if (k === "pending") {
    end = c.dones.get(String(pendingOf(e.reply)));
    k = end ? kindOf(end.args.v[1]) : e.gate?.startsWith("held") ? "held" : "running";
  }
  const r = end?.args.v[1];
  const job = e.to === "jobs/run" && e.args?.v?.length;
  const exit = field(r?.v?.[0], "exit");
  if (job && k === "ok" && exit && exit.v !== "0") k = "err";
  const li = el("li", "step " + (k || "ok"));
  li.dataset.i = i;
  const what = el("span", "step-what", whoOf(e) + " → ");
  what.append(el("b", "", e.kind === "eval" ? "eval" : e.kind === "pub" ? fn(e) : ":" + e.to));
  let sum = job ? jobName(e.args.v[0]) + (end ? (k === "ok" ? " passed" : " failed") : "") : end ? resultText(r) : askText(e);
  if (!job && !end && k && k !== "ok" && k !== "running" && k !== "held") sum = errText(e.reply);
  const how = el("span", "step-sum", sum);
  // Only a gate that stopped it is worth a word here; the pane says the rest.
  if (/^(held|refused)/.test(e.gate || "")) how.prepend(gateBadge(e.gate), " ");
  const ms = end ? Date.parse(end.at) - Date.parse(e.at) : k === "running" ? null : e.took;
  li.append(el("span", "dot"), what, how, el("span", "took", ms === null ? "running " + roughly(Date.now() - Date.parse(e.at)) : took(ms)));
  li.title = fullTime(e.at) + "  " + what.textContent + "  " + sum;
  return li;
}

// cardEl is a request's card: collapsed, when it began, who asked whom,
// what, how it stands and what it cost; open, what was said back to you
// and its steps.
function cardEl(c) {
  const open = opened.has(c.key) || only !== null;
  const card = el("article", "card " + c.state + (open ? " is-open" : ""));
  const head = el("button", "card-head");
  head.type = "button";
  head.dataset.id = c.key;
  head.setAttribute("aria-expanded", String(open));
  const r = c.root;
  // An agent is asked as itself; a service by the function called.
  const whom = r.kind === "eval" ? "eval" : r.kind === "pub" ? fn(r) : isAgent(to(r)) || !r.to ? to(r) : r.to;
  const who = whoOf(r);
  head.append(el("span", "time", time(r.at)), el("span", "who", who[0].toUpperCase() + who.slice(1) + " → " + whom),
    el("span", "ask", askText(r) || fn(r)), el("span", "caret", open ? "▾" : "▸"));
  if (c.project) {
    const p = el("span", "project", c.project);
    p.classList.add(hueOf(c.project));
    p.dataset.project = c.project;
    p.title = `in ${c.project}: show only its requests`;
    head.querySelector(".who").append(p);
  }
  const line = el("span", "outcome-line");
  line.append(el("span", "outcome", c.label));
  const meta = [c.run !== null && "run " + c.run, c.state === "running" && c.job, c.rounds && plural(c.rounds, "review round"),
    c.cost > 0 && "$" + c.cost.toFixed(2), !c.run && !c.rounds && c.is.length > 1 && plural(c.is.length, "message")].filter(Boolean);
  if (meta.length) line.append(el("span", "meta", meta.join(" · ")));
  head.append(line);
  head.title = fullTime(r.at) + "  " + c.label;
  card.append(head);
  if (!open) return card;
  const body = el("div", "card-body");
  // What was said back to you: a message to :me, or the answer itself.
  let said = c.said ? [whoOf(c.said), c.said.args.v[0].v] : null;
  const answer = c.dones.get(String(pendingOf(r.reply)))?.args.v[1] ?? r.reply;
  if (!said && kindOf(answer) === "ok" && answer.v[0]?.t === "str" && answer.v[0].v) said = [whom, answer.v[0].v];
  if (said) {
    const box = el("div", "said-box");
    box.append(el("div", "said-by", said[0] + " replied"), el("p", "", said[1]));
    body.append(box);
  }
  // Its chain as steps, as lanes, an address each and an arrow a
  // message, or as every message, indented under what led to it.
  const view = cardView.get(c.key) || "steps";
  const seg = el("div", "view-seg");
  seg.setAttribute("role", "group");
  seg.setAttribute("aria-label", "Show the chain as");
  for (const [v, label, title] of [["steps", "Steps", "what was asked, sent and run"], ["lanes", "Lanes", "an address each, an arrow a message: who talks to whom, in order"],
    ["all", `All ${c.is.length} messages`, "every message, reads, answers and notes too, under the one that led to it"]]) {
    const b = el("button", "", label);
    b.type = "button";
    b.title = title;
    b.dataset.view = v;
    b.dataset.id = c.key;
    b.setAttribute("aria-pressed", String(v === view));
    seg.append(b);
  }
  body.append(seg);
  if (view === "lanes") body.append(lanesEl(c));
  else if (view === "all") {
    const list = el("div", "every-list");
    for (const [i, depth] of c.chain) list.append(chainRow(entries[i], i, depth, true));
    body.append(list);
  } else {
    const list = el("ol", "steps");
    for (const i of stepsOfCard(c)) list.append(stepRow(c, i));
    body.append(list);
  }
  card.append(body);
  return card;
}

// bgEl is background events in a row, between requests, folded to "4
// background events" and what they were, newest first.
function bgEl(run) {
  const first = entries[run.at(-1)[0][0]];
  const key = keyOf(first);
  const open = showBg || openBg.has(key) || (selected !== null && run.some((ch) => ch[0][0] === selected));
  const box = el("div", "bg" + (open ? " is-open" : ""));
  const h = el("button", "bg-head");
  h.type = "button";
  h.dataset.id = key;
  h.setAttribute("aria-expanded", String(open));
  const what = new Map();
  for (const ch of run) {
    const e = entries[ch[0][0]];
    const name = e.kind === "note" ? "notes" : e.kind === "pub" ? fn(e) : e.kind === "eval" ? "eval" : ":" + e.to;
    what.set(name, (what.get(name) || 0) + 1);
  }
  h.append(el("span", "caret", open ? "▾" : "▸"), el("span", "n-bg", plural(run.length, "background event")),
    el("span", "what-bg", [...what].map(([n, x]) => n + (x > 1 ? " ×" + x : "")).join(", ")));
  h.title = time(first.at) + "–" + time(entries[run[0][0][0]].at);
  box.append(h);
  if (open) {
    for (const ch of run) {
      const i = ch[0][0], r = chainRow(entries[i], i, 0);
      if (ch.length > 1) (r.querySelector(".args") || r.querySelector(".note-text"))?.append(" ", el("span", "times", "+" + (ch.length - 1)));
      box.append(r);
    }
  }
  return box;
}

// chainRow is a message as one line: when, who sent it where, its
// arguments, and how it came back if not ok. In a chain, whose heading says
// when it began, a row's time is only in its title.
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

// Pictures of who talks to whom: a card's chain as lanes, and Activity as
// a map. Both are SVG, in the page's colours.
const SVG = "http://www.w3.org/2000/svg";
function svgEl(tag, attrs = {}, text) {
  const e = document.createElementNS(SVG, tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  if (text !== undefined) e.textContent = text;
  return e;
}
const pct = (x) => (x * 100).toFixed(3) + "%";

// placeOf is where an address is drawn: an agent's session, agent/lead,
// as the agent, lead, since it acts for it.
function placeOf(a) {
  const name = a.startsWith("agent/") ? a.slice("agent/".length) : "";
  return name && services.get(name)?.agent ? name : a;
}
const placeName = (a) => (a === "client" ? "you" : ":" + a);

// arrowLabel is a message on its arrow: what it asked, or for an answer
// how it came back.
function arrowLabel(e) {
  if (isDone(e)) return "↩ " + resultText(e.args.v[1]);
  if (e.kind === "eval") return "eval " + askText(e);
  if (e.to === "jobs/run" && e.args?.v?.length) return "run " + jobName(e.args.v[0]);
  return fn(e) + (askText(e) ? " " + askText(e) : "");
}

// lanesEl is a card's chain as lanes: an address a lane, you first, and an
// arrow a message, in the order they began, answers back to who asked;
// reads, notes and whiff's own notices left out. Each arrow is a message:
// click it for its details.
function lanesEl(c) {
  const is = c.is.filter((i, n) => {
    const e = entries[i];
    if (e.kind === "note" || e.to?.endsWith("/whiff/asked")) return false;
    return n === 0 || !(e.kind === "call" && isRead(e.to));
  });
  const lanes = [];
  const lane = (a) => {
    a = placeOf(a);
    if (!lanes.includes(a)) lanes.push(a);
    return lanes.indexOf(a);
  };
  lane("client");
  const rows = is.map((i) => {
    const e = entries[i];
    return { i, e, a: lane(from(e)), b: e.kind === "eval" ? lane(from(e)) : lane(to(e)) };
  });
  const n = lanes.length, H = 28, top = 34, height = top + rows.length * H + 8;
  const svg = svgEl("svg", { class: "lanes", width: "100%", height, role: "img", "aria-label": "The chain as lanes: who sent whom what, in order" });
  lanes.forEach((a, k) => {
    const x = pct((k + 0.5) / n);
    const head = svgEl("text", { x, y: 14, "text-anchor": "middle", class: "lane-name" + (a === "client" ? " you" : "") }, placeName(a));
    if (a !== "client") head.dataset.address = a;
    svg.append(head, svgEl("line", { x1: x, y1: 22, x2: x, y2: height, class: "lifeline" }));
  });
  rows.forEach(({ i, e, a, b }, r) => {
    const y = top + r * H + 14;
    let k = kindOf(e.reply);
    if (isDone(e)) k = kindOf(e.args.v[1]);
    const g = svgEl("g", { class: "lane-row " + (isDone(e) ? "done" : e.kind) + " " + k });
    g.dataset.i = i;
    g.append(svgEl("rect", { x: 0, y: y - 15, width: "100%", height: H, class: "hit" }));
    const xa = (a + 0.5) / n, xb = (b + 0.5) / n;
    let span, lx, anchor;
    if (a === b) {
      const out = Math.min(0.04, 0.4 / n);
      g.append(svgEl("line", { x1: pct(xa), y1: y - 6, x2: pct(xa + out), y2: y - 6 }),
        svgEl("line", { x1: pct(xa + out), y1: y - 6, x2: pct(xa + out), y2: y + 4 }),
        svgEl("line", { x1: pct(xa + out), y1: y + 4, x2: pct(xa), y2: y + 4, "marker-end": "url(#ah)" }));
      span = 1 - xa - out;
      lx = pct(xa + out + 0.008);
      anchor = "start";
    } else {
      g.append(svgEl("line", { x1: pct(xa), y1: y, x2: pct(xb), y2: y, "marker-end": "url(#ah)" }));
      span = Math.abs(xb - xa);
      lx = pct((xa + xb) / 2);
      anchor = "middle";
    }
    const full = arrowLabel(e);
    const label = svgEl("text", { x: lx, y: a === b ? y + 1 : y - 5, "text-anchor": anchor, class: "arrow-label" }, full);
    label.dataset.span = span; // of the drawing's width: fitLabels cuts it to fit
    label.full = full;
    g.append(label);
    g.append(svgEl("title", {}, `${fullTime(e.at)}  ${placeName(placeOf(from(e)))} → ${placeName(lanes[b])}\n${full}`));
    svg.append(g);
  });
  const box = el("div", "lanes-box");
  box.append(svg);
  fitting.observe(svg);
  return box;
}

// fitLabels cuts each arrow's label in svg to the room it has, as wide as
// its arrow, each time the drawing changes width, as the details open.
function fitLabels(svg) {
  const w = svg.getBoundingClientRect().width;
  if (!w) return;
  for (const t of svg.querySelectorAll(".arrow-label")) {
    const full = t.full ?? t.textContent;
    const max = Math.max(6, Math.floor((parseFloat(t.dataset.span) * w - 8) / 6.9));
    t.textContent = full.length > max ? full.slice(0, max - 1) + "…" : full;
  }
}
const fitting = new ResizeObserver((es) => { for (const e of es) fitLabels(e.target); });

// mapEl is the messages at is as a map: an address a box, in columns by
// how far they are from you, and an arrow for each pair that talked, as
// thick as how often, amber or red if some failed. Answers and whiff's
// notices go without saying. Click a box for the address, an arrow for
// its messages.
function mapEl(is, projAt, perProject) {
  const edges = new Map(), first = new Map(), handled = new Map();
  const answered = new Set(); // what answered something, as one that is gone once did
  const drawn = (e) => !(e.kind === "note" || isDone(e) || e.to?.endsWith("/whiff/asked"));
  // Bands: an address is in a project's band when two thirds of the
  // messages it sent or got are of that project's requests, so one that
  // another project reaches into stays in its own, and the arrow in shows
  // as crossing; one used as much by several, or by none, as you and
  // :jobs, is shared. A band folded is one box, "#project".
  const tally = new Map(); // address → project ("" for none) → messages
  const inBand = (x, p) => {
    const t = tally.get(x) || tally.set(x, new Map()).get(x);
    t.set(p, (t.get(p) || 0) + 1);
  };
  for (const i of is) {
    const e = entries[i];
    if (!drawn(e)) continue;
    inBand(placeOf(from(e)), projAt.get(i) || "");
    if (e.kind !== "eval") inBand(placeOf(to(e)), projAt.get(i) || "");
  }
  const bandOf = new Map();
  for (const [x, t] of tally) {
    const all = [...t.values()].reduce((a, b) => a + b, 0);
    const [p, n] = [...t].sort((a, b) => b[1] - a[1])[0];
    bandOf.set(x, n * 3 >= all * 2 ? p : "");
  }
  bandOf.set("client", "");
  if (bandOf.has("me")) bandOf.set("me", "");
  const members = new Map();
  for (const [x, b] of bandOf) if (b) members.set(b, (members.get(b) || 0) + 1);
  const folded = (x) => {
    const b = bandOf.get(x);
    return b && foldedBands.has(b) ? "#" + b : x;
  };
  for (const b of members.keys()) bandOf.set("#" + b, b);
  for (const i of is) {
    const e = entries[i];
    if (e.kind !== "note" && e.to && e.reply && field(e.reply.v?.[0], "kind")?.v !== "no-address") answered.add(placeOf(to(e)));
    if (!drawn(e)) continue;
    const a = folded(placeOf(from(e))), b = e.kind === "eval" ? a : folded(placeOf(to(e)));
    for (const x of [a, b]) if (!first.has(x)) first.set(x, i);
    const key = a + "\t" + b;
    if (!edges.has(key)) edges.set(key, { a, b, from: from(e), to: e.kind === "eval" ? from(e) : to(e), n: 0, err: 0, fault: 0, pub: true, last: i });
    const ed = edges.get(key), k = kindOf(e.reply);
    ed.n++;
    ed.last = i;
    if (k === "err") ed.err++;
    if (k === "fault") ed.fault++;
    if (e.kind !== "pub") ed.pub = false;
    handled.set(b, (handled.get(b) || 0) + 1);
  }
  if (!edges.size) return el("p", "empty", "Nothing to map yet: the requests shown, and the background once you show it, are drawn here.");
  // Columns: you first, with :me, your inbox, and whoever else begins
  // work, an agent's session or a service on its timer; then each address
  // a column right of the furthest that sends it messages, so work flows
  // left to right. An arrow that closes a loop goes back, under.
  const out = new Map();
  for (const ed of edges.values()) if (ed.a !== ed.b && ed.b !== "me") (out.get(ed.a) || out.set(ed.a, []).get(ed.a)).push(ed.b);
  const into = new Set([...edges.values()].filter((ed) => ed.a !== ed.b).map((ed) => ed.b));
  const roots = ["client", ...[...first.keys()].filter((x) => x !== "client" && !into.has(x)), ...first.keys()].filter((x) => first.has(x));
  const seen = new Map(), back = new Set(), order = [];
  const visit = (x) => {
    seen.set(x, 1);
    for (const y of out.get(x) || []) {
      if (seen.get(y) === 1) back.add(x + "\t" + y);
      else if (!seen.has(y)) visit(y);
    }
    seen.set(x, 2);
    order.push(x);
  };
  for (const r of roots) if (!seen.has(r)) visit(r);
  const depth = new Map();
  for (const x of order.reverse()) {
    if (!depth.has(x)) depth.set(x, 0);
    for (const y of out.get(x) || []) if (!back.has(x + "\t" + y)) depth.set(y, Math.max(depth.get(y) ?? 0, depth.get(x) + 1));
  }
  if (first.has("me")) depth.set("me", 0);
  const cols = [];
  for (const [x, d] of depth) (cols[d] ||= []).push(x);
  for (let d = 0; d < cols.length; d++) cols[d] ||= [];
  // You on top, then in the order they first show; each further column in
  // the order of who sends to it, so fewer arrows cross.
  const row = new Map();
  cols[0].sort((x, y) => (x === "client" ? -1 : y === "client" ? 1 : x === "me" ? -1 : y === "me" ? 1 : first.get(x) - first.get(y)));
  cols[0].forEach((x, r) => row.set(x, r));
  for (let d = 1; d < cols.length; d++) {
    const at = (x) => {
      const from = [...edges.values()].filter((ed) => ed.b === x && row.has(ed.a)).map((ed) => row.get(ed.a));
      return from.length ? from.reduce((a, b) => a + b, 0) / from.length : 1e6 + first.get(x);
    };
    cols[d].sort((x, y) => at(x) - at(y) || first.get(x) - first.get(y));
    cols[d].forEach((x, r) => row.set(x, r));
  }
  const NW = 168, NH = 44, CG = 120, RG = 30, PAD = 24, HEAD = 34, GAP = 22;
  const pos = new Map();
  const W = PAD * 2 + 70 + cols.length * (NW + CG) - CG + 60;
  // A band a project, under the shared one, in the order they first
  // show; each column's rows in it in the order above. Without projects,
  // no bands.
  const bands = ["", ...[...members.keys()].sort((p, q) => Math.min(...[...first.keys()].filter((x) => bandOf.get(x) === p).map((x) => first.get(x)))
    - Math.min(...[...first.keys()].filter((x) => bandOf.get(x) === q).map((x) => first.get(x))))];
  const banded = bands.length > 1;
  const bandsG = svgEl("g");
  let top = PAD;
  for (const b of bands) {
    const nodes = cols.map((col) => col.filter((x) => (bandOf.get(x) || "") === b));
    const n = Math.max(0, ...nodes.map((c) => c.length));
    if (!n && !b) continue;
    const y0 = top + (banded ? HEAD : 0);
    // Room on the left for what goes around a column.
    nodes.forEach((col, d) => col.forEach((x, r) => pos.set(x, { x: PAD + 70 + d * (NW + CG), y: y0 + r * (NH + RG) })));
    const h = (banded ? HEAD : 0) + n * (NH + RG) - RG + (banded ? 16 : 0);
    if (banded) bandsG.append(bandEl(b, PAD - 12, top - 8, W - 2 * PAD + 24, h + 8, perProject.get(b), members.get(b) || 0));
    top += h + GAP + (banded ? 0 : 28);
  }
  const H = top + 28;
  const svg = svgEl("svg", { class: "map", viewBox: `0 0 ${W} ${H}`, width: W, height: H, role: "img", "aria-label": "Who talks to whom" });
  const edgesG = svgEl("g"), nodesG = svgEl("g");
  // Ports: a node's arrows on to the next columns leave its right side
  // spread top to bottom in the order of where they go, and come into
  // the left side so, rather than all from one point.
  const fwd = [...edges.values()].filter((ed) => depth.get(ed.b) > depth.get(ed.a));
  const port = new Map();
  const spreadPorts = (side, key, other) => {
    const by = new Map();
    for (const ed of fwd) (by.get(ed[key]) || by.set(ed[key], []).get(ed[key])).push(ed);
    for (const [, eds] of by) {
      eds.sort((e1, e2) => pos.get(e1[other]).y - pos.get(e2[other]).y);
      eds.forEach((ed, k) => port.set(side + (key === "a" ? ed.a + "\t" + ed.b : ed.a + "\t" + ed.b), 8 + (NH - 16) * (eds.length === 1 ? 0.5 : k / (eds.length - 1))));
    }
  };
  spreadPorts("out", "a", "b");
  spreadPorts("in", "b", "a");
  for (const ed of edges.values()) {
    const p = pos.get(ed.a), q = pos.get(ed.b);
    let d, mid;
    if (ed.a === ed.b) {
      const x = p.x + NW, y = p.y + 10;
      d = `M ${x} ${y} C ${x + 34} ${y - 16}, ${x + 34} ${y + 40}, ${x} ${y + 24}`;
      mid = [x + 26, y + 12];
    } else {
      const da = depth.get(ed.a), db = depth.get(ed.b);
      let x0, y0, x1, y1, x2, y2, x3, y3;
      if (db > da) { // on, to the right: side to side
        const k = ed.a + "\t" + ed.b;
        [x0, y0, x3, y3] = [p.x + NW, p.y + port.get("out" + k), q.x, q.y + port.get("in" + k)];
        [x1, y1, x2, y2] = [x0 + (x3 - x0) * 0.45, y0, x3 - (x3 - x0) * 0.45, y3];
      } else if (db === da) { // in its column: around the left
        [x0, y0, x3, y3] = [p.x, p.y + NH / 2 + 6, q.x, q.y + NH / 2 + 6];
        [x1, y1, x2, y2] = [x0 - 70, y0, x3 - 70, y3];
      } else { // back, to the left: under, from bottom to bottom
        [x0, y0, x3, y3] = [p.x + NW / 2 - 20, p.y + NH, q.x + NW / 2 + 20, q.y + NH];
        [x1, y1, x2, y2] = [x0, y0 + 50, x3, y3 + 50];
      }
      d = `M ${x0} ${y0} C ${x1} ${y1}, ${x2} ${y2}, ${x3} ${y3}`;
      // Its count near where it arrives, where arrows part.
      const t = db > da ? 0.78 : 0.5, u = 1 - t;
      const bez = (a, b, c, e) => u * u * u * a + 3 * u * u * t * b + 3 * u * t * t * c + t * t * t * e;
      mid = [bez(x0, x1, x2, x3), bez(y0, y1, y2, y3)];
    }
    // From one project's band to another's: work that crosses projects.
    const ba = bandOf.get(ed.a), bb = bandOf.get(ed.b), cross = ba && bb && ba !== bb;
    const g = svgEl("g", { class: "edge" + (ed.fault ? " fault" : ed.err ? " err" : "") + (ed.pub ? " pub" : "") + (cross ? " cross" : "") });
    g.dataset.query = [ed.a.startsWith("#") ? "" : `from:${ed.from}`, ed.b.startsWith("#") ? "" : `to:${ed.to}`].filter(Boolean).join(" ") || `from:${ed.from}`;
    g.append(svgEl("path", { d, class: "hit" }), svgEl("path", { d, "stroke-width": 1.2 + Math.min(4, Math.log2(ed.n)), "marker-end": "url(#ah)" }));
    const bad = ed.fault ? ` · ${ed.fault} faulted` : ed.err ? ` · ${ed.err} failed` : "";
    g.append(svgEl("text", { x: mid[0], y: mid[1] - 4, "text-anchor": "middle", class: "edge-label" }, ed.n + bad));
    g.append(svgEl("title", {}, (cross ? `crosses from ${ba} to ${bb}: work of one project sent to another's\n` : "") + `${placeName(ed.a)} → ${placeName(ed.b)}: ${plural(ed.n, ed.pub ? "publish" : "message")}${bad}\nlast: ${arrowLabel(entries[ed.last]).slice(0, 120)}\nclick to show them`));
    edgesG.append(g);
  }
  for (const [x, p] of pos) {
    if (x.startsWith("#")) {
      const b = x.slice(1), g = svgEl("g", { class: "node band-node" });
      g.dataset.band = b;
      g.append(svgEl("rect", { x: p.x, y: p.y, width: NW, height: NH, rx: 8 }),
        svgEl("text", { x: p.x + 14, y: p.y + 20, class: "node-name" }, b.slice(0, 20)),
        svgEl("text", { x: p.x + 14, y: p.y + 35, class: "node-sub" }, `project · ${plural(members.get(b), "address", "addresses")}`),
        svgEl("title", {}, `${b}, folded: click to unfold its addresses`));
      nodesG.append(g);
      continue;
    }
    const st = services.get(x), fault = faulted.has(x) && !st;
    const cls = x === "client" ? "you" : fault ? "fault" : st ? statusClass(st.status) : "";
    const g = svgEl("g", { class: "node " + (cls || "quiet") + (isAgent(x) ? " agent" : "") + (x === shownAddress ? " selected" : "") });
    if (x !== "client") g.dataset.address = x;
    // What it is: an agent, a puff service of yours, a client, one of
    // whiff's own, as :jobs; or not there now: gone, if it answered
    // something once, as a service whose file you removed, else no such
    // address, as a mistyped name.
    const a = addresses.find((y) => y.address === x);
    const what = isAgent(x) ? "agent" : x === "me" ? "your inbox" : st ? "service" : a?.client ? "client" : a ? "built-in"
      : answered.has(x) ? "gone" : "no such address";
    if (x !== "client" && (what === "gone" || what === "no such address")) g.classList.add("unknown");
    const sub = x === "client" ? "you" : [what,
      st && st.status !== "ok" && st.status !== "idle" ? st.status.replace("-", " ") : "", handled.get(x) ? handled.get(x) + " in" : ""].filter(Boolean).join(" · ");
    g.append(svgEl("rect", { x: p.x, y: p.y, width: NW, height: NH, rx: 8 }),
      svgEl("circle", { cx: p.x + 14, cy: p.y + 16, r: 4, class: "dot" }),
      svgEl("text", { x: p.x + 26, y: p.y + 20, class: "node-name" }, placeName(x).slice(0, 20)),
      svgEl("text", { x: p.x + 26, y: p.y + 35, class: "node-sub" }, sub),
      svgEl("title", {}, x === "client" ? "you, and sniff and the CLI"
        : what === "gone" ? placeName(x) + " answered once, but isn't there now: its file was removed, or failed to load"
        : what === "no such address" ? "nothing ever answered at " + placeName(x) + ": a mistyped name?"
        : placeName(x) + (st?.note ? ": " + st.note : "") + "\nclick for its functions, status and state"));
    nodesG.append(g);
  }
  svg.append(bandsG, edgesG, nodesG);
  const box = el("div", "map-box");
  box.append(svg, el("p", "hint map-hint", (showBg ? "Every message shown" : "The requests shown, not the background") +
    ": click an address for its details, an arrow for its messages. The filter narrows it too."));
  return box;
}

// The bands folded on the map, kept in this browser.
const foldedBands = new Set(store.get("folded-bands") || []);
function foldBand(b) {
  if (!foldedBands.delete(b)) foldedBands.add(b);
  store.set("folded-bands", [...foldedBands]);
  redraw();
}

// teams are each project's team, as :board/team gives it, for its
// band's heading and the status strip: fetched once one shows, and again
// once history says a team changed, a run was built or one moved on;
// not on a read, as fetching a team is. null while it is fetched.
const teams = new Map();
function teamOf(b) {
  if (!services.has("board")) return null;
  if (!teams.has(b)) {
    teams.set(b, null);
    getJSON("api/view?target=board/team&args=" + encodeURIComponent(`[${JSON.stringify(b)}]`)).then((r) => {
      const t = r.t === "tag" && r.n === "ok" ? r.v[0] : null;
      if (!t) return;
      const s = (k) => field(t, k)?.v ?? "";
      const n = (k) => Number(field(t, k)?.v ?? 0);
      teams.set(b, { lead: s("lead"), dev: s("dev"), reviewer: s("reviewer"), going: n("going"), most: n("most"), spent: n("spent"), day: n("day") });
      redrawSoon();
      drawPills();
    }, () => {});
  }
  return teams.get(b);
}

// usd is dollars to the cent, as "$0.40".
const usd = (x) => "$" + x.toFixed(2);

// teamSpend is what team t spent today, of its day if it has one.
function teamSpend(t) {
  return t.day > 0 ? `${usd(t.spent)} of ${usd(t.day)} today` : `${usd(t.spent)} today`;
}

// teamLine is project b's team as a line, for its band's heading.
function teamLine(b) {
  const t = teamOf(b);
  if (!t) return "";
  return ["lead :" + t.lead, "dev " + (t.dev || "default"), "review " + (t.reviewer || "default"),
    `${t.going} of ${t.most} running`, teamSpend(t)].join(" · ");
}

// boardOf is project b's board, as a tab's href.
function boardOf(b) {
  return "/view/board/for?args=" + encodeURIComponent(`[${JSON.stringify(b)}]`);
}

// bandEl draws a project's band behind its addresses, its heading saying
// what of the requests shown are its and what they cost; for "", what the
// projects share.
function bandEl(b, x, y, w, h, info, n) {
  const g = svgEl("g", { class: "band" + (b ? "" : " shared") });
  if (b) g.classList.add(hueOf(b));
  g.append(svgEl("rect", { x, y, width: w, height: h, rx: 12 }));
  if (!b) {
    g.append(svgEl("text", { x: x + 14, y: y + 22, class: "band-name" }, "shared"),
      svgEl("text", { x: x + 72, y: y + 22, class: "band-sub" }, "you, and what several projects use"));
    return g;
  }
  const folded = foldedBands.has(b);
  const caret = svgEl("text", { x: x + 14, y: y + 22, class: "band-caret" }, folded ? "▸" : "▾");
  caret.dataset.band = b;
  caret.append(svgEl("title", {}, folded ? "unfold its addresses" : "fold it into one box"));
  const name = svgEl("text", { x: x + 30, y: y + 22, class: "band-name link" }, b);
  name.dataset.project = b;
  name.append(svgEl("title", {}, `show only ${b}'s requests`));
  const sub = [info && plural(info.requests, "request"), plural(n, "address", "addresses"), info?.cost > 0 && "$" + info.cost.toFixed(2), teamLine(b)].filter(Boolean).join(" · ");
  g.append(caret, name, svgEl("text", { x: x + 38 + b.length * 8.5, y: y + 22, class: "band-sub" }, sub));
  // Its lead is asked from its board, whose team says what it runs.
  if (services.has("board")) {
    const ask = svgEl("text", { x: x + w - 14, y: y + 22, class: "band-ask link", "text-anchor": "end" }, "Ask its lead…");
    ask.dataset.board = boardOf(b);
    ask.append(svgEl("title", {}, `open ${b}'s board, to ask its lead`));
    g.append(ask);
  }
  return g;
}

// setLayout shows Activity as cards or as the map, and remembers it.
function setLayout(v) {
  layout = v;
  store.set("layout", v);
  $("activity-title").textContent = v === "map" ? "Map" : "Activity";
  // On the map, details dock under it, so the map keeps its width.
  $("activity").classList.toggle("dock-bottom", v === "map");
  activate(null);
  redraw();
}

// The arrowheads, once for the page, in each line's own colour.
document.body.append((() => {
  const s = svgEl("svg", { width: 0, height: 0, "aria-hidden": "true", style: "position:absolute" });
  const defs = svgEl("defs"), m = svgEl("marker", { id: "ah", viewBox: "0 0 10 10", refX: 9, refY: 5, markerWidth: 8, markerHeight: 8, markerUnits: "userSpaceOnUse", orient: "auto-start-reverse" });
  m.append(svgEl("path", { d: "M 0 0 L 10 5 L 0 10 z", fill: "context-stroke" }));
  defs.append(m);
  s.append(defs);
  return s;
})());

// redraw draws Activity: a card a request, newest first, and the
// background events between them folded into a row. A request to an agent
// that is still thinking runs until it isn't.
function redraw() {
  const rows = $("rows");
  // A heading you went to, by j and k or a click, stays where you are.
  const f = rows.contains(document.activeElement) ? document.activeElement : null;
  const again = f?.dataset.id ? `.${f.classList[0]}[data-id="${CSS.escape(f.dataset.id)}"]` : null;
  const busy = new Set([...services.values()].filter((s) => s.agent && (s.status === "thinking" || s.status === "busy")).map((s) => s.service));
  const all = chains();
  const frag = document.createDocumentFragment();
  let run = null, running = 0;
  const shown = []; // the entries the map draws: the requests', and the background's if shown
  const projAt = new Map(), perProject = new Map(); // an entry's project; a project's requests and cost
  const flush = () => {
    if (run) frag.append(bgEl(run));
    run = null;
  };
  for (let n = all.length - 1; n >= 0; n--) {
    const ch = all[n];
    if (only !== null && !ch.some(([i]) => entries[i].id === only)) continue;
    if (project && only === null && projectIn(ch) !== project) continue;
    const p = projectIn(ch);
    if (p) for (const [i] of ch) projAt.set(i, p);
    if (only === null && isBackground(ch)) {
      (run ||= []).push(ch);
      if (showBg) for (const [i] of ch) shown.push(i);
      continue;
    }
    for (const [i] of ch) shown.push(i);
    flush();
    const c = cardOf(ch);
    if (p) {
      const info = perProject.get(p) || perProject.set(p, { requests: 0, cost: 0 }).get(p);
      info.requests++;
      info.cost += c.cost;
    }
    const agent = [to(c.root), c.root.from].find((a) => busy.has(a));
    if (agent) {
      busy.delete(agent);
      if (c.state === "ok") { c.state = "running"; c.label = "Running"; }
    }
    if (c.state === "running") running++;
    frag.append(cardEl(c));
  }
  flush();
  if (layout === "map") rows.replaceChildren(mapEl(shown, projAt, perProject));
  else rows.replaceChildren(frag);
  $("empty").hidden = entries.length > 0;
  if (counts.running !== running) {
    counts.running = running;
    drawPills();
  }
  if (selected !== null) mark(selected);
  if (again) rows.querySelector(again)?.focus({ preventScroll: true });
}

// redrawSoon redraws once for the entries that come in a frame.
let soon = null;
function redrawSoon() {
  soon ??= requestAnimationFrame(() => {
    soon = null;
    redraw();
  });
}

// showOnly shows only the chain with id in it, open; or, for null, every
// chain again.
function showOnly(id) {
  only = id;
  $("only").hidden = id === null;
  activate(null);
  redraw();
}
$("only").onclick = () => showOnly(null);

// showProject shows only the requests whose work is in name, or, for "",
// every one again.
function showProject(name) {
  project = name;
  const u = new URLSearchParams(location.search);
  if (name) u.set("p", name);
  else u.delete("p");
  history.replaceState(null, "", location.pathname + (u.size ? "?" + u : "") + location.hash);
  drawProject();
  activate(null);
  markSide();
  redraw();
}

// drawProject shows the project shown, if one is, beside the filter, and
// points the sidebar's Board at its tickets.
function drawProject() {
  $("project").hidden = !project;
  $("project").textContent = project + " ×";
  $("project").className = "chip project " + hueOf(project);
  $("side-board").href = project ? boardOf(project) : "/view/board/show";
}
$("project").onclick = () => showProject("");
drawProject();

// showBackground shows every background event, or folds them again.
function showBackground(on) {
  showBg = on;
  store.set("background", on);
  $("show-bg").setAttribute("aria-pressed", String(on));
  $("show-bg").textContent = on ? "Hide background" : "Show background";
  redraw();
}
$("show-bg").onclick = () => showBackground(!showBg);

// append adds e in order of when it began: history records a call when it
// ends, after the calls it made. A new address loads the addresses again.
const seen = new Set();
function append(e) {
  let i = entries.length;
  while (i > 0 && entries[i - 1].us > e.us) i--;
  entries.splice(i, 0, e);
  noteFault(e);
  servicesSoon();
  if (/^board\/(set-team|reset-team|build)$|:pipeline\/(step|done)$/.test(e.to || "")) {
    teams.clear();
    pillsSoon();
  }
  if (selected !== null && selected >= i) selected++;
  if (e.kind !== "note") {
    for (const a of [from(e), to(e)]) {
      if (a === "client" || seen.has(a)) continue;
      seen.add(a);
      if (!addresses.some((x) => x.address === a)) reloadSoon();
    }
  }
  $("empty").hidden = true;
  redrawSoon();
}

function mark(i) {
  document.querySelectorAll("#rows .selected").forEach((r) => r.classList.remove("selected"));
  document.querySelector(`#rows [data-i="${i}"]`)?.classList.add("selected");
}

function section(title, ...kids) {
  const f = document.createDocumentFragment();
  f.append(el("h3", "", title), ...kids);
  return f;
}

// pane fills the detail pane, shown, with a heading: a kicker, "CALL",
// over its title, and a button that closes it.
function pane(kicker, title) {
  const d = $("detail");
  const head = el("div", "pane-head");
  const words = el("div");
  words.append(el("div", "kicker", kicker), el("h2", "", title));
  const close = el("button", "close-pane", "×");
  close.type = "button";
  close.title = "Close (esc)";
  close.setAttribute("aria-label", "Close the details");
  close.onclick = closeDetail;
  head.append(words, close);
  d.replaceChildren(head);
  d.hidden = false;
  return d;
}

// markAddress outlines on the map the address the details show, or none.
let shownAddress = null;
function markAddress(name) {
  shownAddress = name;
  for (const g of document.querySelectorAll(".map .node")) g.classList.toggle("selected", g.dataset.address === name);
}

function closeDetail() {
  markAddress(null);
  selected = null;
  mark(-1);
  $("detail").hidden = true;
  history.replaceState(null, "", location.pathname + location.search);
}

// reveal opens what shows entry i: its card, and every message of it if
// it isn't a step.
function reveal(i) {
  const at = () => $("rows").querySelector(`[data-i="${i}"]`);
  if (at()) return;
  const key = keyOf(entries[chainOf(i)[0]]);
  opened.add(key);
  redraw();
  if (at()) return;
  cardView.set(key, "all");
  redraw();
}

// argsBox is a message's arguments: by name, as its function names them
// or as the one map it sent has them, strings as text; else as puff.
function argsBox(e) {
  const vs = e.args.v;
  if (!vs.length) return el("p", "hint", "None.");
  const names = params.get(e.to);
  let pairs = null;
  if (vs.length === 1 && vs[0].t === "map" && vs[0].v.length && vs[0].v.every(([k]) => k.t === "kw")) pairs = vs[0].v.map(([k, v]) => [k.v, v]);
  else if (names?.length === vs.length) pairs = vs.map((v, n) => [names[n], v]);
  if (!pairs) return drawn(e.args);
  const dl = el("dl", "kv");
  for (const [k, v] of pairs) dl.append(el("dt", "", k), el("dd", "", v.t === "str" ? home(v.v) : print(v)));
  return dl;
}

// rawText is a message as history keeps it, printed as puff.
function rawText(e) {
  const kv = [["kind", ":" + e.kind], ["id", e.id], ["cause", e.cause], ["origin", e.origin && ":" + e.origin],
    ["from", e.from ? ":" + e.from : "client"], ["to", e.to ? ":" + e.to : "client"], ["at", JSON.stringify(e.at)],
    ["took", e.took ? JSON.stringify(took(e.took)) : ""], ["gate", e.gate && JSON.stringify(e.gate)], ["note", e.note && JSON.stringify(e.note)],
    ["args", e.args && print(e.args)], ["reply", e.reply && print(e.reply)]].filter(([, v]) => v);
  return "{" + kv.map(([k, v]) => ":" + k + " " + v).join("\n ") + "}";
}

function showEntry(i) {
  history.replaceState(null, "", location.pathname + location.search);
  const e = entries[i];
  selected = i;
  markAddress(null);
  reveal(i);
  mark(i);
  if (e.kind === "note") {
    const d = pane("note", time(e.at));
    d.append(el("p", "", e.note));
    return;
  }
  const target = e.kind === "eval" ? "eval" : e.kind === "pub" ? fn(e) : e.to ? ":" + e.to : "client";
  const d = pane(e.kind, `${e.from || (e.origin === "agent" ? "an agent" : "you")} → ${target}`);
  const meta = el("div", "meta");
  meta.append(el("span", "", fullTime(e.at)), el("span", "badge", e.origin || "whiff"));
  if (e.took) meta.append(el("span", "", took(e.took)));
  if (e.gate) meta.append(gateBadge(e.gate));
  d.append(meta);
  const chain = chainOf(i);
  if (chain.length > 1) d.append(crumbs(chain));
  if (e.args) d.append(section("Arguments", argsBox(e)));
  // A pending's answer came later, as a done: that is its reply.
  const done = kindOf(e.reply) === "pending" ? entries.find((x) => isDone(x) && String(x.args.v[0].v) === String(pendingOf(e.reply))) : null;
  const reply = el("div", "reply-box");
  if (done) {
    reply.append(drawn(done.args.v[1], "reply"));
    d.append(section("Reply", reply, el("p", "hint", `answered ${took(Date.parse(done.at) - Date.parse(e.at))} later; at once it said ${print(e.reply)}`)));
  } else if (e.reply) {
    reply.append(drawn(e.reply, "reply"));
    d.append(section("Reply", reply));
  } else if (e.kind !== "pub" && e.kind !== "done") {
    d.append(section("Reply", el("p", "hint", "None: nobody handled it.")));
  }
  const raw = el("details", "raw");
  raw.append(el("summary", "", "Show raw message"), el("pre", "", rawText(e)));
  d.append(raw, entryActions(e));
}

// viewOf is the page of the view e answered, or null: a view a :read
// function answers can be a tab of its own, where its actions work.
function viewOf(e) {
  const view = e.reply?.t === "tag" && e.reply.n === "ok" && e.reply.v[0]?.t === "tag";
  return view && e.kind === "call" && e.args && marks.get(e.to) === "read" ? pageOf(e.to, e.args) : null;
}

// chainOf is the indexes of what led to entry i, from the entry that began
// its chain, and i last.
function chainOf(i) {
  const at = new Map(entries.map((e, j) => [e.id, j]));
  const path = [i];
  for (let e = entries[i]; e.cause && at.has(e.cause) && at.get(e.cause) < path[0]; e = entries[path[0]]) path.unshift(at.get(e.cause));
  return path;
}

// crumbs is where an entry sits in its chain, path: who began it, then
// what each message was for, "agent/3 › plan/draft › pipeline/run"; each
// but the last shows its message.
function crumbs(path) {
  const nav = el("nav", "crumbs");
  nav.setAttribute("aria-label", "Its chain");
  const root = entries[path[0]];
  nav.append(whoOf(root));
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

// entryActions are what you can do with e: call it again as you, open
// the address it went to or the view it answered, or copy it as puff.
function entryActions(e) {
  const box = el("div", "entry-actions");
  if ((e.kind === "call" || e.kind === "send") && e.to) {
    const again = el("details", "again");
    again.append(el("summary", "", "Call again"), callBox("[:" + e.to + (e.args && e.args.v.length ? " " + print(e.args).slice(1, -1) : "") + "]"));
    box.append(again);
  }
  const addr = e.kind === "eval" ? "" : to(e);
  if (addr && addr !== "client") {
    const b = el("button", "", "Open :" + addr);
    b.type = "button";
    b.title = "its functions, status and state";
    b.onclick = () => showAddress(addr);
    box.append(b);
  }
  const view = viewOf(e);
  if (view) {
    const a = el("a", "button", "Open as a tab");
    a.href = view;
    a.title = "what :" + e.to + " answers, as a tab where its actions work (enter)";
    box.append(a);
  }
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
          $("rows").querySelector(`[data-i="${i}"]`)?.scrollIntoView({ block: "center" });
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
    return;
  }
  marks = new Map();
  params = new Map();
  asks = new Set();
  for (const a of addresses) {
    for (const f of a.fns) {
      marks.set(a.address + "/" + f.fn, f.mark);
      params.set(a.address + "/" + f.fn, f.params);
      if (f.form === "ask") asks.add(a.address + "/" + f.fn);
    }
  }
  redraw(); // a client's reads go to the background, by their marks
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
  redrawSoon(); // a request to an agent runs while it thinks
  loadSpend();
  drawAgentDots();
}

// drawAgentDots puts each agent in the status strip, as a dot and its
// name: how it is, at a glance, and its details on a click.
function drawAgentDots() {
  const box = $("agent-dots");
  box.replaceChildren();
  for (const st of services.values()) {
    if (!st.agent) continue;
    const quiet = st.status === "ok" || st.status === "idle";
    const b = el("button", "agent-dot " + (statusClass(st.status) || (quiet ? "quiet" : "")), st.service);
    b.type = "button";
    b.title = `:${st.service}: ${st.status.replace("-", " ")}${st.note ? ", " + st.note : ""}`;
    b.setAttribute("aria-haspopup", "dialog");
    b.onclick = () => (agentPop.dataset.for === st.service && !agentPop.hidden ? closeAgentPop() : openAgentPop(b, st.service));
    box.append(b);
  }
}

// The agent popover: a click on an agent's dot shows, under the dot, how
// it is and what you can do from there: see its messages, or its full
// details in the pane. Esc or a click elsewhere closes it.
const agentPop = el("div", "pop");
agentPop.hidden = true;
agentPop.setAttribute("role", "dialog");
document.body.append(agentPop);

async function openAgentPop(anchor, name) {
  agentPop.dataset.for = name;
  const r = anchor.getBoundingClientRect();
  agentPop.style.top = r.bottom + 6 + "px";
  agentPop.style.left = Math.max(8, Math.min(r.left, innerWidth - 328)) + "px";
  agentPop.setAttribute("aria-label", ":" + name);
  agentPop.replaceChildren(el("p", "hint", "…"));
  agentPop.hidden = false;
  let st;
  try {
    st = await getJSON("api/service?address=" + encodeURIComponent(name));
  } catch (err) {
    if (agentPop.dataset.for === name) agentPop.replaceChildren(el("p", "hint", err.message));
    return;
  }
  if (agentPop.dataset.for !== name || agentPop.hidden) return;
  const head = el("p", "svc-head " + statusClass(st.status));
  head.append(el("span", "dot"), el("strong", "", ":" + name), " ", el("span", "", st.status.replace("-", " ")));
  const dl = el("dl");
  if (st.note) dl.append(el("dt", "", "now"), el("dd", "", st.note));
  for (const [k, v] of serviceLines(st)) if (k !== "file") dl.append(el("dt", "", k), el("dd", "", v));
  const acts = el("div", "pop-actions");
  const act = (label, title, f) => {
    const b = el("button", "", label);
    b.type = "button";
    b.title = title;
    b.onclick = () => { closeAgentPop(); f(); };
    acts.append(b);
  };
  act("Its messages", `Activity filtered to what :${name} was sent`, () => { setLayout("cards"); setQuery("to:" + name); });
  act("Details", "its functions, status, work and state, in the pane", () => { activate(null); showAddress(name); });
  const box = el("div", "svc-status");
  box.append(head, dl);
  agentPop.replaceChildren(box, acts);
}

function closeAgentPop() {
  agentPop.hidden = true;
  delete agentPop.dataset.for;
}
document.addEventListener("click", (ev) => {
  if (!agentPop.hidden && !agentPop.contains(ev.target) && !ev.target.closest(".agent-dot")) closeAgentPop();
});
addEventListener("resize", closeAgentPop);

let servicesTimer = 0;
function servicesSoon() {
  clearTimeout(servicesTimer);
  servicesTimer = setTimeout(() => { loadServices(); refreshWork(); }, 1000);
}
setInterval(() => { loadServices(); refreshWork(); }, 15000);

// refreshWork fills again each open Status that shows work, so its steps
// follow a run as it moves, and a running step's time goes on.
function refreshWork() {
  document.querySelectorAll(".svc-status.has-work").forEach((box) => fillStatus(box, box.dataset.address));
}

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
    const tasks = parseFloat(st["tasks-spent"]) > 0 ? ` (${usd(st["tasks-spent"])} of it its tasks')` : "";
    add("turns", `${st.turns} in 24h, ${usd(st.spent)} of ${usd(st.day)} a day${tasks}` + (st["last-turn"] ? `; the last ${ago(st["last-turn"])}` : ""));
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
  if (w.status !== "ended") return [`:${w.to}/${w.fn}`, w.why || (w.status === "held" ? "held for your review" : "waiting")];
  const r = w.result, v = r?.value;
  let how = typeof v === "string" ? v : "";
  if (r && !r.ok) how = "failed: " + (v?.message ?? JSON.stringify(v));
  else if (v && typeof v === "object") {
    how = [v.status, v.branch].filter(Boolean).join(" ");
    if (parseFloat(v.cost) > 0) how += ", $" + parseFloat(v.cost).toFixed(2);
  }
  return [`:${w.to}`, how];
}

// field is map tree m's value at keyword k, or undefined.
function field(m, k) {
  if (m?.t !== "map") return undefined;
  return m.v.find(([key]) => key.t === "kw" && key.v === k)?.[1];
}

// jobName is a job's command, argv a tree or, as :tasks/tree gives it,
// its strings, as a step says it: a claude by its model, and whether it
// has tools, as "claude haiku" or "claude, no tools"; any other as its
// words, "go test ./...".
function jobName(argv) {
  const a = Array.isArray(argv) ? argv : (argv?.v || []).map((t) => t.v);
  if (a[0] !== "claude") return a.join(" ");
  const m = a.indexOf("--model"), tools = a.indexOf("--tools");
  return "claude" + (m >= 0 ? " " + a[m + 1] : "") + (tools >= 0 && a[tools + 1] === "" ? ", no tools" : "");
}

// pendingOf is the task id in reply, a pending, as text; else undefined.
function pendingOf(reply) {
  return reply?.t === "tag" && reply.n === "err" ? field(reply.v[0], "id")?.v : undefined;
}

// stepsOf is the jobs the chain of the call that began task id ran, from history
// as sniff has it, in the order they began: each {from, name, at, ms,
// kind, cost}, kind "running", "ok" or "err", and cost the dollars a
// claude's JSON says, if it says. null if the chain's first message is
// gone from history. Task ids aren't message ids: a task's is in the
// pending its call was answered with.
function stepsOf(id) {
  const led = new Map(), ended = new Map();
  let head = -1;
  entries.forEach((e, i) => {
    if (e.kind === "call" && pendingOf(e.reply) === String(id)) head = i;
    if (e.cause) (led.get(e.cause) || led.set(e.cause, []).get(e.cause)).push(i);
    if (e.kind === "done" && e.to.endsWith("/whiff/done") && e.args?.v?.length === 2) ended.set(e.args.v[0].v, e);
  });
  if (head < 0) return null;
  const steps = [];
  const walk = (i) => {
    const e = entries[i];
    const job = pendingOf(e.reply);
    if (e.kind === "call" && e.to === "jobs/run" && job) {
      const done = ended.get(job), r = done?.args.v[1];
      let kind = done ? kindOf(r) : "running";
      const exit = field(r?.v?.[0], "exit");
      if (kind === "ok" && exit && exit.v !== "0") kind = "err";
      const out = r?.v?.[0]?.t === "str" ? r.v[0].v : field(r?.v?.[0], "stdout")?.v || "";
      const cost = /"total_cost_usd":\s*([0-9.]+)/.exec(out)?.[1];
      steps.push({ from: e.from, name: jobName(e.args.v[0]), at: e.at, ms: done ? Date.parse(done.at) - Date.parse(e.at) : Date.now() - Date.parse(e.at), kind, cost });
    }
    for (const j of led.get(e.id) || []) walk(j);
  };
  walk(head);
  return steps.sort((a, b) => Date.parse(a.at) - Date.parse(b.at));
}

// stepsIn is the jobs among children, as service/status gives a task's
// work, and theirs, in the order they began, as stepsOf gives them: from
// whiff's own record of what each task's work began, so they outlast
// sniff's history and a restart.
function stepsIn(children) {
  const steps = [];
  const walk = (n) => {
    if (n.cmd) {
      const began = Date.parse(n.began || n.at), result = String(n.result || "");
      let kind = n.status === "ended" ? (n.state === "done" ? "ok" : "err") : "running";
      if (kind === "ok" && /^\(ok \{:exit [1-9]/.test(result)) kind = "err";
      steps.push({ from: n.from, name: jobName(n.cmd), at: n.began || n.at, ms: (n.status === "ended" ? Date.parse(n.at) : Date.now()) - began, kind, cost: n.cost });
    }
    (n.children || []).forEach(walk);
  };
  children.forEach(walk);
  return steps.sort((a, b) => Date.parse(a.at) - Date.parse(b.at));
}

// stepList is a task's steps, as stepsOf gives them, one line each:
// who ran what, how long it took or has run, and what it cost. A chain
// that runs its first step again, as a pipeline run's dev does each
// round, goes in rounds, each headed by its number.
function stepList(steps) {
  const ol = el("ol", "work-steps");
  const key = (s) => s.from + " " + s.name;
  const starts = steps.filter((s) => key(s) === key(steps[0])).length;
  let round = 0;
  for (const s of steps) {
    if (starts > 1 && key(s) === key(steps[0])) ol.append(el("li", "work-round hint", "round " + ++round));
    const li = el("li", s.kind === "running" ? "open" : s.kind === "ok" ? "ok" : "failed");
    const how = (s.kind === "running" ? "running " : s.kind === "ok" ? "" : "failed, ") + roughly(s.ms) + (s.cost ? ", $" + parseFloat(s.cost).toFixed(2) : "");
    li.append(el("span", "work-to", ":" + s.from), el("span", "work-how", s.name), el("span", "hint", how));
    li.title = fullTime(s.at);
    ol.append(li);
  }
  return ol;
}

// workActions are what can be done with a task of an agent's work, each
// {label, action, ask}, action printed, as service/status gives them: a
// :read one, as Result or Show review and diff, is a link to its page, a
// tab; a :write one, as Merge or Retry, a button that sends it as you,
// with a box for your answer first if it asks, then shows what it
// answered and calls then.
function workActions(actions, then) {
  const box = el("span", "work-actions");
  const said = el("span", "work-said hint");
  const send = async (b, action, answer) => {
    b.disabled = true;
    const body = new URLSearchParams({ action });
    if (answer !== undefined) body.set("answer", answer);
    try {
      const r = await fetch("api/call", { method: "POST", body });
      if (!r.ok) throw new Error((await r.text()).trim());
      const reply = await r.json();
      said.textContent = print(reply);
      said.classList.toggle("bad", kindOf(reply) !== "ok");
      if (then) setTimeout(then, 1500);
    } catch (err) {
      said.textContent = err.message;
      said.classList.add("bad");
      b.disabled = false;
    }
  };
  for (const a of actions || []) {
    const m = /^\[:(\S+?)(?: (.*))?\]$/.exec(a.action);
    if (!m) continue;
    const [, target, rest = ""] = m;
    if (marks.get(target) === "read") {
      const link = el("a", "work-act", a.label);
      link.href = "/view/" + encodeURI(target) + (rest ? "?args=" + encodeURIComponent("[" + rest + "]") : "");
      link.title = "opens " + a.action + " as a tab";
      box.append(link);
    } else if (a.ask) {
      const d = el("details", "work-act");
      const form = el("form");
      const input = el("input");
      input.placeholder = a.ask;
      input.setAttribute("aria-label", a.ask);
      const go = el("button", "", "Send");
      form.append(input, go);
      form.onsubmit = (ev) => { ev.preventDefault(); send(go, a.action, input.value); };
      d.append(el("summary", "", a.label), form);
      d.title = "sends " + a.action + " as you, with your answer";
      box.append(d);
    } else {
      const b = el("button", "work-act", a.label);
      b.type = "button";
      b.title = "sends " + a.action + " as you";
      b.onclick = () => send(b, a.action);
      box.append(b);
    }
  }
  box.append(said);
  return box;
}

// workList is an agent's work, newest first: one row a task, and under
// it the jobs it led to, the steps of a pipeline run, as whiff keeps them;
// for a task from before whiff kept them, as history has them. Its id
// shows its whole chain in Activity.
function workList(work, then) {
  const ol = el("ol", "svc-work");
  for (const w of [...work].reverse()) {
    const [who, how] = workText(w);
    const bad = !w.result?.ok || ["failed", "gave-up", "cancelled"].includes(w.result?.value?.status);
    const li = el("li", w.status === "ended" ? (bad ? "failed" : "ok") : "open");
    const id = el("button", "work-id", String(w.id));
    id.type = "button";
    id.title = "show its chain in Activity";
    id.onclick = () => showOnly(w.id);
    li.append(id, el("span", "work-to", who), el("span", "work-how", how), el("span", "hint", ago(w.at)));
    if (w.actions?.length) li.append(workActions(w.actions, then));
    const steps = w.children?.length ? stepsIn(w.children) : stepsOf(w.id);
    if (steps === null) li.append(el("span", "work-gone hint", "its steps are older than sniff's history"));
    else if (steps.length) li.append(stepList(steps));
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
  box.dataset.address = name;
  fillStatus(box, name);
  return box;
}

async function fillStatus(box, name) {
  // Fills can overlap, as refreshWork's do: only the last one draws.
  const seq = (box.seq = (box.seq || 0) + 1);
  try {
    const st = await getJSON("api/service?address=" + encodeURIComponent(name));
    if (box.seq !== seq) return;
    box.classList.toggle("has-work", !!st.work?.length);
    const head = el("p", "svc-head " + statusClass(st.status));
    head.append(el("span", "dot"), el("strong", "", st.status.replace("-", " ")));
    if (st.note) head.append(" ", el("span", "", st.note));
    const dl = el("dl");
    for (const [k, v] of serviceLines(st)) dl.append(el("dt", "", k), el("dd", "", v));
    box.replaceChildren(head, dl);
    if (st.work?.length) box.append(el("h4", "", "Work"), workList(st.work, () => fillStatus(box, name)));
    // An agent pauses and resumes here, as you, and is cancelled while it
    // works: its turn and what it handed out.
    if (st.agent) {
      const act = (label, fn, title) => {
        const b = el("button", "svc-act", label);
        b.type = "button";
        b.title = title;
        b.onclick = async () => {
          b.disabled = true;
          try {
            const r = await fetch("api/call", { method: "POST", body: new URLSearchParams({ action: `[:${name}/${fn}]` }) });
            if (!r.ok) throw new Error((await r.text()).trim());
          } catch (err) {
            box.append(el("p", "query-error", err.message));
            return;
          }
          await loadServices();
          box.replaceWith(statusBox(name));
        };
        head.append(b);
      };
      const paused = st.status === "paused";
      act(paused ? "Resume" : "Pause", paused ? "resume" : "pause",
        paused ? "take what waits in its inbox, oldest first" : "take no more turns; messages wait in its inbox");
      if (st.status === "thinking" || st.status === "busy")
        act("Cancel", "cancel", "pause it, stop its turn and cancel the tasks it handed out; what comes back waits for Resume");
    }
  } catch (err) {
    if (box.seq === seq) box.replaceChildren(el("p", "hint", err.message));
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
  markAddress(name);
  const a = addresses.find((x) => x.address === name);
  const d = pane(a?.client ? "client" : isAgent(name) ? "agent" : "service", ":" + name);
  const agent = name.startsWith("agent/");
  if (!a) {
    if (services.has(name)) d.append(section("Status", statusBox(name)));
    else d.append(el("p", "hint", addressesErr || "Not loaded now."));
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
  st.textContent = s.connected ? "whiff" : s.why;
  st.title = s.connected ? "following whiff's history" : "";
  st.classList.toggle("on", s.connected);
  if (s.connected) loadAddresses();
}

$("rows").addEventListener("click", (ev) => {
  const board = ev.target.closest(".map [data-board]");
  if (board) return void openTab(board.dataset.board);
  const p = ev.target.closest("[data-project]");
  if (p) return showProject(p.dataset.project);
  const band = ev.target.closest(".map [data-band]");
  if (band) return foldBand(band.dataset.band);
  const h = ev.target.closest(".card-head");
  if (h) {
    const k = h.dataset.id;
    if (opened.delete(k)) {
      // Folding a card closes what you looked at in it.
      if (selected !== null && keyOf(entries[chainOf(selected)[0]]) === k) closeDetail();
    } else {
      opened.add(k);
    }
    redraw();
    return;
  }
  const b = ev.target.closest(".bg-head");
  if (b) {
    if (showBg) return showBackground(false);
    if (!openBg.delete(b.dataset.id)) openBg.add(b.dataset.id);
    redraw();
    return;
  }
  const v = ev.target.closest(".view-seg [data-view]");
  if (v) {
    cardView.set(v.dataset.id, v.dataset.view);
    redraw();
    return;
  }
  const node = ev.target.closest("svg [data-address]");
  if (node) return showAddress(node.dataset.address);
  const edge = ev.target.closest(".map [data-query]");
  if (edge) return setLayout("cards"), setQuery(edge.dataset.query);
  const r = ev.target.closest("[data-i]");
  if (r) showEntry(Number(r.dataset.i));
});

// The filter: a query, as whiff history takes it, that sniff matches
// history against, so only what it matches comes; kept in the URL, ?q=,
// so a filter is a link. errors, slow and writes stand for the terms
// they name, and become them.
let query = new URLSearchParams(location.search).get("q") || "";
let events = null;
const aliases = { errors: "kind:err", faults: "kind:fault", slow: "time:>500ms", writes: "mark:write", held: "gate:held" };

// follow opens history's events for the query, again each time it changes.
function follow() {
  if (events) events.close();
  events = new EventSource("events" + (query ? "?q=" + encodeURIComponent(query) : ""));
  events.addEventListener("snapshot", (ev) => {
    const snap = JSON.parse(ev.data);
    entries = [];
    if (selected !== null) closeDetail();
    // Faults are known from all of history; a filter shows only some.
    if (!query) faulted.clear();
    for (const e of snap.entries) entries.push(e);
    entries.sort((a, b) => a.us - b.us);
    entries.forEach(noteFault);
    redraw();
    $("query-error").textContent = snap.error || "";
    $("filters").classList.toggle("bad", !!snap.error);
    $("empty").textContent = snap.error ? "Fix the filter to see messages."
      : query ? "Nothing matches this filter yet."
      : "Nothing yet. Ask an agent or call a service and it shows here.";
    setStatus(snap.status);
    $("trace").scrollTop = 0;
  });
  events.addEventListener("entry", (ev) => append(JSON.parse(ev.data)));
  events.addEventListener("status", (ev) => setStatus(JSON.parse(ev.data)));
  events.onerror = () => setStatus({ connected: false, why: "sniff stopped" });
}

// setQuery filters history by q, if it changed.
function setQuery(q) {
  q = q.trim().split(/\s+/).filter(Boolean).map((w) => {
    const not = w.startsWith("-") ? "-" : "";
    return not + (aliases[w.slice(not.length).toLowerCase()] || w.slice(not.length));
  }).join(" ");
  const box = $("query");
  if (box.value.trim() !== q) box.value = q;
  if (q === query) return;
  query = q;
  const u = new URLSearchParams(location.search);
  if (q) u.set("q", q);
  else u.delete("q");
  history.replaceState(null, "", location.pathname + (u.size ? "?" + u : "") + location.hash);
  activate(null);
  follow();
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
follow();

// pillsSoon draws the status strip again shortly, once a burst of
// history that changes a team has passed.
let pillsTimer = null;
function pillsSoon() {
  clearTimeout(pillsTimer);
  pillsTimer = setTimeout(drawPills, 300);
}

// drawPills draws the status strip: only what isn't zero, each a way to
// it. The browser tab's title counts what waits for you.
function drawPills() {
  const box = $("pills");
  box.replaceChildren();
  const pill = (n, cls, text, title, href) => {
    if (!n) return;
    const p = el(href ? "a" : "button", "pill " + cls, `${n} ${text}`);
    p.title = title;
    if (href) p.href = href;
    else {
      p.type = "button";
      p.onclick = showRunning;
    }
    box.append(p);
  };
  pill(counts.failed, "failed", "failed", "tasks that failed in the last day", "/view/tasks/failed");
  pill(counts.running, "running", "running", "requests still running: show the newest", null);
  pill(counts.review, "review", "to review", "agents' writes that wait for your review", "/view/review/pending");
  pill(counts.inbox, "inbox", "in your inbox", "questions and reports sent to you", "/view/me/inbox");
  // A project with runs going, as its team says: how many, and what it
  // spent of its day, its runs' and its lead's turns'.
  for (const p of new Set(entries.map((e) => e.project).filter(Boolean))) {
    const t = teamOf(p);
    if (!t?.going) continue;
    const a = el("a", "pill project " + hueOf(p), `${p}: ${t.going} running · ${teamSpend(t)}`);
    a.href = boardOf(p);
    a.title = `${p}'s board: ${t.going} of ${t.most} runs going, and what its runs and its lead's turns cost in the last 24 hours`;
    box.append(a);
  }
  const all = counts.review + counts.inbox;
  document.title = (all ? `(${all}) ` : "") + "sniff";
}

// showRunning opens the newest running request's card, in Activity.
function showRunning() {
  activate(null);
  const head = $("rows").querySelector(".card.running .card-head");
  if (!head) return;
  if (!opened.has(head.dataset.id)) head.click();
  const again = $("rows").querySelector(`.card-head[data-id="${CSS.escape(head.dataset.id)}"]`);
  again?.focus({ preventScroll: true });
  again?.scrollIntoView({ block: "nearest" });
}

// loadSpend shows what the agents spent in the last 24 hours, of what
// they may spend a day, from each one's status.
async function loadSpend() {
  const agents = [...services.values()].filter((s) => s.agent);
  const box = $("spend");
  if (!agents.length) return void (box.hidden = true);
  let sts;
  try {
    sts = await Promise.all(agents.map((a) => getJSON("api/service?address=" + encodeURIComponent(a.service))));
  } catch {
    return;
  }
  const usd = (x) => "$" + (Number.isInteger(x) ? x : x.toFixed(2));
  const sum = (k) => sts.reduce((n, st) => n + (parseFloat(st[k]) || 0), 0);
  box.textContent = `${usd(sum("spent"))} of ${usd(sum("day"))} today`;
  box.title = "what agents spent in the last 24 hours, of their budgets a day: " +
    sts.map((st, n) => `:${agents[n].service} ${usd(parseFloat(st.spent) || 0)}`).join(", ");
  box.hidden = false;
}

// followQueue keeps how many items target's view has beside the sidebar's
// link of class name, and in the status strip; unless the link is
// data-quiet, as Failed is, whose tasks stay a day whatever you do, it
// marks the link too. It fetches it, and calls then if given, each time
// one of topics fires. Following :review/pending so, while sniff is open,
// it reviews: agents' writes wait for you rather than failing with
// :no-reviewer. A watch that sniff refuses, while whiff isn't running, is
// tried again.
function followQueue(name, target, topics, then) {
  const link = document.querySelector(".side ." + name);
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
    link.querySelector(".count")?.remove();
    if (n) link.append(el("span", "count", String(n)));
    link.classList.toggle("waiting", n > 0 && !("quiet" in link.dataset));
    counts[name] = n;
    drawPills();
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
followQueue("failed", "tasks/failed", ["tasks/changed"]);
$("side-activity").onclick = () => setLayout("cards");
$("side-map").onclick = () => setLayout("map");
$("activity-title").textContent = layout === "map" ? "Map" : "Activity";
$("activity").classList.toggle("dock-bottom", layout === "map");
$("show-bg").setAttribute("aria-pressed", String(showBg));
$("show-bg").textContent = showBg ? "Hide background" : "Show background";

// Tabs: Activity, then a view per tab, each its /view page's href, drawn
// in this page as that page draws it. Only the tab shown follows its
// topics, so a browser's few connections to sniff go further; one shown
// again fetches its view then. The tab bar shows once a view is open.
// Open tabs and pinned views are kept in this browser.
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
  $("tabs").hidden = !tabs.length;
  store.set("active", t ? t.href : "");
  markSide();
  showBanner();
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
  const end = atEnd(t.box);
  t.box.innerHTML = html; // sniff drew it, escaping what it shows
  end();
  const said = t.box.querySelector(":scope > .said");
  if (said) t.said.replaceChildren(said); // it stays as the view changes
  const topics = (t.box.querySelector(".shown")?.dataset.topics || "").split(" ").filter(Boolean);
  setTabStatus(t, "fetched again on " + topics.map((x) => ":" + x).join(", "), true);
  if (active === t && topics.join(" ") !== t.topics.join(" ")) {
    t.topics = topics;
    watch(t);
  }
}

// atEnd is, for el about to be drawn again, what keeps it at its end if
// you were reading there, as a turn you watch grows: a function to call
// once it is drawn.
function atEnd(el) {
  let s = el.parentElement;
  while (s && !(s.scrollHeight > s.clientHeight && /auto|scroll/.test(getComputedStyle(s).overflowY))) s = s.parentElement;
  s = s || document.scrollingElement;
  const was = s.scrollHeight > s.clientHeight && s.scrollHeight - s.scrollTop - s.clientHeight < 40;
  return () => { if (was) s.scrollTop = s.scrollHeight; };
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
  for (const t of tabs) drawPin(t);
}

function drawPin(t) {
  const on = pins.includes(t.href);
  t.pin.textContent = on ? "Unpin" : "Pin";
  t.pin.title = on ? "take it out of ⌘K's views" : "keep it among ⌘K's views";
}

// markSide marks the sidebar's link to the tab shown, or Activity.
function markSide() {
  for (const a of document.querySelectorAll(".side a[href]")) {
    if (active && hrefOf(a.getAttribute("href")) === active.href) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
  for (const [id, v] of [["side-activity", "cards"], ["side-map", "map"]]) {
    if (!active && layout === v) $(id).setAttribute("aria-current", "page");
    else $(id).removeAttribute("aria-current");
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

// Keys over Activity: j and k go to the next and previous card, step or
// background row, Enter opens it (a card or background row folds or
// unfolds, a step answered with a view opens it as a tab), c opens Call
// again for the step shown, and Esc there leaves it uncalled; Esc
// elsewhere closes the details. Never while you type, or over a tab.
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
  if (ev.key === "Escape" && !agentPop.hidden) {
    ev.preventDefault();
    closeAgentPop();
  } else if (ev.key === "Escape" && !$("detail").hidden) {
    ev.preventDefault();
    closeDetail();
  } else if (ev.key === "j" || ev.key === "k") {
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

// step goes d cards, steps or rows on from where you are, the heading
// focused or the message selected; from neither, to the first or last.
function step(d) {
  const all = [...$("rows").querySelectorAll(".card-head, .bg-head, [data-i]")];
  let at = all.indexOf(document.activeElement);
  if (at < 0 && selected !== null) at = all.indexOf($("rows").querySelector(`[data-i="${selected}"]`));
  const next = all[at < 0 ? (d > 0 ? 0 : all.length - 1) : Math.max(0, Math.min(all.length - 1, at + d))];
  if (!next) return;
  if ("i" in next.dataset) {
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
    // Agents first, as what you'd most go to, then views, services and,
    // once you type, functions. A service whose file failed to load, so
    // has no address, is there too, with why.
    const known = [...addresses, ...[...services.values()].filter((x) => !addresses.some((a) => a.address === x.service)).map((x) => ({ address: x.service, fns: [] }))];
    // todo/ matches :todo too, as it begins its functions.
    const addrs = first(known, (a) => a.address + "/");
    const line = (a) => {
      const st = services.get(a.address);
      const fault = faulted.has(a.address) && !st;
      const quiet = !st || st.status === "ok" || st.status === "idle";
      const cls = fault ? "fault" : st ? statusClass(st.status) : "";
      const what = el("span", "pick-line addr" + (a.client ? " client" : "") + (cls ? " " + cls : quiet ? " quiet" : ""));
      what.append(el("span", "dot"), el("code", "", ":" + a.address), el("span", "doc",
        [fault ? "fault" : !quiet && st.status.replace("-", " "), st?.note, a.fns.length && plural(a.fns.length, "function"), a.state && "state", a.client && "client"].filter(Boolean).join(" · ")));
      return pick([what], "", () => go(() => { activate(null); showAddress(a.address); }), true);
    };
    groups.push(["Agents", addrs.filter((a) => isAgent(a.address)).map(line)]);
    const viewLine = (href) => {
      const what = el("span", "pick-line");
      what.append(el("code", "", titleOf(href)));
      if (pins.includes(href)) what.append(el("span", "doc", "pinned"));
      return what;
    };
    const views = [...new Set(["/view/review/pending", "/view/me/inbox", "/view/tasks/view", "/view/tasks/failed", "/view/timers/view", ...pins, ...tabs.map((t) => t.href)])];
    groups.push(["Views", [
      ...(hits("activity") ? [pick([el("span", "", "Activity")], "", () => go(() => activate(null)), true)] : []),
      ...first(views, titleOf).map((href) => pick([viewLine(href)], "opens a tab", () => go(() => openTab(href)), true)),
    ]]);
    groups.push(["Services", addrs.filter((a) => !isAgent(a.address)).map(line)]);
    const fns = words.length ? first(addresses.flatMap((a) => a.fns.map((f) => ({ a, f, name: a.address + "/" + f.fn }))), (x) => x.name) : [];
    groups.push(["Functions", fns.map(({ f, name }) => {
      const tab = f.mark === "read" && !f.params.length && f.form !== "ask";
      return pick([el("code", "", name), el("span", "badge " + f.mark, f.mark), el("span", "doc", f.doc)],
        tab ? "opens a tab" : "↵ fill in args",
        tab ? () => go(() => openTab(pageOf(name, { t: "vec", v: [] }))) : () => fillIn(name, f.params));
    })]);
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
for (const k of document.querySelectorAll(".mod-k")) k.textContent = modK;
// ⌘K, or Ctrl K, opens it from anywhere, and closes it again.
document.addEventListener("keydown", (ev) => {
  if (!(ev.metaKey || ev.ctrlKey) || ev.altKey || ev.shiftKey || ev.key?.toLowerCase() !== "k") return;
  ev.preventDefault();
  if (palette.open) palette.close();
  else openPalette();
});

for (const href of store.get("tabs", [])) openTab(href, false);
activate(tabs.find((t) => t.href === store.get("active", "")) || null);
