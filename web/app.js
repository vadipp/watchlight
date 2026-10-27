"use strict";
const privateView = location.pathname === "/dashboard";
let selected = null,
  views = [];
const el = (tag, text, cls) => {
  const n = document.createElement(tag);
  if (text !== undefined) n.textContent = text;
  if (cls) n.className = cls;
  return n;
};
const fmt = (t) => new Date(t * 1000).toLocaleString();
function render() {
  const list = document.querySelector("#services");
  list.replaceChildren();
  if (!views.some((v) => v.id === selected)) selected = views[0]?.id;
  for (const v of views) {
    const b = el(
      "button",
      undefined,
      "service" + (v.id === selected ? " selected" : ""),
    );
    b.append(
      el("span", undefined, "dot " + v.state.status),
      el("span", v.name),
      el("small", v.state.status),
    );
    b.onclick = () => {
      selected = v.id;
      render();
    };
    list.append(b);
  }
  const sum = document.querySelector("#summary");
  const down = views.filter((v) => v.state.status === "down").length,
    unknown = views.filter((v) => v.state.status === "unknown").length;
  sum.className =
    "summary " + (down ? "down" : unknown || !views.length ? "unknown" : "up");
  sum.textContent = !views.length
    ? "No services published"
    : down
      ? `${down} service${down === 1 ? "" : "s"} experiencing problems`
      : unknown
        ? `${unknown} service${unknown === 1 ? "" : "s"} awaiting fresh checks`
        : "All services operational";
  const detail = document.querySelector("#detail");
  detail.replaceChildren();
  const v = views.find((v) => v.id === selected);
  if (!v) {
    detail.append(el("p", "No services to display."));
    return;
  }
  detail.append(el("h2", v.name));
  const metrics = el("div", undefined, "metrics");
  for (const [label, value] of [
    [
      "Observed uptime · 24h",
      v.uptime === null ? "—" : v.uptime.toFixed(2) + "%",
    ],
    [
      "Response time",
      v.type === "heartbeat"
        ? "—"
        : v.state.checked
          ? v.state.latency.toFixed(0) + " ms"
          : "—",
    ],
    ["Current status", v.state.status],
  ]) {
    const m = el("div", undefined, "metric");
    m.append(el("b", value), el("span", label));
    metrics.append(m);
  }
  detail.append(metrics);
  const h = [...v.history].reverse();
  const section = el("div", undefined, "section");
  section.append(el("div", "Recent checks · latest 240 observations", "label"));
  const bars = el("div", undefined, "bars");
  let prev = null;
  for (const p of h) {
    if (prev && p.time - prev.time > v.interval_seconds * 1.5) {
      const gap = el("span", undefined, "bar unknown");
      gap.title = "Monitoring gap";
      bars.append(gap);
    }
    const bar = el("span", undefined, "bar " + (p.ok ? "up" : "down"));
    bar.title = `${fmt(p.time)} · ${p.ok ? "success" : "failure"} · ${p.latency.toFixed(1)} ms`;
    bars.append(bar);
    prev = p;
  }
  section.append(bars);
  const times = el("div", undefined, "times");
  times.append(
    el("span", h.length ? fmt(h[0].time) : "No observations"),
    el("span", h.length ? fmt(h[h.length - 1].time) : ""),
  );
  section.append(times);
  if (h.length > 1 && v.type !== "heartbeat") {
    const ns = "http://www.w3.org/2000/svg",
      svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 600 125");
    svg.classList.add("chart");
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", "Response time chart for recent checks");
    const line = document.createElementNS(ns, "polyline"),
      max = Math.max(1, ...h.map((p) => p.latency));
    line.setAttribute(
      "points",
      h
        .map(
          (p, i) =>
            `${(i / (h.length - 1)) * 600},${120 - (p.latency / max) * 110}`,
        )
        .join(" "),
    );
    svg.append(line);
    section.append(svg);
  }
  detail.append(section);
  if (privateView && v.state.error)
    detail.append(el("p", v.state.error, "error"));
  const incidents = el("div", undefined, "section");
  incidents.append(el("h2", "Status changes"));
  if (!v.incidents.length)
    incidents.append(el("p", "No recorded status changes.", "label"));
  for (const i of v.incidents) {
    const row = el("div", undefined, "incident");
    row.append(el("span", i.status), el("time", fmt(i.time)));
    incidents.append(row);
  }
  detail.append(
    incidents,
    el(
      "p",
      `Last check: ${v.state.checked ? fmt(v.state.checked) : "never"} · ${v.samples} observed checks in 24 hours`,
      "label",
    ),
  );
}
async function refresh() {
  try {
    const r = await fetch(privateView ? "/api/private" : "/api/public", {
      cache: "no-store",
    });
    if (!r.ok) throw Error();
    views = await r.json();
    if (privateView) {
      const healthResponse = await fetch("/api/health", { cache: "no-store" });
      const health = await healthResponse.json();
      const panel = document.querySelector("#delivery");
      panel.hidden = false;
      const telegram = health.telegram
        ? health.telegram.ok
          ? "API reachable"
          : "API probe failing or pending"
        : "not configured";
      panel.textContent =
        "Monitor engine: " +
        (health.ok ? "checks fresh" : "checks stale") +
        " · Telegram: " +
        telegram +
        " · Queued alerts: " +
        health.pending_notifications;
    }
    document.querySelector("#updated").textContent =
      "Updated " + new Date().toLocaleTimeString();
    render();
  } catch {
    document.querySelector("#summary").textContent =
      "Unable to refresh monitoring data";
    document.querySelector("#summary").className = "summary unknown";
    document.querySelector("#updated").textContent =
      "Connection lost · displayed details may be stale";
  }
}
if (privateView) document.querySelector("h1").textContent = "Owner dashboard";
refresh();
setInterval(refresh, 15000);
