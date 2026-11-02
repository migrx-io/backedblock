"use strict";

// Prometheus scrapes every 15s; refreshing faster shows nothing new.
const REFRESH_MS = 15000;
const SERIES_SLOTS = 8;

const $ = (id) => document.getElementById(id);
const state = { range: 3600, overview: null, storage: null, charts: {}, timer: null, updatedAt: 0, pools: [] };

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
  return slotColor(nodeIndex(pool, ip));
}

function nodeName(pool, ip) {
  const n = state.overview?.nodes[nodeIndex(pool, ip)];
  return n?.hostname || ip;
}

// A volume's color is its position in the volume table, as a node's is.
function volumeIndex(pool, name) {
  return (state.storage?.volumes || []).findIndex((v) => v.pool === pool && v.name === name);
}

function slotColor(i) {
  return i >= 0 && i < SERIES_SLOTS ? css(`--s${i + 1}`) : css("--muted");
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
  $("tabs").hidden = true;
  $("pool-pick").hidden = true;
  $("pool-pick").open = false;
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

// The view, the pools and the host shown in detail are in the URL hash
// (#hosts&pool=pool1&host=pool1/10.0.1.10), so a reload keeps them.
function hashParams() {
  const [view, ...rest] = location.hash.replace(/^#/, "").split("&");
  const q = new URLSearchParams(rest.join("&"));
  return { view, pool: q.get("pool") || "all", host: q.get("host") || "" };
}

function currentView() {
  const v = hashParams().view;
  return ["cache", "storage"].includes(v) ? v : "hosts";
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

function setHash(view, pools, host = hashParams().host) {
  const all = !pools || pools.length === 0 || pools.length === state.pools.length;
  location.hash = view + (all ? "" : `&pool=${pools.map(encodeURIComponent).join(",")}`) + (host ? `&host=${encodeURIComponent(host)}` : "");
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
  // Unticking the last pool means every pool, never none; ticking every pool
  // is "All pools".
  setHash(currentView(), pools);
});

// Close the menu on a click outside it.
document.addEventListener("click", (e) => {
  if (!$("pool-pick").contains(e.target)) $("pool-pick").open = false;
});

function renderPools() {
  $("pool-pick").hidden = state.pools.length === 0 || $("main-view").hidden;
  const picked = new Set(selectedPools());
  const all = allPicked();
  $("pool-menu").innerHTML =
    `<label class="all"><input type="checkbox" value="*" ${all ? "checked disabled" : ""}> All pools</label>` +
    state.pools.map((p) => `<label><input type="checkbox" data-pool value="${esc(p)}" ${picked.has(p) ? "checked" : ""}> ${esc(p)}</label>`).join("");
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
    const view = currentView();
    if (view === "cache") {
      renderCache(await api(`/api/cache?${poolQuery()}`));
    } else if (view === "storage") {
      state.storage = await api(`/api/storage?${poolQuery()}`);
      renderStorage(state.storage);
      await renderCharts(["read_mbps", "write_mbps", "read_iops", "write_iops"]);
    } else {
      state.overview = await api(`/api/overview?${poolQuery()}`);
      renderOverview(state.overview);
      await Promise.all([renderCharts(Object.keys(CHARTS).filter((m) => m.startsWith("host_"))), renderHost()]);
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
  const col = (k) => nodes.map((n) => n.values?.[k]).filter((v) => v != null);
  const rootMax = highest(col("root"));
  const tiles = [
    kpi("Nodes", nodes.length, listed === nodes.length ? "all reporting" : `${nodes.length - listed} not reporting`, listed === nodes.length ? "" : "bad"),
    kpi("Nodes up", up, up === nodes.length && nodes.length ? "all up" : `${nodes.length - up} not up`, up === nodes.length && nodes.length ? "good" : "bad"),
  ];
  for (const svc of ["core", "cache", "storage"]) {
    const d = down(svc);
    tiles.push(kpi(`${svc[0].toUpperCase() + svc.slice(1)} down`, d, d ? "see the table" : "none", d ? "bad" : "good"));
  }
  tiles.push(
    kpi("CPU", pct(mean(col("cpu"))), "average"),
    kpi("RAM", pct(mean(col("ram"))), "average"),
    kpi("Root disk", pct(rootMax), "fullest host", rootMax >= 90 ? "bad" : ""),
    kpi("IO pressure", pct(highest(col("psi_io"))), "highest host"),
  );
  $("kpis").innerHTML = tiles.join("");

  const sel = selectedHost();
  $("nodes").innerHTML = nodes.map((n) => nodeRow(n, sel && sel.pool === n.pool && sel.ip === n.ip)).join("") ||
    `<tr><td colspan="15" class="dim">No nodes report to Prometheus.</td></tr>`;
}

// A row picks the host shown in detail below the charts.
$("nodes").addEventListener("click", (e) => {
  const tr = e.target.closest("tr[data-ip]");
  if (tr) setHash("hosts", allPicked() ? [] : selectedPools(), `${tr.dataset.pool}/${tr.dataset.ip}`);
});

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

function nodeRow(n, selected) {
  const v = n.values || {};
  return `<tr class="pick${selected ? " selected" : ""}" data-pool="${esc(n.pool)}" data-ip="${esc(n.ip)}" title="${esc(n.uuid ? "UUID " + n.uuid : "")}">
    <td><span class="swatch" style="background:${nodeColor(n.pool, n.ip)}"></span></td>
    <td class="col-pool">${esc(n.pool)}</td>
    <td><div class="name">${esc(n.hostname || n.ip)}</div><div class="ip">${esc(n.ip)}</div></td>
    <td>${clusterStatus(n)}</td>
    <td>${service(n, "core")}</td>
    <td>${service(n, "cache")}</td>
    <td>${service(n, "storage")}</td>
    <td class="num">${meter(v.cpu)}<div class="ip">${v.cpus ? `${v.cpus} cores` : ""}</div></td>
    <td class="num">${meter(v.ram)}<div class="ip">${v.ram_total ? bytes(v.ram_total) : ""}</div></td>
    <td class="num">${meter(v.root)}</td>
    <td class="num">${v.load1 == null ? "–" : v.load1.toFixed(2)}</td>
    <td class="num">${bytes(v.disk_read, "/s")} / ${bytes(v.disk_write, "/s")}</td>
    <td class="num">${bytes(v.net_rx, "/s")} / ${bytes(v.net_tx, "/s")}</td>
    <td class="num">${bytes(v.dirty)}</td>
    <td class="num">${pct(v.psi_cpu)} / ${pct(v.psi_memory)} / ${pct(v.psi_io)}</td>
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

// --- storage ------------------------------------------------------------

function renderStorage(st) {
  $("warnings").innerHTML = (st.warnings || []).map((w) => `<div>${esc(w)}</div>`).join("");
  const s = st.summary;
  $("storage-kpis").innerHTML = [
    kpi("Volumes", s.volumes, s.volumes ? "" : "none reported"),
    kpi("Not ready", s.not_ready, s.not_ready ? "see the table" : "none", s.not_ready ? "bad" : "good"),
    kpi("Provisioned", size(s.size_mib), "all volumes"),
    kpi("RW cache", size(s.rw_mib), "write-back, all volumes"),
    kpi("Read cache", size(s.r_mib), "all volumes"),
  ].join("");

  $("volumes").innerHTML = st.volumes.map((v, i) => `<tr>
    <td><span class="swatch" style="background:${slotColor(i)}"></span></td>
    <td class="col-pool">${esc(v.pool)}</td>
    <td class="name">${esc(v.name)}</td>
    <td>${check(v.ready, v.status || "READY", v.status || "unknown")}</td>
    <td class="num">${size(v.size_mib)}</td>
    <td class="num">${size(v.rw_mib)}</td>
    <td class="num">${size(v.r_mib)}</td>
    <td>${v.node ? nodeCell(v) : status("–", "none")}</td>
    <td class="mono">${esc(v.bucket || "–")}</td>
  </tr>`).join("") || `<tr><td colspan="9" class="dim">No volumes reported.</td></tr>`;
}

// --- host detail --------------------------------------------------------

// Byte sizes and rates in binary units, as node_exporter counts them.
function bytes(b, suffix = "") {
  if (b == null) return "–";
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  let i = 0;
  while (Math.abs(b) >= 1024 && i < units.length - 1) { b /= 1024; i++; }
  return `${b.toFixed(Math.abs(b) >= 100 || i === 0 ? 0 : 1)} ${units[i]}${suffix}`;
}

// Network rates in bits, decimal units, as links are rated.
function bits(b) {
  const units = ["b/s", "kb/s", "Mb/s", "Gb/s", "Tb/s"];
  let i = 0;
  while (Math.abs(b) >= 1000 && i < units.length - 1) { b /= 1000; i++; }
  return `${b.toFixed(Math.abs(b) >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

function seconds(v) {
  if (v === 0) return "0";
  if (v < 0.001) return `${(v * 1e6).toFixed(0)} µs`;
  if (v < 1) return `${(v * 1000).toFixed(v < 0.01 ? 2 : 1)} ms`;
  return `${v.toFixed(2)} s`;
}

function duration(s) {
  if (s == null) return "–";
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}

const pct = (v) => (v == null ? "–" : (v < 10 ? v.toFixed(1) : v.toFixed(0)) + "%");
const mean = (xs) => (xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : null);
const highest = (xs) => (xs.length ? Math.max(...xs) : null);

// The host in the hash, if it is still listed; else the first node that
// reports node_exporter readings.
function selectedHost() {
  const nodes = state.overview?.nodes || [];
  const want = hashParams().host;
  const n = nodes.find((n) => `${n.pool}/${n.ip}` === want) || nodes.find((n) => n.values) || nodes[0];
  return n ? { pool: n.pool, ip: n.ip, node: n } : null;
}

$("host-pick").addEventListener("change", (e) => setHash("hosts", allPicked() ? [] : selectedPools(), e.target.value));

// Rows load their charts when opened, so closed ones cost nothing.
document.querySelectorAll("#view-hosts details.row").forEach((d) =>
  d.addEventListener("toggle", () => {
    if (d.open && d.dataset.row !== "quick") renderHostRow(d);
  }),
);

function hostQuery(h) {
  return `pool=${encodeURIComponent(h.pool)}&ip=${encodeURIComponent(h.ip)}`;
}

async function renderHost() {
  const h = selectedHost();
  const nodes = state.overview?.nodes || [];
  $("host-pick").innerHTML = nodes.map((n) => {
    const v = `${n.pool}/${n.ip}`;
    return `<option value="${esc(v)}" ${h && v === `${h.pool}/${h.ip}` ? "selected" : ""}>${esc(n.hostname || n.ip)} (${esc(multiPool() ? n.pool + ", " : "")}${esc(n.ip)})</option>`;
  }).join("");
  $("host-sub").textContent = h?.node.kernel ? `kernel ${h.node.kernel}` : "";
  if (!h) {
    $("host-quick").innerHTML = `<div class="dim">No hosts report to Prometheus.</div>`;
    return;
  }
  const rows = [...document.querySelectorAll("#view-hosts details.row[open]")].filter((d) => d.dataset.row !== "quick");
  await Promise.all([renderQuick(h), ...rows.map(renderHostRow)]);
}

function multiPool() {
  return document.body.classList.contains("multi-pool");
}

function gauge(label, v, sub) {
  const w = v == null ? 0 : Math.max(0, Math.min(100, v));
  return `<div class="kpi"><div class="k">${esc(label)}</div><div class="v">${pct(v)}</div>
    <div class="gauge"><span style="width:${w}%"></span></div><div class="s">${esc(sub || "")}</div></div>`;
}

async function renderQuick(h) {
  let q;
  try {
    q = await api(`/api/host/quick?${hostQuery(h)}`);
  } catch (err) {
    if (err.auth) throw err;
    $("host-quick").innerHTML = `<div class="dim">${esc(err.message)}</div>`;
    return;
  }
  const psi = [["CPU", q.psi_cpu], ["Mem", q.psi_memory], ["I/O", q.psi_io], ["Irq", q.psi_irq]]
    .map(([k, v]) => `<div class="psi"><span>${k}</span>${meter(v)}</div>`).join("");
  $("host-quick").innerHTML = [
    `<div class="kpi"><div class="k">Pressure</div>${psi}</div>`,
    gauge("CPU busy", q.cpu_busy, q.cpu_cores ? `${q.cpu_cores} cores` : ""),
    gauge("Sys load", q.sys_load, "load 1m per core"),
    gauge("RAM used", q.ram_used, q.ram_total ? `of ${bytes(q.ram_total)}` : ""),
    gauge("Swap used", q.swap_used, q.swap_total ? `of ${bytes(q.swap_total)}` : "no swap"),
    gauge("Root FS used", q.root_used, q.root_total ? `of ${bytes(q.root_total)}` : ""),
    kpi("Uptime", duration(q.uptime), ""),
  ].join("");
}

async function renderHostRow(d) {
  const h = selectedHost();
  if (!h) return;
  await Promise.all([...d.querySelectorAll(".chart")].map(async (el) => {
    const id = el.id.replace(/^chart-/, "");
    try {
      drawChart(id, await api(`/api/host/series?${hostQuery(h)}&panel=${id.replace(/^hp-/, "")}&range=${state.range}`));
    } catch (err) {
      if (err.auth) throw err;
      emptyChart(id, err.message);
    }
  }));
}

// --- charts -------------------------------------------------------------

// How each chart scales and prints its values.
const fmtRate = (v, d) => (v >= 100 ? v.toFixed(0) : v.toFixed(d));
const CHARTS = {
  read_mbps: { axis: (v) => fmtRate(v, 1), value: (v) => fmtRate(v, 1) + " MiB/s" },
  write_mbps: { axis: (v) => fmtRate(v, 1), value: (v) => fmtRate(v, 1) + " MiB/s" },
  read_iops: { axis: (v) => fmtRate(v, 0), value: (v) => fmtRate(v, 0) + "/s" },
  write_iops: { axis: (v) => fmtRate(v, 0), value: (v) => fmtRate(v, 0) + "/s" },
};
const percent = { max: 100, axis: (v) => v + "%", value: (v) => v.toFixed(1) + "%" };
const mibps = { axis: (v) => fmtRate(v, 1), value: (v) => fmtRate(v, 1) + " MiB/s" };
// Pressure is mostly near zero: scale to the data, not to 100%.
const stall = { axis: (v) => fmtRate(v, 1) + "%", value: (v) => v.toFixed(2) + "%" };
Object.assign(CHARTS, {
  host_cpu: percent, host_ram: percent,
  host_disk_read: mibps, host_disk_write: mibps, host_net_rx: mibps, host_net_tx: mibps,
  host_dirty: { axis: (v) => fmtRate(v, 1), value: (v) => fmtRate(v, 1) + " MiB" },
  host_psi_cpu: stall, host_psi_memory: stall, host_psi_io: stall,
});

// A host panel's values print by its unit; its axis is wider for them.
const UNITS = {
  percent: { axis: (v) => fmtRate(v, 1) + "%", value: (v) => v.toFixed(2) + "%" },
  bytes: { size: 72, axis: (v) => bytes(v), value: (v) => bytes(v) },
  bytes_rate: { size: 80, axis: (v) => bytes(v, "/s"), value: (v) => bytes(v, "/s") },
  bits: { size: 72, axis: bits, value: bits },
  ops: { axis: (v) => fmtRate(v, 1) + "/s", value: (v) => fmtRate(v, 2) + "/s" },
  seconds: { size: 64, axis: seconds, value: seconds },
  count: { axis: (v) => fmtRate(v, 1), value: (v) => fmtRate(v, 2) },
};

// A node's or volume's line takes its name and color from its row in the
// view's table; a host panel's lines are colored in the panel's order.
function lineLook(s, i) {
  if (s.label) return { label: s.label, stroke: slotColor(i) };
  if (s.volume) return { label: s.volume, stroke: slotColor(volumeIndex(s.pool, s.volume)) };
  return { label: nodeName(s.pool, s.ip), stroke: nodeColor(s.pool, s.ip) };
}

// Every view's range picker sets the one range.
document.querySelectorAll(".filters [data-range]").forEach((b) =>
  b.addEventListener("click", () => {
    document.querySelectorAll(".filters [data-range]").forEach((x) => x.setAttribute("aria-checked", String(x.dataset.range === b.dataset.range)));
    state.range = Number(b.dataset.range);
    refresh();
  }),
);

async function renderCharts(metrics) {
  await Promise.all(metrics.map(async (m) => {
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
  const spec = CHARTS[metric] || UNITS[data.unit] || UNITS.count;
  if (!data.series || data.series.length === 0) {
    return emptyChart(metric, data.zero ? `All ${data.zero} series stayed at zero in this range.` : "No data yet.");
  }

  // Recreated when the nodes change, so colors and labels follow them.
  const key = data.series.map((s) => s.pool + "/" + (s.label || s.volume || s.ip)).join(",") + "|" + state.range;
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
    scales: { y: { range: (u, lo, hi) => [0, spec.max ?? Math.max((hi || 0) * 1.1, 1)] } },
    axes: [
      { ...axis, space: 80 },
      { ...axis, size: spec.size || 52, values: (u, vals) => vals.map(spec.axis) },
    ],
    series: [
      { value: (u, v) => (v == null ? "–" : new Date(v * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })) },
      ...data.series.map((s, i) => ({
        ...lineLook(s, i),
        width: 2,
        points: { show: false },
        value: (u, v) => (v == null ? "–" : spec.value(v)),
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
