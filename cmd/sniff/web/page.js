// A view pinned as a page of its own, /view/address/fn: what the function
// answers, called as you with no arguments, fetched again each time its
// topic fires: ?on=topic, or address/changed by default.
"use strict";

const target = decodeURIComponent(location.pathname.slice("/view/".length));
const topic = new URLSearchParams(location.search).get("on") || target.slice(0, target.lastIndexOf("/")) + "/changed";
document.title = ":" + target + " · sniff";
$("target").textContent = ":" + target;

function setStatus(text, on) {
  const st = $("status");
  st.textContent = text;
  st.classList.toggle("on", on);
}

async function load() {
  try {
    const reply = await getJSON("/api/view?target=" + encodeURIComponent(target));
    const v = viewOf(reply);
    $("view").replaceChildren(v ? showView(v, reply, load) : wrap(value(reply)));
    setStatus("fetched again on :" + topic, true);
  } catch (err) {
    setStatus(err.message, false);
  }
}

// follow watches the topic, fetching the view each time the watch opens,
// so what changed while it was closed shows too. A watch that sniff
// refuses, while whiff isn't running, isn't tried again by the browser:
// follow tries it again itself.
function follow() {
  const watch = new EventSource("/api/watch?topic=" + encodeURIComponent(topic));
  watch.addEventListener("open", load);
  watch.addEventListener("changed", load);
  watch.onerror = () => {
    setStatus("not following :" + topic + ": is whiff running?", false);
    if (watch.readyState === EventSource.CLOSED) setTimeout(follow, 2000);
  };
}

follow();
