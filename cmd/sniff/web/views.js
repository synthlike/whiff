// What sniff's pages share: puff values as trees, folded, and views drawn
// as what they are, their actions sent as you.
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

// Printing trees, as puff prints values.
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

// Values, folded: short ones on a line, longer ones as a tree.
function value(t, depth = 0) {
  const flat = print(t);
  if (flat.length <= 64 || !["map", "vec", "tag"].includes(t.t)) return inline(t);
  const d = el("details");
  d.open = depth < 2;
  const open = t.t === "map" ? "{" : t.t === "vec" ? "[" : "(" + t.n;
  const close = t.t === "map" ? "}" : t.t === "vec" ? "]" : ")";
  const s = el("summary");
  s.append(t.t === "tag" ? el("span", "tag", open) : open, el("span", "count", ` ${t.v.length}`));
  const kids = el("div", "kids");
  for (const kid of t.v) {
    const line = el("div");
    if (t.t === "map") {
      line.append(inline(kid[0]), " ", value(kid[1], depth + 1));
    } else {
      line.append(value(kid, depth + 1));
    }
    kids.append(line);
  }
  d.append(s, kids, close);
  return d;
}

function inline(t) {
  const s = el("span");
  switch (t.t) {
    case "kw": s.className = "kw"; s.textContent = ":" + t.v; break;
    case "str": s.className = "str"; s.textContent = JSON.stringify(t.v); break;
    case "lit": s.className = "lit"; s.textContent = t.v; break;
    case "tag":
      s.append("(", el("span", "tag", t.n));
      for (const a of t.v) s.append(" ", inline(a));
      s.append(")");
      break;
    case "vec":
      s.append("[");
      t.v.forEach((a, i) => s.append(i ? " " : "", inline(a)));
      s.append("]");
      break;
    case "map":
      s.append("{");
      t.v.forEach(([k, v], i) => s.append(i ? " " : "", inline(k), " ", inline(v)));
      s.append("}");
      break;
  }
  return s;
}

function wrap(v) {
  const w = el("div", "value");
  w.append(v);
  return w;
}

// Views, as a service answers with them, (ok view): (items [...]), (text
// "..."), (locations [...]) or (board [...]), each with its opts after it
// if it has any. viewOf is the view in reply, or null if it holds none, or
// one of a shape sniff doesn't know.
function viewOf(reply) {
  if (reply.t !== "tag" || reply.n !== "ok" || reply.v.length !== 1) return null;
  const v = reply.v[0];
  if (v.t !== "tag" || v.v.length < 1 || v.v.length > 2) return null;
  const arg = v.v[0];
  // maps says whether t is a vector of maps, each with a string at need.
  const maps = (t, need) => t?.t === "vec" && t.v.every((m) => m.t === "map" && field(m, need)?.t === "str");
  switch (v.n) {
    case "text": return arg.t === "str" ? v : null;
    case "items": return maps(arg, "label") ? v : null;
    case "locations": return maps(arg, "file") ? v : null;
    case "board": return maps(arg, "title") && arg.v.every((c) => maps(field(c, "items"), "label")) ? v : null;
  }
  return null;
}

// changedOf is the topics view names in its opts, {:changed :plan/changed}
// or a vector of them, published when it changes; or none.
function changedOf(view) {
  const opts = view.v[1];
  const t = opts && opts.t === "map" && field(opts, "changed");
  if (t && t.t === "kw") return [t.v];
  if (t && t.t === "vec") return t.v.filter((k) => k.t === "kw").map((k) => k.v);
  return [];
}

// marks is each function's mark, "read" or "write", by its target,
// "plan/board", from sniff's addresses: whether an action to it is a link
// or a button.
let marks = new Map();

function setMarks(addresses) {
  marks = new Map();
  for (const a of addresses) for (const f of a.fns) marks.set(a.address + "/" + f.fn, f.mark);
}

async function loadMarks() {
  setMarks(await getJSON("/api/addresses"));
}

// pageOf is the page that shows what target answers to args, a tree of a
// vector, as a view: /view/pipeline/show?args=[2].
function pageOf(target, args) {
  return "/view/" + encodeURI(target) + (args.v.length ? "?args=" + encodeURIComponent(print(args)) : "");
}

// field is map m's value at the keyword name, or undefined.
function field(m, name) {
  const kv = m.v.find(([k]) => k.t === "kw" && k.v === name);
  return kv && kv[1];
}

