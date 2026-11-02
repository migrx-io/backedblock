"use strict";

// Prometheus scrapes every 15s; refreshing faster shows nothing new.
const REFRESH_MS = 15000;
const SERIES_SLOTS = 8;

const $ = (id) => document.getElementById(id);
const state = { range: 3600, overview: null, charts: {}, timer: null, updatedAt: 0, pools: [] };

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function css(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

// Color follows the node: its position in the node table. Past eight nodes
// the rest are drawn in muted ink and stay identified by the legend and the
// table.
function nodeIndex(pool, ip) {
  return (state.overview?.nodes || []).findIndex((n) => n.pool === pool && n.ip === ip);
}

function nodeColor(pool, ip) {
  const i = nodeIndex(pool, ip);
  return i >= 0 && i < SERIES_SLOTS ? css(`--s${i + 1}`) : css("--muted");
}

function nodeName(pool, ip) {
  const n = state.overview?.nodes[nodeIndex(pool, ip)];
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
  $("tabs").hidden = false;
  applyView();
  refresh();
}

// The view and the pool are in the URL hash (#cache&pool=pool1), so a reload
// keeps both.
function hashParams() {
  const [view, ...rest] = location.hash.replace(/^#/, "").split("&");
  return { view, pool: new URLSearchParams(rest.join("&")).get("pool") || "all" };
}

function currentView() {
  return hashParams().view === "cache" ? "cache" : "overview";
}

// The selected pools, in the configured order; every pool when none is
// picked (or the URL names none that exist).
function selectedPools() {
  const want = new Set(hashParams().pool.split(","));
  const picked = state.pools.filter((p) => want.has(p));
  return picked.length ? picked : [...state.pools];
}

function allPicked() {
  return selectedPools().length === state.pools.length;
}

function setHash(view, pools) {
  const all = !pools || pools.length === 0 || pools.length === state.pools.length;
  location.hash = view + (all ? "" : `&pool=${pools.map(encodeURIComponent).join(",")}`);
}

function poolQuery() {
  return `pool=${allPicked() ? "all" : selectedPools().map(encodeURIComponent).join(",")}`;
}

document.querySelectorAll("#tabs a").forEach((a) =>
  a.addEventListener("click", (e) => {
    e.preventDefault();
    setHash(a.dataset.view, allPicked() ? [] : selectedPools());
  }),
);

$("pool-menu").addEventListener("change", (e) => {
  const box = e.target;
  let pools;
  if (box.value === "*") {
    pools = []; // all
  } else {
    pools = [...$("pool-menu").querySelectorAll("input[data-pool]:checked")].map((b) => b.value);
  }
  // Unticking the last pool means every pool, never none.
  setHash(currentView(), pools);
});

// Close the menu on a click outside it.
document.addEventListener("click", (e) => {
  if (!$("pool-pick").contains(e.target)) $("pool-pick").open = false;
});

function renderPools() {
  $("pool-pick").hidden = state.pools.length === 0;
  const picked = new Set(selectedPools());
  const all = allPicked();
  $("pool-menu").innerHTML =
    `<label class="all"><input type="checkbox" value="*" ${all ? "checked disabled" : ""}> All pools</label>` +
    state.pools.map((p) => `<label><input type="checkbox" data-pool value="${esc(p)}" ${!all && picked.has(p) ? "checked" : ""}> ${esc(p)}</label>`).join("");
  $("pool-label").textContent = all ? "All" : [...picked].join(", ");
  document.body.classList.toggle("multi-pool", picked.size > 1);
}

function applyView() {
  renderPools();
  const v = currentView();
  document.querySelectorAll(".view").forEach((el) => (el.hidden = el.id !== `view-${v}`));
  document.querySelectorAll("#tabs a").forEach((a) => (a.dataset.view === v ? a.setAttribute("aria-current", "page") : a.removeAttribute("aria-current")));
}

window.addEventListener("hashchange", () => {
  if ($("main-view").hidden) return;
  applyView();
  $("warnings").innerHTML = "";
  refresh();
});

function stopRefresh() {
  clearTimeout(state.timer);
  state.timer = null;
}

async function refresh() {
  stopRefresh();
  // Keep the previous render, dimmed, while new data loads.
  document.querySelectorAll(".chart").forEach((c) => c.classList.add("loading"));
  try {
    if (currentView() === "cache") {
      renderCache(await api(`/api/cache?${poolQuery()}`));
    } else {
      state.overview = await api(`/api/overview?${poolQuery()}`);
      renderOverview(state.overview);
      await renderCharts();
    }
    state.updatedAt = Date.now();
  } catch (err) {
    if (err.auth) return showLogin();
    $("warnings").innerHTML = `<div>${esc(err.message)}</div>`;
  } finally {
    document.querySelectorAll(".chart").forEach((c) => c.classList.remove("loading"));
  }
  state.timer = setTimeout(refresh, REFRESH_MS);
}

function kpi(label, value, sub, cls) {
  return `<div class="kpi"><div class="k">${esc(label)}</div><div class="v">${esc(value)}</div><div class="s ${cls || ""}">${esc(sub || "")}</div></div>`;
}

function renderOverview(ov) {
  $("warnings").innerHTML = (ov.warnings || []).map((w) => `<div>${esc(w)}</div>`).join("");

  const nodes = ov.nodes;
  const up = nodes.filter((n) => n.status === "UP").length;
  const listed = nodes.filter((n) => n.status).length;
  const down = (svc) => nodes.filter((n) => n.services && n.services[svc] === false).length;
  const tiles = [
    kpi("Nodes", nodes.length, listed === nodes.length ? "all reporting" : `${nodes.length - listed} not reporting`, listed === nodes.length ? "" : "bad"),
    kpi("Nodes up", up, up === nodes.length && nodes.length ? "all up" : `${nodes.length - up} not up`, up === nodes.length && nodes.length ? "good" : "bad"),
  ];
  for (const svc of ["core", "cache", "storage"]) {
    const d = down(svc);
    tiles.push(kpi(`${svc[0].toUpperCase() + svc.slice(1)} down`, d, d ? "see the table" : "none", d ? "bad" : "good"));
  }
  $("kpis").innerHTML = tiles.join("");

  $("nodes").innerHTML = nodes.map(nodeRow).join("") || `<tr><td colspan="10" class="dim">No nodes report to Prometheus.</td></tr>`;
}

function status(text, kind) {
  return `<span class="st ${kind}">${esc(text)}</span>`;
}

function clusterStatus(n) {
  if (n.status === "UP") return status("UP", "good");
  if (n.status === "DOWN") return status("DOWN", "bad");
  return status("not reporting", "none");
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

function nodeRow(n) {
  return `<tr>
    <td><span class="swatch" style="background:${nodeColor(n.pool, n.ip)}"></span></td>
    <td class="col-pool">${esc(n.pool)}</td>
    <td><div class="name">${esc(n.hostname || n.ip)}</div><div class="ip">${esc(n.ip)}</div></td>
    <td>${clusterStatus(n)}</td>
    <td>${service(n, "core")}</td>
    <td>${service(n, "cache")}</td>
    <td>${service(n, "storage")}</td>
    <td class="num">${meter(n.cpu)}</td>
    <td class="num">${meter(n.ram)}</td>
    <td class="dim">${esc(n.uuid || "–")}</td>
  </tr>`;
}

// --- cache --------------------------------------------------------------

// Sizes come in MiB; show them in GiB or TiB (binary units, as the cache
// plugin reports them).
function size(mib) {
  if (mib == null) return "–";
  const gib = mib / 1024;
  return gib >= 1024 ? `${(gib / 1024).toFixed(2)} TiB` : `${gib.toFixed(gib >= 100 ? 0 : 1)} GiB`;
}

function check(ok, good = "ok", bad = "failed") {
  return ok ? status(good, "good") : status(bad, "bad");
}

function nodeCell(n) {
  return `<div class="name">${esc(n.hostname || n.node)}</div>${n.ip ? `<div class="ip">${esc(n.ip)}</div>` : ""}`;
}

function renderCache(c) {
  $("warnings").innerHTML = (c.warnings || []).map((w) => `<div>${esc(w)}</div>`).join("");
  const s = c.summary;
  const total = s.nodes_up + s.nodes_down;
  const downTile = (label, n) => kpi(label, n, n ? "see the tables" : "none", n ? "bad" : "good");
  $("cache-kpis").innerHTML = [
    kpi("Cache nodes up", s.nodes_up, total ? `of ${total}` : "no cache nodes report", s.nodes_up === total && total ? "good" : "bad"),
    downTile("Cache nodes down", s.nodes_down),
    downTile("RAID down", s.raids_down),
    downTile("Mounts down", s.mounts_down),
    kpi("RW cache", size(s.rw_mib), "write-back, all nodes"),
    kpi("Read cache", size(s.r_mib), "all nodes"),
    kpi("Disks", s.disks, "cache disks, all nodes"),
  ].join("");

  const empty = (cols, what) => `<tr><td colspan="${cols}" class="dim">No ${what} reported.</td></tr>`;

  $("cache-nodes").innerHTML = c.nodes.map((n) => `<tr>
    <td class="col-pool">${esc(n.pool)}</td>
    <td>${nodeCell(n)}</td>
    <td>${check(n.status, "up", "down")}</td>
    <td>${check(n.mounts, "attached", "detached")}</td>
    <td>${check(n.errors, "passing", "failing")}</td>
    <td class="num">${n.disks}</td>
    <td class="num">${size(n.rw_mib)}</td>
    <td class="num">${size(n.r_mib)}</td>
  </tr>`).join("") || empty(8, "cache nodes");

  $("cache-raids").innerHTML = c.raids.map((r) => {
    const bad = r.members.filter((m) => !m.ok);
    const summary = `${r.members.length - bad.length} of ${r.members.length} active` + (r.total_devices && r.total_devices !== r.members.length ? ` · ${r.total_devices} expected` : "");
    const list = r.members.map((m) => `<li>${esc(m.device)} — ${m.ok ? esc(m.state) : `<b class="st bad">${esc(m.state || "missing")}</b>`}</li>`).join("");
    return `<tr>
      <td class="col-pool">${esc(r.pool)}</td>
      <td>${nodeCell(r)}</td>
      <td class="mono">${esc(r.raid)}</td>
      <td>${esc(r.level)}</td>
      <td>${check(r.state_ok && r.ok, r.state || "ok", r.state || "failed")}</td>
      <td>${check(r.no_failed, "none", "failed")}</td>
      <td><details class="members"><summary>${bad.length ? status(summary, "bad") : esc(summary)}</summary><ul>${list}</ul></details></td>
    </tr>`;
  }).join("") || empty(7, "RAIDs");

  $("cache-mounts").innerHTML = c.mounts.map((m) => `<tr>
    <td class="col-pool">${esc(m.pool)}</td>
    <td>${nodeCell(m)}</td>
    <td>${m.kind === "rw" ? "read-write" : m.kind === "r" ? "read" : esc(m.kind)}</td>
    <td class="mono">${esc(m.mountpoint)}</td>
    <td class="mono">${esc(m.device)}</td>
    <td class="mono">${esc(m.options)}</td>
    <td>${check(m.ok, "mounted", "down")}</td>
  </tr>`).join("") || empty(7, "mounts");
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
  await Promise.all(["cpu", "ram"].map(async (m) => {
    try {
      drawChart(m, await api(`/api/series?metric=${m}&range=${state.range}&${poolQuery()}`));
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

  // Recreated when the nodes change, so colors and labels follow them.
  const key = data.series.map((s) => s.pool + "/" + s.ip).join(",") + "|" + state.range;
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
        label: nodeName(s.pool, s.ip),
        stroke: nodeColor(s.pool, s.ip),
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
    state.pools = info.pools || [];
    $("cluster").textContent = info.cluster;
    document.title = `${info.cluster} · bbctl ui`;
    $("login-cluster").textContent = `cluster ${info.cluster}`;
    if (info.login_hint) {
      $("login-hint").textContent = info.login_hint;
      $("login-hint-line").hidden = false;
    }
    const resp = await fetch("/api/overview", { credentials: "same-origin" });
    if (resp.status === 401) return showLogin();
    showMain();
  } catch {
    showLogin("bbctl ui is not reachable. Is it still running?");
  }
})();
