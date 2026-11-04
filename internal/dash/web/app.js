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
  $("pick").hidden = true;
  $("pick").open = false;
  $("node-pick").hidden = true;
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

// The view, the pools, the volumes picked and the host shown are in the URL
// hash (#hosts&pool=pool1&host=pool1/10.0.1.10), so a reload keeps them.
function hashParams() {
  const [view, ...rest] = location.hash.replace(/^#/, "").split("&");
  const q = new URLSearchParams(rest.join("&"));
  return { view, pool: q.get("pool") || "all", vols: q.get("vols") || "", host: q.get("host") || "" };
}

const VIEWS = ["overview", "hosts", "cache", "volumes", "snapshots"];

function currentView() {
  const v = hashParams().view;
  return v === "storage" ? "volumes" : VIEWS.includes(v) ? v : "overview";
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

// setHash goes to view, changing the hash parameters in patch and keeping
// the rest.
function setHash(view, patch = {}) {
  const h = { ...hashParams(), ...patch };
  const list = (v) => v.split(",").map(encodeURIComponent).join(",");
  const parts = [view];
  if (h.pool && h.pool !== "all") parts.push(`pool=${list(h.pool)}`);
  if (h.vols) parts.push(`vols=${list(h.vols)}`);
  if (h.host) parts.push(`host=${encodeURIComponent(h.host)}`);
  location.hash = parts.join("&");
}

function poolQuery() {
  return `pool=${allPicked() ? "all" : selectedPools().map(encodeURIComponent).join(",")}`;
}

const nodeKey = (n) => `${n.pool}/${n.ip}`;
const volumeKey = (v) => `${v.pool}/${v.name}`;

// A host's name, with its pool and IP unless short.
function hostLabel(n, short) {
  if (short) return n.hostname || n.ip;
  return `${n.hostname || n.ip} (${multiPool() ? n.pool + ", " : ""}${n.ip})`;
}

// The header picker narrows the Volumes view to some volumes, as Grafana's
// $volume variable does, and keeps the choice in the hash.
const PICKS = {
  volumes: {
    param: "vols", title: "Volumes", all: "All volumes",
    items: () => state.storage?.volumes || [], key: volumeKey,
    label: (v, short) => (short || !multiPool() ? v.name : `${v.name} (${v.pool})`),
  },
};

// The items picked for a view, in table order; every item when none is
// picked (or none picked is still listed).
function picked(view) {
  const c = PICKS[view];
  const items = c.items();
  const want = new Set(hashParams()[c.param].split(","));
  const out = items.filter((i) => want.has(c.key(i)));
  return out.length ? out : items;
}

function allItemsPicked(view) {
  return picked(view).length === PICKS[view].items().length;
}


// Lines of a chart narrowed to the view's picked items, matched by keyOf.
function onlyPicked(view, data, keyOf) {
  if (!allItemsPicked(view)) {
    const want = new Set(picked(view).map(PICKS[view].key));
    data.series = (data.series || []).filter((s) => want.has(keyOf(s)));
  }
  return data;
}

document.querySelectorAll("#tabs a").forEach((a) =>
  a.addEventListener("click", (e) => {
    e.preventDefault();
    setHash(a.dataset.view);
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
  const all = pools.length === 0 || pools.length === state.pools.length;
  setHash(currentView(), { pool: all ? "all" : pools.join(",") });
});

$("pick-menu").addEventListener("change", (e) => {
  const view = currentView();
  const c = PICKS[view];
  const keys = e.target.value === "*" ? [] : [...$("pick-menu").querySelectorAll("input[data-item]:checked")].map((b) => b.value);
  // As with pools: none or every item ticked is all of them.
  const all = keys.length === 0 || keys.length === c.items().length;
  setHash(view, { [c.param]: all ? "" : keys.join(",") });
});

// Close a menu on a click outside it.
document.addEventListener("click", (e) => {
  for (const id of ["pool-pick", "pick"]) if (!$(id).contains(e.target)) $(id).open = false;
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

function renderPick() {
  const view = currentView();
  const c = PICKS[view];
  const items = c ? c.items() : [];
  $("pick").hidden = !c || items.length === 0 || $("main-view").hidden;
  if ($("pick").hidden) return;
  const chosen = picked(view);
  const keys = new Set(chosen.map(c.key));
  const all = allItemsPicked(view);
  $("pick-title").textContent = c.title;
  $("pick-menu").setAttribute("aria-label", c.title);
  $("pick-menu").innerHTML =
    `<label class="all"><input type="checkbox" value="*" ${all ? "checked disabled" : ""}> ${c.all}</label>` +
    items.map((i) => `<label><input type="checkbox" data-item value="${esc(c.key(i))}" ${keys.has(c.key(i)) ? "checked" : ""}> ${esc(c.label(i))}</label>`).join("");
  $("pick-label").textContent = all ? "All" : chosen.length === 1 ? c.label(chosen[0], true) : `${chosen.length} of ${items.length}`;
}

function applyView() {
  renderPools();
  renderPick();
  $("node-pick").hidden = currentView() !== "hosts" || $("main-view").hidden;
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
    } else if (view === "volumes") {
      state.storage = await api(`/api/volumes?${poolQuery()}`);
      renderStorage(state.storage);
      const rows = [...document.querySelectorAll("#view-volumes details.row[open]")];
      await Promise.all(rows.map(renderPanelRow));
    } else if (view === "snapshots") {
      renderSnapshots(await api(`/api/snapshots?${poolQuery()}`));
    } else if (view === "hosts") {
      state.overview = await api(`/api/overview?${poolQuery()}`);
      warnings(state.overview.warnings);
      await renderHost();
    } else {
      state.overview = await api(`/api/overview?${poolQuery()}`);
      renderOverview(state.overview);
      await renderCharts(["cpu", "ram"]);
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

function warnings(list) {
  $("warnings").innerHTML = (list || []).map((w) => `<div>${esc(w)}</div>`).join("");
}

// The Overview: the "Core" dashboard, the cluster's view of its nodes.
function renderOverview(ov) {
  warnings(ov.warnings);
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
  $("ov-kpis").innerHTML = tiles.join("");

  $("ov-nodes").innerHTML = nodes.map((n) => `<tr>
    <td><span class="swatch" style="background:${nodeColor(n.pool, n.ip)}"></span></td>
    <td class="col-pool">${esc(n.pool)}</td>
    <td><div class="name">${esc(n.hostname || n.ip)}</div><div class="ip">${esc(n.ip)}</div></td>
    <td>${clusterStatus(n)}</td>
    <td>${service(n, "core")}</td>
    <td>${service(n, "cache")}</td>
    <td>${service(n, "storage")}</td>
    <td class="mono">${esc(n.uuid || "–")}</td>
  </tr>`).join("") || `<tr><td colspan="8" class="dim">No nodes report to Prometheus.</td></tr>`;
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
  warnings(st.warnings);
  renderPick();
  const vols = picked("volumes");
  const total = (k) => vols.reduce((a, v) => a + (v[k] || 0), 0);
  const notReady = vols.filter((v) => !v.ready).length;
  $("storage-kpis").innerHTML = [
    kpi("Volumes", vols.length, vols.length ? "" : "none reported"),
    kpi("Not ready", notReady, notReady ? "see the table" : "none", notReady ? "bad" : "good"),
    kpi("Provisioned", size(total("size_mib")), allItemsPicked("volumes") ? "all volumes" : "picked volumes"),
    kpi("RW cache", size(total("rw_mib")), "write-back"),
    kpi("Read cache", size(total("r_mib")), ""),
  ].join("");

  $("volumes").innerHTML = vols.map((v) => `<tr>
    <td><span class="swatch" style="background:${slotColor(volumeIndex(v.pool, v.name))}"></span></td>
    <td class="col-pool">${esc(v.pool)}</td>
    <td class="name">${esc(v.name)}</td>
    <td>${check(v.ready, v.status || "READY", v.status || "unknown")}</td>
    <td class="num">${size(v.size_mib)}</td>
    <td class="num">${size(v.rw_mib)}</td>
    <td class="num">${size(v.r_mib)}</td>
    <td>${qosCell(v.qos)}</td>
    <td>${paramsCell(v.extra_params)}</td>
    <td>${v.node ? nodeCell(v) : status("–", "none")}</td>
    <td class="mono">${esc(v.bucket || "–")}</td>
  </tr>`).join("") || `<tr><td colspan="11" class="dim">No volumes reported.</td></tr>`;
}

// A volume's QoS caps, as the StorageClass names them, in the hint.
const QOS = [
  ["r_mbytes_per_sec", "Read", "MB/s"],
  ["w_mbytes_per_sec", "Write", "MB/s"],
  ["rw_mbytes_per_sec", "Read or write", "MB/s"],
  ["rw_ios_per_sec", "IOPS, read + write", "IOPS"],
];

// qosCell shows the combined caps, with every cap in its hint.
function qosCell(q) {
  if (!q) return `<span class="dim">none</span>`;
  const short = [];
  if (q.rw_mbytes_per_sec) short.push(`${q.rw_mbytes_per_sec} MB/s`);
  else if (q.r_mbytes_per_sec || q.w_mbytes_per_sec) short.push(`R ${q.r_mbytes_per_sec || "–"} / W ${q.w_mbytes_per_sec || "–"} MB/s`);
  if (q.rw_ios_per_sec) short.push(`${q.rw_ios_per_sec} IOPS`);
  const hint = QOS.map(([k, label, unit]) => `${label}: ${q[k] ? `${q[k]} ${unit}` : "no cap"}`).join("\n");
  return `<span class="hint" data-hint="${esc(hint)}">${esc(short.join(" · ") || "set")}</span>`;
}

// paramsCell counts the nbdkit arguments, one per line in the hint.
function paramsCell(p) {
  if (!p) return `<span class="dim">none</span>`;
  const args = p.split(/\s+(?=--)/).filter(Boolean);
  return `<span class="hint" data-hint="${esc(args.join("\n"))}">${args.length} ${args.length === 1 ? "param" : "params"}</span>`;
}

// A hint follows the element it explains. It hangs off the page, not the
// element, so the table's scroll box does not clip it.
const hintPop = document.createElement("div");
hintPop.className = "hint-pop";
hintPop.hidden = true;
document.body.appendChild(hintPop);
document.addEventListener("mouseover", (e) => {
  const el = e.target.closest?.(".hint");
  if (!el) {
    hintPop.hidden = true;
    return;
  }
  hintPop.textContent = el.dataset.hint;
  hintPop.hidden = false;
  const r = el.getBoundingClientRect();
  const w = hintPop.offsetWidth;
  hintPop.style.left = Math.max(8, Math.min(r.left, innerWidth - w - 8)) + "px";
  hintPop.style.top = r.bottom + 6 + "px";
});
document.addEventListener("scroll", () => (hintPop.hidden = true), true);

// --- snapshots ----------------------------------------------------------

function renderSnapshots(sv) {
  warnings(sv.warnings);
  const m = sv.summary;
  const count = (label, v, sub) => kpi(label, v ?? 0, sub || "", v ? "bad" : "good");
  $("snapshot-kpis").innerHTML = [
    kpi("Snapshots", m.snapshots, m.snapshots ? "" : "none reported"),
    kpi("Ready", m.ready, ""),
    count("Not ready", m.not_ready, m.not_ready ? "see the table" : "none"),
    kpi("Logical size", bytes(m.logical_bytes), "all snapshots"),
    count("Transfer errors", m.transfer_errors),
    count("Fatal errors", m.fatal_errors, "jobs"),
    count("Retry errors", m.retry_errors, "jobs"),
    kpi("Server-side copy", pct(m.server_side_share), "of the bytes sent"),
    kpi("Server-side copied", bytes(m.server_side_bytes), ""),
    kpi("Elapsed", m.avg_elapsed == null ? "–" : seconds(m.avg_elapsed), "average"),
    kpi("Transfer time", m.avg_transfer_time == null ? "–" : seconds(m.avg_transfer_time), "average"),
    kpi("Transfers", m.avg_total_transfers == null ? "–" : m.avg_total_transfers.toFixed(1), "average per snapshot"),
    kpi("Speed", m.avg_speed == null ? "–" : bytes(m.avg_speed, "/s"), "average"),
  ].join("");

  $("snapshots").innerHTML = sv.snapshots.map((s) => `<tr>
    <td class="col-pool">${esc(s.pool)}</td>
    <td class="name">${esc(s.name)}</td>
    <td>${check(s.ready, s.status || "READY", s.status || "unknown")}</td>
    <td>${esc(s.kind || "–")}</td>
    <td>${esc(s.incremental || "–")}</td>
    <td>${esc(s.volume || "–")}</td>
    <td>${s.node ? nodeCell(s) : status("–", "none")}</td>
    <td>${esc(s.storage_pool || "–")}</td>
  </tr>`).join("") || `<tr><td colspan="8" class="dim">No snapshots reported.</td></tr>`;
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

// The Hosts view's nodes: those of the selected pools that node_exporter
// reports (they have a kernel), or every node when none does.
function hostNodes() {
  const nodes = state.overview?.nodes || [];
  const reporting = nodes.filter((n) => n.kernel);
  return reporting.length ? reporting : nodes;
}

// The host in the hash, if it is still listed; else the first.
function selectedHost() {
  const nodes = hostNodes();
  const want = hashParams().host;
  const n = nodes.find((n) => nodeKey(n) === want) || nodes[0];
  return n ? { pool: n.pool, ip: n.ip, node: n } : null;
}

$("host-pick").addEventListener("change", (e) => setHash("hosts", { host: e.target.value }));

// Rows load their charts when opened, so closed ones cost nothing.
document.querySelectorAll("details.row").forEach((d) =>
  d.addEventListener("toggle", () => {
    if (d.open && d.dataset.row !== "quick") renderPanelRow(d).catch((err) => err.auth && showLogin());
  }),
);

function hostQuery(h) {
  return `pool=${encodeURIComponent(h.pool)}&ip=${encodeURIComponent(h.ip)}`;
}

async function renderHost() {
  const h = selectedHost();
  $("host-pick").innerHTML = hostNodes().map((n) => {
    const v = nodeKey(n);
    return `<option value="${esc(v)}" ${h && v === `${h.pool}/${h.ip}` ? "selected" : ""}>${esc(hostLabel(n))}</option>`;
  }).join("");
  $("host-name").textContent = h ? h.node.hostname || h.ip : "No host";
  $("host-sub").textContent = h ? [h.node.pool, h.ip, h.node.kernel && `kernel ${h.node.kernel}`].filter(Boolean).join(" · ") : "";
  if (!h) {
    $("host-quick").innerHTML = `<div class="dim">No hosts report to Prometheus.</div>`;
    return;
  }
  const rows = [...document.querySelectorAll("#view-hosts details.row[open]")].filter((d) => d.dataset.row !== "quick");
  await Promise.all([renderQuick(h), ...rows.map(renderPanelRow)]);
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

// renderPanelRow draws a row's panels: the selected host's (hp-) or the
// picked volumes' (vp-).
async function renderPanelRow(d) {
  const h = selectedHost();
  await Promise.all([...d.querySelectorAll(".chart")].map(async (el) => {
    const id = el.id.replace(/^chart-/, "");
    // A panel shown in two rows, as in Grafana, names it in data-panel.
    const panel = el.dataset.panel || id.slice(3);
    try {
      if (id.startsWith("vp-")) {
        const data = await api(`/api/volume/series?panel=${panel}&range=${state.range}&${poolQuery()}`);
        drawChart(id, onlyPicked("volumes", data, (s) => `${s.pool}/${s.volume}`));
      } else if (h) {
        drawChart(id, await api(`/api/host/series?${hostQuery(h)}&panel=${panel}&range=${state.range}`));
      }
    } catch (err) {
      if (err.auth) throw err;
      emptyChart(id, err.message);
    }
  }));
}

// --- charts -------------------------------------------------------------

// How each chart scales and prints its values.
const fmtRate = (v, d) => (v >= 100 ? v.toFixed(0) : v.toFixed(d));
const percent = { max: 100, axis: (v) => v + "%", value: (v) => v.toFixed(1) + "%" };
const CHARTS = { cpu: percent, ram: percent };
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

// A node's line, or a volume panel's one line per volume, takes its name
// and color from its row in the view's table; other panel lines are colored
// in the panel's order.
function lineLook(s, i) {
  if (s.volume && s.label === s.volume) return { label: s.label, stroke: slotColor(volumeIndex(s.pool, s.volume)) };
  if (s.label) return { label: s.label, stroke: slotColor(i) };
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
      const data = await api(`/api/series?metric=${m}&range=${state.range}&${poolQuery()}`);
      drawChart(m, data);
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
    return emptyChart(metric, "No data yet.");
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
  const looks = data.series.map((s, i) => lineLook(s, i));
  const u = new uPlot({
    width: el.clientWidth || 600,
    height: 220,
    padding: [8, 8, 0, 0],
    cursor: { points: { size: 8 }, drag: { x: false, y: false } },
    legend: { live: true },
    plugins: [tooltip(looks, spec)],
    scales: { y: { range: (u, lo, hi) => [0, spec.max ?? Math.max((hi || 0) * 1.1, 1)] } },
    axes: [
      { ...axis, space: 80 },
      { ...axis, size: spec.size || 52, values: (u, vals) => vals.map(spec.axis) },
    ],
    series: [
      { value: (u, v) => (v == null ? "–" : new Date(v * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })) },
      ...data.series.map((s, i) => ({
        ...looks[i],
        width: 2,
        points: { show: false },
        value: (u, v) => (v == null ? "–" : spec.value(v)),
      })),
    ],
  }, aligned, el);
  u._key = key;
  state.charts[metric] = u;
}

// TIP_ROWS caps the hover tooltip; the line nearest the cursor is always in it.
const TIP_ROWS = 12;

// tooltip shows, at the cursor, the time and each line's name and value,
// highest first, with the line nearest the cursor in bold: Grafana's
// "all series" tooltip.
function tooltip(looks, spec) {
  let tip;
  return {
    hooks: {
      init: (u) => {
        tip = document.createElement("div");
        tip.className = "chart-tip";
        tip.hidden = true;
        u.over.appendChild(tip);
        u.over.addEventListener("mouseleave", () => (tip.hidden = true));
      },
      setCursor: (u) => {
        const { left, top, idx } = u.cursor;
        if (idx == null || left == null || left < 0) {
          tip.hidden = true;
          return;
        }
        let rows = [];
        for (let i = 1; i < u.series.length; i++) {
          const v = u.data[i][idx];
          if (u.series[i].show && v != null) rows.push({ look: looks[i - 1], v, d: Math.abs(u.valToPos(v, "y") - top) });
        }
        if (rows.length === 0) {
          tip.hidden = true;
          return;
        }
        const near = rows.reduce((a, b) => (b.d < a.d ? b : a));
        rows.sort((a, b) => b.v - a.v);
        let more = 0;
        if (rows.length > TIP_ROWS) {
          more = rows.length - TIP_ROWS;
          rows = rows.slice(0, TIP_ROWS);
          if (!rows.includes(near)) {
            rows[TIP_ROWS - 1] = near;
          }
        }
        const t = new Date(u.data[0][idx] * 1000).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
        tip.innerHTML = `<div class="tip-time">${esc(t)}</div>` + rows.map((r) =>
          `<div class="tip-row${r === near ? " near" : ""}"><span class="tip-sw" style="background:${r.look.stroke}"></span>` +
          `<span class="tip-label">${esc(r.look.label)}</span><span class="tip-v">${esc(spec.value(r.v))}</span></div>`,
        ).join("") + (more ? `<div class="tip-more">+${more} more</div>` : "");
        tip.hidden = false;
        // Right of the cursor, or left of it near the right edge.
        const w = tip.offsetWidth, h = tip.offsetHeight, ow = u.over.clientWidth, oh = u.over.clientHeight;
        tip.style.left = (left + 16 + w > ow ? Math.max(0, left - 16 - w) : left + 16) + "px";
        tip.style.top = Math.max(0, Math.min(top - h / 2, oh - h)) + "px";
      },
    },
  };
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
