// A view pinned as a page of its own, /view/address/fn?args=[...]: what the
// function answers, called as you with args (none by default), fetched
// again each time its topic fires: ?on=topic, or the one the view names,
// {:changed topic}, or address/changed.
"use strict";

const params = new URLSearchParams(location.search);
const target = decodeURIComponent(location.pathname.slice("/view/".length));
const args = params.get("args") || "[]";
const shown = ":" + target + (args === "[]" ? "" : " " + args.slice(1, -1));
document.title = shown + " · sniff";
$("target").textContent = shown;

let topic = ""; // the topic followed
let watch = null; // the watch on it, if one is open or opening

function setStatus(text, on) {
  const st = $("status");
  st.textContent = text;
  st.classList.toggle("on", on);
}

async function load() {
  try {
    await loadMarks().catch(() => {});
    const reply = await getJSON("/api/view?target=" + encodeURIComponent(target) + "&args=" + encodeURIComponent(args));
    const v = viewOf(reply);
    $("view").replaceChildren(v ? showView(v, reply, load) : wrap(value(reply)));
    follow(params.get("on") || (v && changedOf(v)) || target.slice(0, target.lastIndexOf("/")) + "/changed");
    setStatus("fetched again on :" + topic, true);
  } catch (err) {
    setStatus(err.message, false);
    if (!watch) setTimeout(load, 2000);
  }
}

// follow watches t, fetching the view each time it fires, and each time the
// watch opens, so what changed while it was closed shows too. A watch that
// sniff refuses, while whiff isn't running, isn't tried again by the
// browser: load tries again itself.
function follow(t) {
  if (t === topic && watch) return;
  if (watch) watch.close();
  topic = t;
  const w = new EventSource("/api/watch?topic=" + encodeURIComponent(t));
  watch = w;
  w.addEventListener("open", load);
  w.addEventListener("changed", load);
  w.onerror = () => {
    if (watch !== w) return;
    setStatus("not following :" + t + ": is whiff running?", false);
    if (w.readyState === EventSource.CLOSED) {
      watch = null;
      setTimeout(load, 2000);
    }
  };
}

load();
