"use strict";

// Prometheus scrapes every 15s; refreshing faster shows nothing new.
const REFRESH_MS = 15000;
const SERIES_SLOTS = 8;

const $ = (id) => document.getElementById(id);
const state = { range: 3600, overview: null, charts: {}, timer: null, updatedAt: 0 };

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function css(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

// Color follows the node: its position in the node table, which is the
// deployment's node order. Past eight nodes the rest are drawn in muted ink
// and stay identified by the legend and the table.
function nodeColor(ip) {
  const i = (state.overview?.nodes || []).findIndex((n) => n.ip === ip);
  return i >= 0 && i < SERIES_SLOTS ? css(`--s${i + 1}`) : css("--muted");
}

function nodeName(ip) {
  const n = (state.overview?.nodes || []).find((n) => n.ip === ip);
  return n?.hostname || ip;
}

async function api(path, opts = {}) {
  const resp = await fetch(path, { credentials: "same-origin", ...opts });
  const body = await resp.json().catch(() => ({}));
  if (resp.status === 401) throw Object.assign(new Error(body.error || "not logged in"), { auth: true });
  if (!resp.ok) throw new Error(body.error || resp.statusText);
  return body;
}

// --- login --------------------------------------------------------------

function showLogin(message) {
  stopRefresh();
  $("main-view").hidden = true;
  $("logout").hidden = true;
  $("login-view").hidden = false;
  $("login-error").textContent = message || "";
  $("login-form").password.focus();
}

$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  $("login-error").textContent = "";
  f.querySelector("button").disabled = true;
  try {
    await api("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: f.username.value, password: f.password.value }),
    });
    f.password.value = "";
    showMain();
  } catch (err) {
    $("login-error").textContent = err.message;
  } finally {
    f.querySelector("button").disabled = false;
  }
});

$("logout").addEventListener("click", async () => {
  await api("/api/logout", { method: "POST" }).catch(() => {});
  showLogin();
});

// --- main view ----------------------------------------------------------

function showMain() {
  $("login-view").hidden = true;
  $("main-view").hidden = false;
  $("logout").hidden = false;
  refresh();
}

function stopRefresh() {
  clearTimeout(state.timer);
  state.timer = null;
}

async function refresh() {
  stopRefresh();
  // Keep the previous render, dimmed, while new data loads.
  document.querySelectorAll(".chart").forEach((c) => c.classList.add("loading"));
  try {
    state.overview = await api("/api/overview");
    renderOverview(state.overview);
    await renderCharts();
    state.updatedAt = Date.now();
  } catch (err) {
    if (err.auth) return showLogin();
    $("warnings").innerHTML = `<div>${esc(err.message)}</div>`;
  } finally {
    document.querySelectorAll(".chart").forEach((c) => c.classList.remove("loading"));
  }
  state.timer = setTimeout(refresh, REFRESH_MS);
}

function renderHeader(ov) {
  $("deployment").textContent = ov.deployment;
  document.title = `${ov.deployment} · bbctl ui`;
  const pools = ov.pools.length > 1 ? ` <span class="dim">of ${ov.pools.map(esc).join(", ")}</span>` : "";
  $("meta").innerHTML = `<span>region <b>${esc(ov.region)}</b></span><span>pool <b>${esc(ov.pool)}</b>${pools}</span>`;
}

function kpi(label, value, sub, cls) {
  return `<div class="kpi"><div class="k">${esc(label)}</div><div class="v">${esc(value)}</div><div class="s ${cls || ""}">${esc(sub || "")}</div></div>`;
}

function renderOverview(ov) {
  renderHeader(ov);
  $("warnings").innerHTML = (ov.warnings || []).map((w) => `<div>${esc(w)}</div>`).join("");

  const nodes = ov.nodes;
  const up = nodes.filter((n) => n.status === "UP").length;
  const listed = nodes.filter((n) => n.status).length;
  const down = (svc) => nodes.filter((n) => n.services && n.services[svc] === false).length;
  const tiles = [
    kpi("Nodes", nodes.length, `${listed} in the cluster`),
    kpi("Nodes up", up, up === nodes.length && nodes.length ? "all up" : `${nodes.length - up} not up`, up === nodes.length && nodes.length ? "good" : "bad"),
  ];
  if (ov.metrics) {
    for (const svc of ["core", "cache", "storage"]) {
      const d = down(svc);
      tiles.push(kpi(`${svc[0].toUpperCase() + svc.slice(1)} down`, d, d ? "see the table" : "none", d ? "bad" : "good"));
    }
  }
  $("kpis").innerHTML = tiles.join("");

  // The gateway reports 127.0.0.1 when no VIP address is assigned; the node
  // holding the VIP role is still worth showing.
  const vipNode = ov.vip_node ? esc(nodeName(ov.vip_node)) : "";
  const vipAddr = ov.vip && !ov.vip.startsWith("127.") ? `<b>${esc(ov.vip)}</b> on ` : "";
  const vip = vipNode ? ` · VIP ${vipAddr}${vipNode}` : "";
  $("cluster-line").innerHTML = `cluster <b>${esc(ov.cluster)}</b>${vip}`;

  $("nodes").innerHTML = nodes.map(nodeRow).join("") || `<tr><td colspan="12" class="dim">No nodes yet. Run bbctl apply.</td></tr>`;
}

function status(text, kind) {
  return `<span class="st ${kind}">${esc(text)}</span>`;
}