// str is map m's string at name, or "".
function str(m, name) {
  const v = field(m, name);
  return v && v.t === "str" ? v.v : "";
}

// showView draws view as what it is, with the reply it came in folded under
// it as puff. acted, if given, is called once an item's action is answered.
function showView(view, reply, acted) {
  const box = el("div", "view");
  const arg = view.v[0];
  const itemOf = (m) => item(m, acted);
  if (view.n === "text") {
    box.append(el("pre", "", arg.v));
  } else if (view.n === "board") {
    box.classList.add("board");
    for (const c of arg.v) {
      const col = el("div", "column");
      const items = field(c, "items").v;
      col.append(el("h4", "", str(c, "title")), ...items.map(itemOf));
      if (!items.length) col.append(el("p", "hint", "None."));
      box.append(col);
    }
  } else {
    for (const m of arg.v) box.append(view.n === "items" ? itemOf(m) : place(m));
    if (!arg.v.length) box.append(el("p", "hint", "None."));
  }
  const raw = el("details", "raw");
  raw.append(el("summary", "", "as puff"), wrap(value(reply)));
  const f = document.createDocumentFragment();
  f.append(box, raw);
  return f;
}

// item is an item of a view, with what can be done with it: its :actions,
// each saying what it does, and its :action, which shows the message it
// sends. One to a :read function is a link, which opens its answer as a
// page; any other is a button, which sends it as you, as whiff act does,
// and shows the reply under the item. A :write button shows its target too,
// as its label can say anything.
function item(m, acted) {
  const r = el("div", "item");
  r.append(el("span", "", str(m, "label")));
  if (str(m, "detail")) r.append(el("span", "detail", str(m, "detail")));
  const actions = [];
  if (field(m, "action")) actions.push({ label: "", action: field(m, "action") });
  for (const a of field(m, "actions")?.v || []) {
    if (a.t === "map" && field(a, "action")) actions.push({ label: str(a, "label"), action: field(a, "action") });
  }
  if (!actions.length) return r;
  const buttons = el("span", "actions");
  const out = el("div", "reply");
  for (const { label, action } of actions) {
    const target = action.v[0].v;
    const mark = marks.get(target);
    if (mark === "read") {
      const a = el("a", label ? "action link labelled" : "action link", label || print(action));
      a.href = pageOf(target, { t: "vec", v: action.v.slice(1) });
      a.target = "_blank";
      a.rel = "noopener";
      a.title = "opens what " + print(action) + " answers";
      buttons.append(a);
      continue;
    }
    const b = el("button", label ? "action labelled" : "action", label || print(action));
    if (label && mark === "write") b.append(el("span", "to", " → :" + target));
    b.title = "sends " + print(action) + " as you";
    b.onclick = async () => {
      buttons.querySelectorAll("button").forEach((x) => { x.disabled = true; });
      out.className = "reply";
      out.replaceChildren("…");
      try {
        const reply = await act(action);
        out.className = "reply " + (reply.t === "tag" && reply.n === "ok" ? "ok" : "err");
        out.replaceChildren(inline(reply));
        if (acted) acted();
      } catch (err) {
        out.className = "reply err";
        out.textContent = err.message;
      }
      buttons.querySelectorAll("button").forEach((x) => { x.disabled = false; });
    };
    buttons.append(b);
  }
  r.append(buttons, out);
  return r;
}

// act sends action, a message as [:address/fn args...], as you; its reply
// as a tree.
async function act(action) {
  const r = await fetch("/api/act", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ action: print(action) }),
  });
  if (!r.ok) throw new Error((await r.text()).trim());
  return r.json();
}

// place is a location of a view as file:line:col, as compilers print it;
// clicking copies that, for hiss or a terminal, since a page can't open a
// file.
function place(m) {
  let at = str(m, "file");
  for (const key of ["line", "col"]) {
    const n = field(m, key);
    if (n && n.t === "lit") at += ":" + n.v;
  }
  const b = el("button", "place");
  b.append(el("span", "at", at));
  if (str(m, "note")) b.append(el("span", "detail", str(m, "note")));
  b.title = "copy " + at;
  b.onclick = () => navigator.clipboard.writeText(at).then(() => {
    b.classList.add("copied");
    setTimeout(() => b.classList.remove("copied"), 1200);
  }, (err) => { b.title = err.message; });
  return b;
}
