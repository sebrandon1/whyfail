// The board is a client over two files the daily Action publishes:
// snapshot.json (today) and history.json (one point per job per day). It has no
// build step and no dependencies, and it never invents a number: a missing
// history file just means no trend line today.

const $ = (id) => document.getElementById(id);

const state = { snapshot: null, history: null, filter: "" };

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") node.className = v;
    else if (k === "href") node.href = v;
    else node.setAttribute(k, v);
  }
  for (const c of children) {
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

function pct(rate) {
  if (typeof rate !== "number" || !isFinite(rate)) return "—";
  return (rate * 100).toFixed(1) + "%";
}

function verdictClass(v) {
  return "v-" + String(v || "unknown").toLowerCase();
}

// A sparkline of pass rate over the retained days. Points are drawn even when
// only two exist; a single point is a flat line rather than a guess.
function spark(job) {
  const points = (state.history?.points || [])
    .filter((p) => p.jobs && p.jobs[job])
    .map((p) => p.jobs[job].pass_rate);
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("class", "spark");
  svg.setAttribute("viewBox", "0 0 80 20");
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", points.length ? `pass rate trend, ${points.length} days` : "no trend yet");
  if (points.length === 0) return svg;

  const w = 80, h = 20, pad = 2;
  const x = (i) => (points.length === 1 ? w / 2 : pad + (i * (w - 2 * pad)) / (points.length - 1));
  const y = (v) => h - pad - Math.min(Math.max(v, 0), 1) * (h - 2 * pad);
  const line = points.map((v, i) => `${x(i).toFixed(1)},${y(v).toFixed(1)}`).join(" ");

  const ns = "http://www.w3.org/2000/svg";
  if (points.length === 1) {
    // One day is a point, not a line: drawing a polyline would paint nothing.
    const dot = document.createElementNS(ns, "circle");
    dot.setAttribute("cx", x(0).toFixed(1));
    dot.setAttribute("cy", y(points[0]).toFixed(1));
    dot.setAttribute("r", "2");
    dot.setAttribute("fill", "currentColor");
    svg.append(dot);
    return svg;
  }
  const poly = document.createElementNS(ns, "polyline");
  poly.setAttribute("points", line);
  svg.append(poly);
  return svg;
}

function chips(jobs) {
  const counts = { BROKEN: 0, FLAKY: 0, DEGRADED: 0, HEALTHY: 0, UNKNOWN: 0 };
  for (const j of jobs) counts[j.verdict] = (counts[j.verdict] || 0) + 1;
  const host = $("chips");
  const nodes = Object.entries(counts).map(([v, n]) =>
    el("span", { class: "chip" }, el("b", {}, n), " ", v.toLowerCase())
  );
  // A regression is the one thing a daily refresh can show that a single
  // snapshot cannot, so it gets its own chip above the verdict counts.
  const broken = jobs.filter((j) => j.regressed).length;
  if (broken > 0) {
    nodes.unshift(el("span", { class: "chip newly-broken" }, el("b", {}, broken), " newly broken"));
  }
  host.replaceChildren(...nodes);
}

function row(j) {
  const tr = el("tr", { class: j.error ? "err" : String(j.verdict).toLowerCase() });
  const verdict = el(
    "td",
    {},
    el("span", { class: "verdict " + verdictClass(j.verdict) }, j.error ? "ERROR" : j.verdict)
  );
  if (j.regressed) {
    verdict.append(
      el(
        "span",
        { class: "regressed", title: "previous verdict: " + j.previous_verdict },
        "was " + String(j.previous_verdict).toLowerCase()
      )
    );
  }
  tr.append(
    el("td", { class: "job" }, el("a", { href: j.url, target: "_blank", rel: "noreferrer" }, j.job)),
    el("td", { class: "repo" }, j.repo || "—"),
    verdict,
    el("td", { class: "num" }, pct(j.pass_rate)),
    el("td", { class: "num" }, pct(j.recent_pass_rate)),
    el("td", { class: "num runs" }, j.runs),
    el("td", { class: "num streak" }, j.streak),
    el("td", { class: "num prs" }, j.distinct_prs_failing),
    el("td", {}, spark(j.job))
  );
  if (j.error) tr.title = j.error;
  return tr;
}

function render() {
  const snap = state.snapshot;
  if (!snap) return;

  const at = new Date(snap.generated_at);
  $("meta").textContent =
    `Snapshot ${isNaN(at) ? snap.generated_at : at.toLocaleString()} · ` +
    `window ${snap.window || "all retained"} · ${snap.bucket}`;

  chips(snap.jobs || []);

  const needle = state.filter.trim().toLowerCase();
  const shown = (snap.jobs || []).filter(
    (j) => !needle || j.job.toLowerCase().includes(needle) || (j.repo || "").toLowerCase().includes(needle)
  );
  $("rows").replaceChildren(...shown.map(row));
  $("empty").hidden = shown.length > 0;
}

async function getJSON(path) {
  const res = await fetch(path, { cache: "no-store" });
  if (!res.ok) throw new Error(`${path}: ${res.status}`);
  return res.json();
}

async function main() {
  $("filter").addEventListener("input", (e) => {
    state.filter = e.target.value;
    render();
  });

  try {
    state.snapshot = await getJSON("snapshot.json");
  } catch (err) {
    $("meta").textContent = `Could not load the snapshot: ${err.message}`;
    return;
  }
  // History is optional: the first day has no trend to draw.
  try {
    state.history = await getJSON("history.json");
  } catch (_) {
    state.history = null;
  }
  render();
}

main();
