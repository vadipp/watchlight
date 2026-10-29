"use strict";
const privateView = location.pathname === "/dashboard";
const statusSlug = location.pathname.startsWith("/status/") ? location.pathname.slice(8) : null;
let selectedWindow = "24h";
const endpoint = privateView ? "/api/private" : statusSlug ? "/api/status/" + encodeURIComponent(statusSlug) : "/api/public";
const detailCache = new Map();
const detailTTL = 15000;
let detailRequest = null, detailError = null;
let listLoading = false, healthLoading = false;
const detailKey = () => JSON.stringify([selected, selectedWindow]);
let selected = null,
  views = [];
const expandedGroups = new Map();
const el = (tag, text, cls) => {
  const n = document.createElement(tag);
  if (text !== undefined) n.textContent = text;
  if (cls) n.className = cls;
  return n;
};
const fmt = (t) => {
 const d = new Date(t * 1000), pad = n => String(n).padStart(2, "0");
 return `${pad(d.getDate())}.${pad(d.getMonth()+1)}.${d.getFullYear()} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
};
const milliseconds = (value) => new Intl.NumberFormat(undefined, {
  maximumSignificantDigits: 3,
}).format(value) + " ms";
function chartCeiling(value) {
  const magnitude = 10 ** Math.floor(Math.log10(value || 1));
  return [1, 2, 5, 10].find((step) => step * magnitude >= value) * magnitude;
}
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
      loadDetail();
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
  renderDetail();
}
function renderDetail() {
  historyObserver.disconnect();
  const detail = document.querySelector("#detail");
  detail.replaceChildren();
  const summary = views.find((v) => v.id === selected);
  if (!summary) {
    detail.append(el("p", "No services to display."));
    return;
  }
  detail.append(el("h2", summary.name));
  const selector = el("div", undefined, "window-selector");
  selector.setAttribute("role", "group");
  selector.setAttribute("aria-label", "History window");
  for (const [key, label] of [["4h", "4 hours"], ["24h", "24 hours"], ["7d", "7 days"]]) {
    const button = el("button", label);
    button.setAttribute("aria-pressed", String(key === selectedWindow));
    button.onclick = () => { selectedWindow = key; renderDetail(); loadDetail(); };
    selector.append(button);
  }
  detail.append(selector);
  const cached = detailCache.get(detailKey());
  const error = detailError?.key === detailKey() ? detailError.message : null;
  if (error) detail.append(el("p", error + (cached ? " Displayed details may be stale." : ""), "label error"));
  if (!cached) {
    if (!error) detail.append(el("p", "Loading details…", "label"));
    return;
  }
  const v = {...cached.data, state: {...cached.data.state, status: summary.state.status}};
  if (Date.now() - cached.at >= detailTTL && detailRequest?.key === detailKey())
    detail.append(el("p", "Updating details…", "label"));
  const metrics = el("div", undefined, "metrics");
  for (const [label, value] of [
    [
      `Observed uptime · ${Math.round((v.window_end-v.window_start)/3600) === 168 ? "7d" : Math.round((v.window_end-v.window_start)/3600)+"h"}`,
      v.uptime === null ? "—" : v.uptime.toFixed(2) + "%",
    ],
    ...(v.latency_ms !== null && v.type !== "heartbeat" ? [["Response time", milliseconds(v.latency_ms)]] : []),
    ["Current status", v.state.status],
  ]) {
    const m = el("div", undefined, "metric");
    m.append(el("b", value), el("span", label));
    metrics.append(m);
  }
  detail.append(metrics);
  const h = v.buckets.filter(b => b.checks > 0).map(b => ({time: (b.time+b.end)/2, latency: b.latency}));
  const section = el("div", undefined, "section");
  const historyLabel = el("div", `${v.samples} observed checks · green: up · red: down · grey: unknown`, "label");
  section.append(historyLabel);
  const bars = el("div", undefined, "bars");
  const tooltip = el("p", "Tap a bar for details.", "label bar-details");
  bars.drawBars = (width) => {
    const capacity = Math.min(60, Math.max(1, Math.floor((width + 4) / 12)));
    const count = [60,48,40,30,24,20,16,15,12,10,8,6,5,4,3,2,1].find(n => n <= capacity);
    bars.replaceChildren();
    for (let i = 0; i < count; i++) {
      const group = v.buckets.slice(Math.floor(i*240/count), Math.floor((i+1)*240/count));
      const duration = group[group.length-1].end-group[0].time;
      const sum = key => group.reduce((n,b) => n+b[key],0);
      const up = sum("up"), down = sum("down"), unknown = Math.max(0,duration-up-down);
      const bar = el("button", undefined, "bar");
      const description = `${fmt(group[0].time)} – ${fmt(group[group.length-1].end)} · ${sum("checks")} checks · ${sum("failures")} failures · up ${(100*up/duration).toFixed(2)}% · down ${(100*down/duration).toFixed(2)}% · unknown ${(100*unknown/duration).toFixed(2)}%`;
      bar.title = description;
      bar.setAttribute("aria-label", description);
      bar.onclick = bar.onfocus = () => { tooltip.textContent = description; };
      for (const [status, amount] of [["up",up],["down",down],["unknown",unknown]]) {
        if (amount <= 0) continue;
        const segment = el("span", undefined, "bar-segment "+status);
        segment.style.flexGrow = amount;
        bar.append(segment);
      }
      bars.append(bar);
    }
  };
  section.append(bars, tooltip);
  historyObserver.observe(bars);
  const times = el("div", undefined, "times");
  times.append(el("span", fmt(v.window_start)), el("span", fmt(v.window_end)));
  section.append(times);
  if (h.length > 1 && v.latency_ms !== null && v.type !== "heartbeat") {
    const ns = "http://www.w3.org/2000/svg",
      svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 600 125");
    svg.setAttribute("preserveAspectRatio", "none");
    svg.classList.add("chart");
    svg.setAttribute("role", "img");
    const observedMax = Math.max(0, ...h.map((p) => p.latency));
    const max = chartCeiling(observedMax);
    const observedMin = Math.min(...h.map((p) => p.latency));
    svg.setAttribute("aria-label", `Response time chart. Scale: 0 to ${milliseconds(max)}. Observed bucket mean range: ${milliseconds(observedMin)} to ${milliseconds(observedMax)}.`);
    const chart = el("div", undefined, "response-chart");
    const axis = el("div", undefined, "chart-axis");
    axis.setAttribute("aria-hidden", "true");
    for (const value of [max, max / 2, 0]) axis.append(el("span", milliseconds(value)));
    for (const y of [10, 65, 120]) {
      const grid = document.createElementNS(ns, "line");
      grid.setAttribute("x1", "0");
      grid.setAttribute("x2", "600");
      grid.setAttribute("y1", String(y));
      grid.setAttribute("y2", String(y));
      grid.classList.add("chart-grid");
      svg.append(grid);
    }
    const title = document.createElementNS(ns, "title");
    title.textContent = `Observed bucket mean response times: ${milliseconds(observedMin)} – ${milliseconds(observedMax)}`;
    svg.append(title);
    let run = [];
    const flush = () => {
      if (!run.length) return;
      const line = document.createElementNS(ns, "polyline");
      line.setAttribute("points", run.join(" "));
      svg.append(line);
      run = [];
    };
    for (const b of v.buckets) {
      if (!b.checks) { flush(); continue; }
      run.push(`${(((b.time+b.end)/2-v.window_start)/(v.window_end-v.window_start))*600},${120-(b.latency/max)*110}`);
    }
    flush();
    chart.append(axis, svg);
    section.append(chart, el("div", `Observed bucket mean range: ${milliseconds(observedMin)} – ${milliseconds(observedMax)}`, "label chart-range"));
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
      `Last check: ${v.state.checked ? fmt(v.state.checked) : "never"} · ${v.samples} observed checks in the selected window`,
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
      ["Connection reuse", c.type === "http" ? (c.disable_keep_alive ? "Disabled" : "Enabled") : null],
      ["Latency recording", c.type === "http" ? (c.record_latency === false ? "Disabled" : "Enabled") : null],
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
async function loadDetail(force = false) {
  const key = detailKey();
  if (detailRequest?.key === key) return;
  detailRequest?.controller.abort();
  detailRequest = null;
  detailError = null;
  if (!selected) { renderDetail(); return; }
  const cached = detailCache.get(key);
  if (!force && cached && Date.now() - cached.at < detailTTL) { renderDetail(); return; }
  const request = {key, controller: new AbortController()};
  detailRequest = request;
  renderDetail();
  try {
    const r = await fetch(endpoint + "/" + encodeURIComponent(selected) + "?window=" + selectedWindow, {
      cache: "no-store", signal: request.controller.signal,
    });
    if (!r.ok) throw Error();
    const data = await r.json();
    if (request !== detailRequest || key !== detailKey()) return;
    detailCache.delete(key);
    detailCache.set(key, {data, at: Date.now()});
    // Bound the per-page cache when many monitors/windows are visited.
    if (detailCache.size > 24) detailCache.delete(detailCache.keys().next().value);
  } catch (error) {
    if (request !== detailRequest || key !== detailKey()) return;
    if (error.name !== "AbortError") detailError = {key, message: "Unable to load details. Retrying on the next refresh."};
  } finally {
    if (request === detailRequest) { detailRequest = null; renderDetail(); }
  }
}
async function refreshList() {
  if (listLoading) return;
  listLoading = true;
  try {
    const r = await fetch(endpoint, {cache: "no-store"});
    if (!r.ok) throw Error();
    const data = await r.json();
    views = statusSlug ? data.monitors : data;
    if (!views.some(v => v.id === selected)) {
      selected = views[0]?.id ?? null;
      detailRequest?.controller.abort();
      detailRequest = null;
    }
    render();
    loadDetail();
    document.querySelector("#updated").textContent = "Updated " + fmt(Date.now()/1000);
  } catch {
    document.querySelector("#summary").textContent = "Unable to refresh monitoring data";
    document.querySelector("#summary").className = "summary unknown";
    document.querySelector("#updated").textContent = "Connection lost · displayed statuses may be stale";
  } finally { listLoading = false; }
}
async function refreshHealth() {
  if (!privateView || healthLoading) return;
  healthLoading = true;
  const panel = document.querySelector("#delivery");
  panel.hidden = false;
  try {
    const r = await fetch("/api/health", {cache: "no-store"});
    if (!r.ok && r.status !== 503) throw Error();
    const health = await r.json();
    const telegram = health.telegram ? health.telegram.ok ? "API reachable" : "API probe failing or pending" : "not configured";
    panel.textContent = "Monitor engine: " + (health.ok ? "checks fresh" : "checks stale") + " · Telegram: " + telegram + " · Queued alerts: " + health.pending_notifications;
  } catch { panel.textContent = "Unable to refresh engine health"; }
  finally { healthLoading = false; }
}
window.addEventListener("pagehide", () => detailRequest?.controller.abort());

if (privateView) {
  document.querySelector("h1").textContent = "Owner dashboard";
  document.querySelector("aside").classList.add("grouped");
}
refreshList();
refreshHealth();
setInterval(() => { refreshList(); refreshHealth(); loadDetail(true); }, 15000);