function clusterStatus(n) {
  if (n.status === "UP") return status("UP", "good");
  if (n.status === "DOWN") return status("DOWN", "bad");
  return status("not listed", "none");
}

function service(n, svc) {
  if (!n.services || !(svc in n.services)) return status("–", "none");
  return n.services[svc] ? status("up", "good") : status("down", "bad");
}

function meter(v) {
  if (v == null) return `<span class="dim">–</span>`;
  const pct = Math.max(0, Math.min(100, v));
  return `<span class="meter"><span>${v.toFixed(1)}%</span><span class="track"><span class="fill" style="width:${pct}%"></span></span></span>`;
}

function ssmStatus(s) {
  if (!s) return status("–", "none");
  return status(s, s === "Online" ? "good" : "warn");
}

function provision(p) {
  if (p === "Success") return status("done", "good");
  if (p === "Failed") return status("failed", "bad");
  if (!p) return status("–", "none");
  return status(p.toLowerCase(), "warn");
}

function nodeRow(n) {
  return `<tr>
    <td><span class="swatch" style="background:${nodeColor(n.ip)}"></span></td>
    <td><div class="name">${esc(n.hostname || n.ip)}</div><div class="ip">${esc(n.ip)}</div></td>
    <td>${clusterStatus(n)}</td>
    <td>${service(n, "core")}</td>
    <td>${service(n, "cache")}</td>
    <td>${service(n, "storage")}</td>
    <td class="num">${meter(n.cpu)}</td>
    <td class="num">${meter(n.ram)}</td>
    <td><div>${esc(n.instance_id || "–")}</div><div class="dim">${esc([n.instance_type, n.az].filter(Boolean).join(" · "))}</div></td>
    <td>${n.state ? status(n.state, n.state === "running" ? "good" : "warn") : status("–", "none")}</td>
    <td>${ssmStatus(n.ssm)}</td>
    <td>${provision(n.provision)}</td>
  </tr>`;
}

// --- charts -------------------------------------------------------------

document.querySelectorAll("#filters [data-range]").forEach((b) =>
  b.addEventListener("click", () => {
    document.querySelectorAll("#filters [data-range]").forEach((x) => x.setAttribute("aria-checked", String(x === b)));
    state.range = Number(b.dataset.range);
    refresh();
  }),
);

async function renderCharts() {
  if (!state.overview.metrics) {
    $("filters").hidden = true;
    $("charts").hidden = true;
    return;
  }
  $("filters").hidden = false;
  $("charts").hidden = false;
  await Promise.all(["cpu", "ram"].map(async (m) => {
    try {
      drawChart(m, await api(`/api/series?metric=${m}&range=${state.range}`));
    } catch (err) {
      if (err.auth) throw err;
      emptyChart(m, err.message);
    }
  }));
}

function emptyChart(metric, message) {
  state.charts[metric]?.destroy();
  delete state.charts[metric];
  $(`chart-${metric}`).innerHTML = `<div class="empty">${esc(message)}</div>`;
}

function drawChart(metric, data) {
  const el = $(`chart-${metric}`);
  if (!data.series || data.series.length === 0) return emptyChart(metric, "No data yet.");

  const key = data.series.map((s) => s.ip).join(",") + "|" + state.range;
  const aligned = [data.t, ...data.series.map((s) => s.v)];
  const existing = state.charts[metric];
  // Same nodes and range: update in place, so the chart does not flash.
  if (existing && existing._key === key) {
    existing.setData(aligned);
    return;
  }
  existing?.destroy();
  el.innerHTML = "";

  const axis = {
    stroke: css("--muted"),
    grid: { stroke: css("--grid"), width: 1 },
    ticks: { stroke: css("--grid"), width: 1, size: 4 },
    font: "12px system-ui, sans-serif",
  };
  const u = new uPlot({
    width: el.clientWidth || 600,
    height: 220,
    padding: [8, 8, 0, 0],
    cursor: { points: { size: 8 }, drag: { x: false, y: false } },
    legend: { live: true },
    scales: { y: { range: [0, 100] } },
    axes: [
      { ...axis, space: 80 },
      { ...axis, size: 48, values: (u, vals) => vals.map((v) => v + "%") },
    ],
    series: [
      { value: (u, v) => (v == null ? "–" : new Date(v * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })) },
      ...data.series.map((s) => ({
        label: nodeName(s.ip),
        stroke: nodeColor(s.ip),
        width: 2,
        points: { show: false },
        value: (u, v) => (v == null ? "–" : v.toFixed(1) + "%"),
      })),
    ],
  }, aligned, el);
  u._key = key;
  state.charts[metric] = u;
}

window.addEventListener("resize", () => {
  for (const [m, u] of Object.entries(state.charts)) {
    const w = $(`chart-${m}`).clientWidth;
    if (w) u.setSize({ width: w, height: 220 });
  }
});

// Start: show what this dashboard is for, then log in unless the session
// still is.
(async () => {
  try {
    const info = await api("/api/info");
    $("deployment").textContent = info.deployment;
    $("login-cluster").textContent = `${info.deployment} / ${info.pool}`;
    $("login-hint").textContent = `bbctl secrets get ${info.deployment} MGX_GW_ADMIN_PASSWD`;
    const resp = await fetch("/api/overview", { credentials: "same-origin" });
    if (resp.status === 401) return showLogin();
    showMain();
  } catch {
    showLogin("bbctl ui is not reachable. Is it still running?");
  }
})();
