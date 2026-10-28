"use strict";
const privateView = location.pathname === "/dashboard";
const statusSlug = location.pathname.startsWith("/status/") ? location.pathname.slice(8) : null;
let selected = null,
  views = [];
const expandedGroups = new Map();
const el = (tag, text, cls) => {
  const n = document.createElement(tag);
  if (text !== undefined) n.textContent = text;
  if (cls) n.className = cls;
  return n;
};
const fmt = (t) => new Date(t * 1000).toLocaleString();
const historyObserver = new ResizeObserver((entries) => {
  for (const { target, contentRect } of entries)
    target.drawBars(contentRect.width);
});
function render() {
  historyObserver.disconnect();
  const list = document.querySelector("#services");
  list.replaceChildren();
  if (!views.some((v) => v.id === selected)) selected = views[0]?.id;
  const groups = new Map();
  if (privateView) {
    for (const v of views) {
      const name = v.group || "Other";
      if (!groups.has(name)) {
        const group = el("details", undefined, "service-group");
        group.open = expandedGroups.get(name) ?? views.find((v) => v.id === selected)?.group === v.group;
        const summary = el("summary");
        const members = views.filter((v) => (v.group || "Other") === name);
        const down = members.filter((v) => v.state.status === "down").length;
        const unknown = members.filter((v) => v.state.status === "unknown").length;
        summary.append(el("span", name), el("small", `${members.length}${down ? ` · ${down} down` : unknown ? ` · ${unknown} pending` : " · all up"}`, down ? "error" : ""));
        group.append(summary);
        group.addEventListener("toggle", () => {
          if (group.isConnected) expandedGroups.set(name, group.open);
        });
        groups.set(name, group);
        list.append(group);
      }
    }
  }
  const ordered = privateView
    ? [...views].sort((a, b) => Number(b.state.status === "down") - Number(a.state.status === "down"))
    : views;
  for (const v of ordered) {
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
    (privateView ? groups.get(v.group || "Other") : list).append(b);
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
      v.latency_ms === null || v.type === "heartbeat"
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
  const deltas = h.slice(1).map((p, i) => p.time - h[i].time).filter((d) => d > 0).sort((a, b) => a - b);
  const cadence = v.interval_seconds || deltas[Math.floor(deltas.length / 2)] || 0;
  const section = el("div", undefined, "section");
  const historyLabel = el("div", undefined, "label");
  section.append(historyLabel);
  const bars = el("div", undefined, "bars");
  const observations = [];
  let prev = null;
  for (const p of h) {
    if (prev && cadence && p.time - prev.time > cadence * 1.5) {
      observations.push({ time: prev.time, gap: true });
    }
    observations.push(p);
    prev = p;
  }
  bars.drawBars = (width) => {
    // Keep blocks at least 8px wide with 4px gutters, even on phones.
    const count = Math.min(60, Math.max(1, Math.floor((width + 4) / 12)));
    const size = Math.max(1, Math.ceil(observations.length / count));
    historyLabel.textContent = `Recent checks · ${h.length} observations${size > 1 ? " · grouped" : ""}`;
    bars.replaceChildren();
    for (let i = 0; i < observations.length; i += size) {
      const group = observations.slice(i, i + size);
      const failures = group.filter((p) => !p.gap && !p.ok).length;
      const gap = group.some((p) => p.gap);
      const status = failures ? "down" : gap ? "unknown" : "up";
      const bar = el("span", undefined, "bar " + status);
      bar.title = `${fmt(group[0].time)} – ${fmt(group[group.length - 1].time)} · ${group.filter((p) => !p.gap).length} checks · ${failures} failures${gap ? " · monitoring gap" : ""}`;
      bars.append(bar);
    }
  };
  section.append(bars);
  historyObserver.observe(bars);
  const times = el("div", undefined, "times");
  times.append(
    el("span", h.length ? fmt(h[0].time) : "No observations"),
    el("span", h.length ? fmt(h[h.length - 1].time) : ""),
  );
  section.append(times);
  if (h.length > 1 && v.latency_ms !== null && v.type !== "heartbeat") {
    const ns = "http://www.w3.org/2000/svg",
      svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 600 125");
    svg.setAttribute("preserveAspectRatio", "none");
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
  if (privateView && v.check) {
    const technical = el("section", undefined, "section technical");
    technical.append(el("h2", "Check details"));
    const fields = el("dl");
    const c = v.check;
    for (const [label, value] of [
      ["Type", c.type], ["Target", c.target], ["Description", c.description],
      ["Interval", c.interval], ["Timeout", c.timeout],
      ["Failure threshold", c.failures], ["Recovery threshold", c.successes],
      ["Expected HTTP status", c.expected_status], ["Expected text", c.body_contains],
      ["Heartbeat freshness", c.max_age],
    ]) {
      if (value === undefined || value === "") continue;
      fields.append(el("dt", label), el("dd", String(value)));
    }
    technical.append(fields);
    detail.append(technical);
  }
}
async function refresh() {
  try {
    const r = await fetch(privateView ? "/api/private" : statusSlug ? "/api/status/" + encodeURIComponent(statusSlug) : "/api/public", {
      cache: "no-store",
    });
    if (!r.ok) throw Error();
    const data = await r.json();
    views = statusSlug ? data.monitors : data;
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
if (privateView) {
  document.querySelector("h1").textContent = "Owner dashboard";
  document.querySelector("aside").classList.add("grouped");
}
refresh();
setInterval(refresh, 15000);
