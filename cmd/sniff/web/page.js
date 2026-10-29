// A view pinned as a page of its own, /view/address/fn?args=[...], which
// sniff draws: this follows the topics it names, data-topics, and swaps in
// the view drawn again each time one fires, and each time the watch opens,
// so what changed while it was closed shows too. It waits while you type
// an answer, so a change doesn't wipe it.
"use strict";

const view = document.getElementById("view");
const status = document.getElementById("status");
const topics = (view.dataset.topics || "").split(" ").filter(Boolean);
const named = topics.map((t) => ":" + t).join(", ");

// What the action sent from here answered shows once: a reload doesn't
// show it again.
const here = new URL(location.href);
if (here.searchParams.has("said")) {
  here.searchParams.delete("said");
  history.replaceState(null, "", here);
}

function setStatus(text, on) {
  status.textContent = text;
  status.classList.toggle("on", on);
}

let stale = false; // a change came while you typed

async function load() {
  if (view.contains(document.activeElement) && document.activeElement.tagName === "INPUT") {
    stale = true;
    return;
  }
  stale = false;
  const part = new URL(here);
  part.searchParams.set("part", "view");
  try {
    const r = await fetch(part);
    const html = await r.text();
    if (!r.ok) throw new Error(html.trim());
    // sniff drew it, escaping what it shows.
    view.innerHTML = html;
    setStatus("fetched again on " + named, true);
  } catch (err) {
    setStatus(err.message, false);
  }
}

view.addEventListener("focusout", () => setTimeout(() => { if (stale) load(); }, 0));

// A location copies itself, file:line:col, for hiss or a terminal: a page
// can't open a file.
view.addEventListener("click", (ev) => {
  const b = ev.target.closest("[data-copy]");
  if (!b) return;
  navigator.clipboard.writeText(b.dataset.copy).then(() => {
    b.classList.add("copied");
    setTimeout(() => b.classList.remove("copied"), 1200);
  }, (err) => { b.title = err.message; });
});

// follow watches the topics. A watch that sniff refuses, while whiff isn't
// running, isn't tried again by the browser: this tries again itself.
function follow() {
  const w = new EventSource("/api/watch?" + topics.map((t) => "topic=" + encodeURIComponent(t)).join("&"));
  w.addEventListener("open", load);
  w.addEventListener("changed", load);
  w.onerror = () => {
    setStatus("not following " + named + ": is whiff running?", false);
    if (w.readyState === EventSource.CLOSED) setTimeout(follow, 2000);
  };
}

// A page sniff couldn't draw, as whiff wasn't running, tries again.
if ("error" in view.dataset) setTimeout(() => location.reload(), 2000);
else if (topics.length) follow();
