// A view pinned as a page of its own, /view/address/fn?args=[...]: what the
// function answers, called as you with args (none by default), fetched
// again each time one of its topics fires: ?on=topic, as often as needed,
// or the ones the view names, {:changed topic}, or address/changed.
"use strict";

const params = new URLSearchParams(location.search);
const target = decodeURIComponent(location.pathname.slice("/view/".length));
const args = params.get("args") || "[]";
const shown = ":" + target + (args === "[]" ? "" : " " + args.slice(1, -1));
document.title = shown + " · sniff";
$("target").textContent = shown;

let topics = []; // the topics followed
let watch = null; // the watch on them, if one is open or opening

function setStatus(text, on) {
  const st = $("status");
  st.textContent = text;
  st.classList.toggle("on", on);
}

const named = (ts) => ts.map((t) => ":" + t).join(", ");

async function load() {
  try {
    await loadMarks().catch(() => {});
    const reply = await getJSON("/api/view?target=" + encodeURIComponent(target) + "&args=" + encodeURIComponent(args));
    const v = viewOf(reply);
    $("view").replaceChildren(v ? showView(v, reply, load) : wrap(value(reply)));
    const on = params.getAll("on");
    const changed = v ? changedOf(v) : [];
    follow(on.length ? on : changed.length ? changed : [target.slice(0, target.lastIndexOf("/")) + "/changed"]);
    setStatus("fetched again on " + named(topics), true);
  } catch (err) {
    setStatus(err.message, false);
    if (!watch) setTimeout(load, 2000);
  }
}

// follow watches ts, fetching the view each time one fires, and each time
// the watch opens, so what changed while it was closed shows too. A watch
// that sniff refuses, while whiff isn't running, isn't tried again by the
// browser: load tries again itself.
function follow(ts) {
  if (ts.join(" ") === topics.join(" ") && watch) return;
  if (watch) watch.close();
  topics = ts;
  const w = new EventSource("/api/watch?" + ts.map((t) => "topic=" + encodeURIComponent(t)).join("&"));
  watch = w;
  w.addEventListener("open", load);
  w.addEventListener("changed", load);
  w.onerror = () => {
    if (watch !== w) return;
    setStatus("not following " + named(ts) + ": is whiff running?", false);
    if (w.readyState === EventSource.CLOSED) {
      watch = null;
      setTimeout(load, 2000);
    }
  };
}

load();
