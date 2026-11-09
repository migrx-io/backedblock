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

// Color follows the node: its place among the nodes picked in the Overview
// table, as only they have lines. Past eight nodes the rest are drawn in
// muted ink and stay identified by the legend and the table.
function nodeIndex(pool, ip) {
  return pickedNodes().indexOf(`${pool}/${ip}`);
}

// pickedNodes are the nodes ticked in the Overview table (nodes in the
// hash), in the order they were ticked.
function pickedNodes() {
  return hashParams().nodes.split(",").filter(Boolean);
}

function nodeColor(pool, ip) {
  return slotColor(nodeIndex(pool, ip));
}

function nodeName(pool, ip) {
  const n = state.overview?.nodes.find((n) => n.pool === pool && n.ip === ip);
  return n?.hostname || ip;
}

// A volume's color is its place among the picked volumes: only they have
// lines on the charts.
function volumeIndex(pool, name) {
  return pickedVolumes().indexOf(`${pool}/${name}`);
}

// pickedVolumes are the volumes ticked in the Volumes table (vols in the
// hash), in the order they were ticked.
function pickedVolumes() {
  return hashParams().vols.split(",").filter(Boolean);
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
  $("node-pick").hidden = true;
  $("node-pick").open = false;
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
  return { view, pool: q.get("pool") || "all", vols: q.get("vols") || "", nodes: q.get("nodes") || "", cnodes: q.get("cnodes") || "", host: q.get("host") || "", from: q.get("from") || "", to: q.get("to") || "" };
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
  location.hash = hashFor(view, patch);
}

// hashFor is the hash for view with patch over the current parameters.
function hashFor(view, patch = {}) {
  const h = { ...hashParams(), ...patch };
  const list = (v) => v.split(",").map(encodeURIComponent).join(",");
  const parts = [view];
  if (h.pool && h.pool !== "all") parts.push(`pool=${list(h.pool)}`);
  if (h.vols) parts.push(`vols=${list(h.vols)}`);
  if (h.nodes) parts.push(`nodes=${list(h.nodes)}`);
  if (h.cnodes) parts.push(`cnodes=${list(h.cnodes)}`);
  if (h.host) parts.push(`host=${encodeURIComponent(h.host)}`);
  if (h.from && h.to) parts.push(`from=${h.from}`, `to=${h.to}`);
  return parts.join("&");
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

document.querySelectorAll("#tabs a").forEach((a) =>
  a.addEventListener("click", (e) => {
    e.preventDefault();
    setHash(a.dataset.view);
  }),
);

// Unticking "All" clears a menu's boxes, to tick just the items wanted. The
// choice changes only when an item is ticked, so the menu stays cleared,
// even across refreshes, until then or until it closes.
const cleared = new Set();

function clearMenu(id) {
  cleared.add(id);
  $(id).querySelectorAll("input[type=checkbox]").forEach((b) => (b.checked = false));
}

function keepCleared(id) {
  if (cleared.has(id)) $(id).querySelectorAll("input[type=checkbox]").forEach((b) => (b.checked = false));
}

$("pool-menu").addEventListener("change", (e) => {
  const box = e.target;
  if (box.value === "*" && !box.checked) return clearMenu("pool-menu");
  cleared.delete("pool-menu");
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


// Close a menu on a click outside it.
document.addEventListener("click", (e) => {
  for (const id of ["pool-pick", "node-pick"]) if (!$(id).contains(e.target)) $(id).open = false;
});

function renderPools() {
  $("pool-pick").hidden = state.pools.length === 0 || $("main-view").hidden;
  const picked = new Set(selectedPools());
  const all = allPicked();
  $("pool-menu").innerHTML =
    `<label class="all"><input type="checkbox" value="*" ${all ? "checked" : ""}> All pools</label>` +
    state.pools.map((p) => `<label><input type="checkbox" data-pool value="${esc(p)}" ${picked.has(p) ? "checked" : ""}> ${esc(p)}</label>`).join("");
  $("pool-label").textContent = all ? "All" : [...picked].join(", ");
  keepCleared("pool-menu");
  searchMenu($("pool-pick"));
  document.body.classList.toggle("multi-pool", picked.size > 1);
}


// searchMenu shows the menu's items that contain its search text, any case;
// the "All" item stays. It runs on typing and after each re-render, which
// keeps the text.
function searchMenu(d) {
  const q = d.querySelector(".pick-search").value.trim().toLowerCase();
  const list = d.querySelector(".menu-list");
  let shown = 0;
  list.querySelectorAll("label:not(.all)").forEach((l) => {
    l.hidden = q !== "" && !l.textContent.toLowerCase().includes(q);
    if (!l.hidden) shown++;
  });
  list.querySelector(".none")?.remove();
  if (q && !shown) list.insertAdjacentHTML("beforeend", `<div class="none">No match</div>`);
}

document.querySelectorAll(".pool-pick").forEach((d) => {
  const search = d.querySelector(".pick-search");
  search.addEventListener("input", () => searchMenu(d));
  // Enter picks the only match, Escape clears the search or closes the menu.
  search.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      const shown = [...d.querySelectorAll(".menu-list label:not(.all):not([hidden]) input")];
      if (shown.length === 1) shown[0].click();
    } else if (e.key === "Escape") {
      if (search.value) {
        search.value = "";
        searchMenu(d);
      } else {
        d.open = false;
      }
    }
  });
  // Opening a menu starts with an empty search, ready to type; closing a
  // cleared one shows the choice it kept.
  d.addEventListener("toggle", () => {
    if (!d.open) {
      const list = d.querySelector(".menu-list");
      if (cleared.delete(list.id)) renderPools();
      return;
    }
    search.value = "";
    searchMenu(d);
    search.focus();
  });
});

function applyView() {
  renderRange();
  renderPools();
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
      state.cache = await api(`/api/cache?${poolQuery()}`);
      renderCache(state.cache);
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

  renderNodeRows();
}

// renderNodeRows lists the picked nodes first, then the others that match
// the search, up to TABLE_ROWS.
function renderNodeRows() {
  const nodes = state.overview?.nodes || [];
  const picks = pickedNodes();
  const pickedSet = new Set(picks);
  const q = $("ov-search").value.trim().toLowerCase();
  const bad = $("ov-bad").checked;
  const matches = (n) => (!bad || nodeDown(n)) && (!q || [n.pool, n.hostname, n.ip, n.uuid, n.status].some((x) => x && x.toLowerCase().includes(q)));
  const byKey = new Map(nodes.map((n) => [nodeKey(n), n]));
  const top = picks.map((k) => byKey.get(k)).filter(Boolean);
  const rest = nodes.filter((n) => !pickedSet.has(nodeKey(n)) && matches(n));
  $("ov-count").textContent = rowCount(top.length, rest.length, q || bad);
  $("ov-clear").hidden = picks.length === 0;
  $("ov-nodes").innerHTML = [...top, ...rest.slice(0, TABLE_ROWS)].map((n) => {
    const key = nodeKey(n);
    const on = pickedSet.has(key);
    return `<tr class="pickable${on ? " picked" : ""}" data-key="${esc(key)}">
    <td><input type="checkbox" ${on ? "checked" : ""} aria-label="Show ${esc(n.hostname || n.ip)} on the charts"></td>
    <td>${on ? `<span class="swatch" style="background:${nodeColor(n.pool, n.ip)}"></span>` : ""}</td>
    <td class="col-pool">${esc(n.pool)}</td>
    <td><div class="name">${hostLink(n.pool, n.ip, n.hostname || n.ip)}</div><div class="ip">${esc(n.ip)}</div></td>
    <td>${clusterStatus(n)}</td>
    <td>${service(n, "core")}</td>
    <td>${service(n, "cache")}</td>
    <td>${service(n, "storage")}</td>
    <td class="mono">${esc(n.uuid || "–")}</td>
  </tr>`;
  }).join("") || `<tr><td colspan="9" class="dim">${nodes.length ? "No node matches." : "No nodes report to Prometheus."}</td></tr>`;
}

// nodeDown is a node to look at: not UP in its cluster, not reporting, or
// with a service Prometheus does not reach.
function nodeDown(n) {
  return n.status !== "UP" || Object.values(n.services || {}).includes(false);
}

// rowCount is a picking table's count: its picked rows and how many of the
// others it shows.
function rowCount(picked, rest, q) {
  const shown = rest > TABLE_ROWS ? `showing ${TABLE_ROWS} of ${rest}${q ? " matches" : ""}, search to narrow` : `${rest}${q ? " matching" : ""}`;
  return `${picked ? `${picked} picked · ` : ""}${shown}`;
}

// pickRows sets a picking table's picks (param in the hash) without a
// hashchange, so the table is not fetched again; then redraws.
function pickRows(param, keys, redraw) {
  history.replaceState(null, "", "#" + hashFor(currentView(), { [param]: keys.join(",") }));
  redraw();
}

// onPick makes a table's rows toggle their key among the picks in param.
function onPick(tbody, param, picks, redraw) {
  $(tbody).addEventListener("click", (e) => {
    const tr = e.target.closest("tr[data-key]");
    // Hints and links in a row keep their own clicks.
    if (!tr || e.target.closest(".hint, a")) return;
    const key = tr.dataset.key;
    const now = picks();
    pickRows(param, now.includes(key) ? now.filter((k) => k !== key) : [...now, key], redraw);
  });
}

function redrawNodes() {
  renderNodeRows();
  renderCharts(["cpu", "ram"]).catch((err) => err.auth && showLogin());
}

onPick("ov-nodes", "nodes", pickedNodes, redrawNodes);
$("ov-search").addEventListener("input", renderNodeRows);
$("ov-bad").addEventListener("change", renderNodeRows);
$("ov-clear").addEventListener("click", () => pickRows("nodes", [], redrawNodes));

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
// size prints MiB as GiB, TiB or PiB.
function size(mib) {
  if (mib == null) return "–";
  const gib = mib / 1024;
  if (gib < 1024) return `${gib.toFixed(gib >= 100 ? 0 : 1)} GiB`;
  const tib = gib / 1024;
  if (tib < 1024) return `${tib.toFixed(tib >= 100 ? 1 : 2)} TiB`;
  return `${(tib / 1024).toFixed(2)} PiB`;
}

function check(ok, good = "ok", bad = "failed") {
  return ok ? status(good, "good") : status(bad, "bad");
}

// nodeCell is a node's name, linked to its Hosts view, and its IP.
function nodeCell(n) {
  return `<div class="name">${hostLink(n.pool, n.node, n.hostname || n.node)}</div>${n.ip ? `<div class="ip">${esc(n.ip)}</div>` : ""}`;
}

// hostLink opens a node in the Hosts view. id is its management IP or its
// UUID: tables of cache nodes and snapshots know only the UUID.
function hostLink(pool, id, text) {
  if (!id) return esc(text);
  return `<a class="host-link" href="#${esc(hashFor("hosts", { host: `${pool}/${id}` }))}" title="Open in Hosts">${esc(text)}</a>`;
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

  renderCacheRows();
}

// cacheKey names a cache node, and its RAID and mounts, by pool and UUID.
const cacheKey = (x) => `${x.pool}/${x.node}`;

// pickedCacheNodes are the nodes ticked in the Cache nodes table (cnodes in
// the hash), in the order they were ticked.
function pickedCacheNodes() {
  return hashParams().cnodes.split(",").filter(Boolean);
}

// renderCacheRows lists the picked cache nodes first, then the others that
// match the search, up to TABLE_ROWS; the RAID and mount tables show only
// the picked nodes'.
function renderCacheRows() {
  const c = state.cache || { nodes: [], raids: [], mounts: [] };
  const picks = pickedCacheNodes();
  const pickedSet = new Set(picks);
  // A node is down when its own checks fail or its RAID or a mount does.
  const badNodes = new Set([...c.raids.filter((r) => !(r.state_ok && r.ok && r.no_failed)), ...c.mounts.filter((m) => !m.ok)].map(cacheKey));
  const down = (n) => !(n.status && n.mounts && n.errors) || badNodes.has(cacheKey(n));
  const q = $("cache-search").value.trim().toLowerCase();
  const bad = $("cache-bad").checked;
  const matches = (n) => (!bad || down(n)) && (!q || [n.pool, n.hostname, n.ip, n.node].some((x) => x && x.toLowerCase().includes(q)));
  const byKey = new Map(c.nodes.map((n) => [cacheKey(n), n]));
  const top = picks.map((k) => byKey.get(k)).filter(Boolean);
  const rest = c.nodes.filter((n) => !pickedSet.has(cacheKey(n)) && matches(n));
  $("cache-count").textContent = rowCount(top.length, rest.length, q || bad);
  $("cache-clear").hidden = picks.length === 0;
  $("cache-nodes").innerHTML = [...top, ...rest.slice(0, TABLE_ROWS)].map((n) => {
    const key = cacheKey(n);
    const on = pickedSet.has(key);
    return `<tr class="pickable${on ? " picked" : ""}" data-key="${esc(key)}">
    <td><input type="checkbox" ${on ? "checked" : ""} aria-label="Show ${esc(n.hostname || n.ip)}'s RAID and mounts"></td>
    <td class="col-pool">${esc(n.pool)}</td>
    <td>${nodeCell(n)}</td>
    <td>${check(n.status, "up", "down")}</td>
    <td>${check(n.mounts, "attached", "detached")}</td>
    <td>${check(n.errors, "passing", "failing")}</td>
    <td class="num">${n.disks}</td>
    <td class="num">${size(n.rw_mib)}</td>
    <td class="num">${size(n.r_mib)}</td>
  </tr>`;
  }).join("") || `<tr><td colspan="9" class="dim">${c.nodes.length ? "No cache node matches." : "No cache nodes reported."}</td></tr>`;

  const pick = (what) => `<tr><td colspan="7" class="dim">Pick nodes in the Cache nodes table to see their ${what}.</td></tr>`;
  const none = (what) => `<tr><td colspan="7" class="dim">No ${what} reported for the picked nodes.</td></tr>`;
  const raids = c.raids.filter((r) => pickedSet.has(cacheKey(r)));
  $("cache-raids").innerHTML = !picks.length ? pick("RAID") : raids.map((r) => {
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
  }).join("") || none("RAID");

  const mounts = c.mounts.filter((m) => pickedSet.has(cacheKey(m)));
  $("cache-mounts").innerHTML = !picks.length ? pick("mounts") : mounts.map((m) => `<tr>
    <td class="col-pool">${esc(m.pool)}</td>
    <td>${nodeCell(m)}</td>
    <td>${m.kind === "rw" ? "read-write" : m.kind === "r" ? "read" : esc(m.kind)}</td>
    <td class="mono">${esc(m.mountpoint)}</td>
    <td class="mono">${esc(m.device)}</td>
    <td class="mono">${esc(m.options)}</td>
    <td>${check(m.ok, "mounted", "down")}</td>
  </tr>`).join("") || none("mounts");
}

onPick("cache-nodes", "cnodes", pickedCacheNodes, renderCacheRows);
$("cache-search").addEventListener("input", renderCacheRows);
$("cache-bad").addEventListener("change", renderCacheRows);
$("cache-clear").addEventListener("click", () => pickRows("cnodes", [], renderCacheRows));

// --- storage ------------------------------------------------------------

// TABLE_ROWS caps the big tables (nodes, cache nodes, volumes, snapshots):
// the picked rows, then this many of the others that match the search. Tens
// of thousands of rows would stall the page.
const TABLE_ROWS = 200;

function renderStorage(st) {
  warnings(st.warnings);
  const vols = st.volumes;
  const total = (k) => vols.reduce((a, v) => a + (v[k] || 0), 0);
  const notReady = vols.filter((v) => !v.ready).length;
  $("storage-kpis").innerHTML = [
    kpi("Volumes", vols.length, vols.length ? "" : "none reported"),
    kpi("Not ready", notReady, notReady ? "see the table" : "none", notReady ? "bad" : "good"),
    kpi("Provisioned", size(total("size_mib")), "all volumes"),
    kpi("RW cache", size(total("rw_mib")), "write-back"),
    kpi("Read cache", size(total("r_mib")), ""),
  ].join("");
  renderVolumeRows();
}

// renderVolumeRows lists the picked volumes first, then the others that
// match the search, up to TABLE_ROWS.
function renderVolumeRows() {
  const vols = state.storage?.volumes || [];
  const picks = pickedVolumes();
  const pickedSet = new Set(picks);
  const q = $("vol-search").value.trim().toLowerCase();
  const bad = $("vol-bad").checked;
  const matches = (v) => (!bad || !v.ready) && (!q || [v.name, v.pool, v.hostname, v.ip, v.bucket, v.status].some((x) => x && x.toLowerCase().includes(q)));
  const byKey = new Map(vols.map((v) => [volumeKey(v), v]));
  const top = picks.map((k) => byKey.get(k)).filter(Boolean);
  const rest = vols.filter((v) => !pickedSet.has(volumeKey(v)) && matches(v));
  const shown = [...top, ...rest.slice(0, TABLE_ROWS)];
  $("vol-count").textContent = rowCount(top.length, rest.length, q || bad);
  $("vol-clear").hidden = picks.length === 0;
  $("volumes").innerHTML = shown.map((v) => {
    const key = volumeKey(v);
    const on = pickedSet.has(key);
    return `<tr class="pickable${on ? " picked" : ""}" data-key="${esc(key)}">
    <td><input type="checkbox" ${on ? "checked" : ""} aria-label="Show ${esc(v.name)} on the charts"></td>
    <td>${on ? `<span class="swatch" style="background:${slotColor(volumeIndex(v.pool, v.name))}"></span>` : ""}</td>
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
  </tr>`;
  }).join("") || `<tr><td colspan="12" class="dim">${vols.length ? "No volume matches." : "No volumes reported."}</td></tr>`;
}

function redrawVolumes() {
  renderVolumeRows();
  document.querySelectorAll("#view-volumes details.row[open]").forEach((d) => renderPanelRow(d).catch((err) => err.auth && showLogin()));
}

onPick("volumes", "vols", pickedVolumes, redrawVolumes);
$("vol-search").addEventListener("input", renderVolumeRows);
$("vol-bad").addEventListener("change", renderVolumeRows);
$("vol-clear").addEventListener("click", () => pickRows("vols", [], redrawVolumes));

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
// element, so the table's scroll box does not clip it. The pointer can move
// into it, and a long one scrolls; a click on the element keeps it open
// until a click elsewhere or Escape.
const hintPop = document.createElement("div");
hintPop.className = "hint-pop";
hintPop.hidden = true;
document.body.appendChild(hintPop);
let hintFor = null;
let hintPinned = false;
let hintTimer = null;

function showHint(el) {
  clearTimeout(hintTimer);
  if (hintFor === el && !hintPop.hidden) return;
  hintFor = el;
  hintPop.textContent = el.dataset.hint;
  hintPop.style.maxHeight = "";
  hintPop.hidden = false;
  hintPop.scrollTop = 0;
  // Below the element if it fits, else on the side with more room, as high
  // as that room and scrolling past it.
  const r = el.getBoundingClientRect();
  const below = innerHeight - r.bottom - 14;
  const above = r.top - 14;
  const h = hintPop.offsetHeight;
  const down = h <= below || below >= above;
  hintPop.style.maxHeight = Math.max(80, down ? below : above) + "px";
  hintPop.style.left = Math.max(8, Math.min(r.left, innerWidth - hintPop.offsetWidth - 8)) + "px";
  hintPop.style.top = (down ? r.bottom + 6 : r.top - 6 - hintPop.offsetHeight) + "px";
}

function hideHint() {
  clearTimeout(hintTimer);
  hintPop.hidden = true;
  hintFor = null;
  hintPinned = false;
}

document.addEventListener("mouseover", (e) => {
  const el = e.target.closest?.(".hint");
  if (el && !hintPinned) return showHint(el);
  if (hintPop.contains(e.target)) return clearTimeout(hintTimer);
  // A moment to cross the gap from the element into the hint.
  if (!hintPinned && !hintPop.hidden) {
    clearTimeout(hintTimer);
    hintTimer = setTimeout(hideHint, 200);
  }
});
document.addEventListener("click", (e) => {
  const el = e.target.closest?.(".hint");
  if (el) {
    hintPinned = false;
    showHint(el);
    hintPinned = true;
  } else if (!hintPop.contains(e.target)) {
    hideHint();
  }
});
document.addEventListener("keydown", (e) => e.key === "Escape" && hideHint());
// The page scrolling moves the element away; the hint's own scroll does not.
document.addEventListener("scroll", (e) => e.target !== hintPop && hideHint(), true);

// --- snapshots ----------------------------------------------------------

function renderSnapshots(sv) {
  warnings(sv.warnings);
  const snaps = sv.snapshots;
  const ready = snaps.filter((s) => s.ready).length;
  const notReady = snaps.length - ready;
  $("snapshot-kpis").innerHTML = [
    kpi("Snapshots", snaps.length, snaps.length ? "" : "none reported"),
    kpi("Ready", ready, ""),
    kpi("Not ready", notReady, notReady ? "see the table" : "none", notReady ? "bad" : "good"),
    kpi("Logical size", bytes(snaps.reduce((a, s) => a + (s.logical_bytes || 0), 0)), "all snapshots"),
  ].join("");

  state.snapRows = snaps;
  renderSnapshotRows();
}

// renderSnapshotRows lists the snapshots that match the search, up to
// TABLE_ROWS.
function renderSnapshotRows() {
  const snaps = state.snapRows || [];
  const q = $("snap-search").value.trim().toLowerCase();
  const bad = $("snap-bad").checked;
  const matches = (s) => (!bad || !s.ready) && (!q || [s.name, s.volume, s.pool, s.status, s.kind, s.hostname].some((x) => x && x.toLowerCase().includes(q)));
  const rows = snaps.filter(matches);
  $("snap-count").textContent = rowCount(0, rows.length, q || bad);
  const opt = (v, f) => (v == null ? "–" : f(v));
  $("snapshots").innerHTML = rows.slice(0, TABLE_ROWS).map((s) => `<tr>
    <td class="col-pool">${esc(s.pool)}</td>
    <td class="name">${esc(s.name)}</td>
    <td>${check(s.ready, s.status || "READY", s.status || "unknown")}</td>
    <td>${esc(s.kind || "–")}</td>
    <td>${esc(s.incremental || "–")}</td>
    <td>${esc(s.volume || "–")}</td>
    <td class="num">${opt(s.logical_bytes, bytes)}</td>
    <td class="num">${opt(s.bytes, bytes)}</td>
    <td class="num">${serverSideCell(s)}</td>
    <td class="num">${errorsCell(s)}</td>
    <td class="num">${opt(s.elapsed, seconds)}</td>
    <td class="num">${opt(s.transfer_time, seconds)}</td>
    <td class="num">${opt(s.transfers, (v) => v.toFixed(0))}</td>
    <td class="num">${opt(s.speed, (v) => bytes(v, "/s"))}</td>
    <td>${s.node ? nodeCell(s) : status("–", "none")}</td>
  </tr>`).join("") || `<tr><td colspan="15" class="dim">${snaps.length ? "No snapshot matches." : "No snapshots reported."}</td></tr>`;
}

$("snap-search").addEventListener("input", renderSnapshotRows);
$("snap-bad").addEventListener("change", renderSnapshotRows);

// serverSideCell is the share of the bytes sent that S3 copied server-side,
// with the bytes in its hint.
function serverSideCell(s) {
  if (s.server_side_bytes == null) return "–";
  const share = s.bytes ? (100 * s.server_side_bytes) / s.bytes : null;
  return `<span class="hint" data-hint="${esc(`${bytes(s.server_side_bytes)} of ${bytes(s.bytes)} sent`)}">${share == null ? bytes(s.server_side_bytes) : pct(share)}</span>`;
}

// errorsCell is the snapshot's errors, by kind in its hint.
function errorsCell(s) {
  const kinds = [["Transfer", s.transfer_errors], ["Fatal", s.fatal_errors], ["Retry", s.retry_errors]];
  if (kinds.every(([, v]) => v == null)) return "–";
  const total = kinds.reduce((a, [, v]) => a + (v || 0), 0);
  const hint = kinds.map(([k, v]) => `${k}: ${v ?? "–"}`).join("\n");
  return `<span class="hint" data-hint="${esc(hint)}">${total ? status(String(total), "bad") : "0"}</span>`;
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
  // A link from another table may name the node by its UUID.
  const n = nodes.find((n) => nodeKey(n) === want || (n.uuid && `${n.pool}/${n.uuid}` === want)) || nodes[0];
  return n ? { pool: n.pool, ip: n.ip, node: n } : null;
}

$("host-pick").addEventListener("change", (e) => {
  $("node-pick").open = false;
  setHash("hosts", { host: e.target.value });
});

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
    return `<label><input type="radio" name="host" value="${esc(v)}" ${h && v === `${h.pool}/${h.ip}` ? "checked" : ""}> ${esc(hostLabel(n))}</label>`;
  }).join("");
  $("node-label").textContent = h ? hostLabel(h.node, true) : "none";
  searchMenu($("node-pick"));
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
    // A panel shown in two rows names it in data-panel.
    const panel = el.dataset.panel || id.slice(3);
    try {
      if (id.startsWith("vp-")) {
        // Volume charts load only for the volumes picked in the table.
        const vols = pickedVolumes();
        if (!vols.length) return emptyChart(id, "Pick volumes in the table above to see their metrics.");
        drawChart(id, await api(`/api/volume/series?panel=${panel}&${timeQuery()}&${poolQuery()}&vols=${vols.map(encodeURIComponent).join(",")}`));
      } else if (h) {
        drawChart(id, await api(`/api/host/series?${hostQuery(h)}&panel=${panel}&${timeQuery()}`));
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

// The charts' time: a window dragged on a chart (from/to in the hash), or else the last state.range seconds.
function zoomed() {
  const h = hashParams();
  return h.from && h.to ? { from: Number(h.from), to: Number(h.to) } : null;
}

function timeQuery() {
  const z = zoomed();
  return z ? `from=${z.from}&to=${z.to}` : `range=${state.range}`;
}

// Every view's range picker sets the one range, and leaves a zoom.
document.querySelectorAll(".filters [data-range]").forEach((b) =>
  b.addEventListener("click", () => {
    state.range = Number(b.dataset.range);
    if (zoomed()) return setHash(currentView(), { from: "", to: "" });
    renderRange();
    refresh();
  }),
);

// Each range picker gets the zoomed window and a way back.
document.querySelectorAll(".filters").forEach((f) => {
  if (!f.querySelector("[data-range]")) return;
  f.insertAdjacentHTML("beforeend", `<span class="zoom-info" hidden></span><button class="ghost zoom-out" hidden>Zoom out</button>`);
  f.querySelector(".zoom-out").addEventListener("click", zoomOut);
});

function zoomOut() {
  if (zoomed()) setHash(currentView(), { from: "", to: "" });
}

// renderRange marks the picked range, or none and the window when zoomed.
function renderRange() {
  const z = zoomed();
  document.querySelectorAll(".filters [data-range]").forEach((x) => x.setAttribute("aria-checked", String(!z && Number(x.dataset.range) === state.range)));
  const fmt = (t) => new Date(t * 1000).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" });
  document.querySelectorAll(".filters .zoom-info").forEach((el) => {
    el.hidden = !z;
    el.textContent = z ? `${fmt(z.from)} – ${fmt(z.to)}` : "";
  });
  document.querySelectorAll(".filters .zoom-out").forEach((el) => (el.hidden = !z));
}

// zoomTo is a drag across a chart: the page shows that window, at least a
// minute, refetched at its own finer step.
function zoomTo(u) {
  const { left, width } = u.select;
  u.setSelect({ left: 0, top: 0, width: 0, height: 0 }, false);
  if (width < 5) return;
  let from = Math.floor(u.posToVal(left, "x"));
  let to = Math.ceil(u.posToVal(left + width, "x"));
  if (to - from < 60) {
    const mid = Math.round((from + to) / 2);
    [from, to] = [mid - 30, mid + 30];
  }
  setHash(currentView(), { from: String(from), to: String(to) });
}

async function renderCharts(metrics) {
  const nodes = pickedNodes();
  await Promise.all(metrics.map(async (m) => {
    try {
      // The charts load only for the nodes picked in the table.
      if (!nodes.length) return emptyChart(m, "Pick nodes in the table above to see their metrics.");
      const data = await api(`/api/series?metric=${m}&${timeQuery()}&${poolQuery()}&nodes=${nodes.map(encodeURIComponent).join(",")}`);
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
  const key = data.series.map((s) => s.pool + "/" + (s.label || s.volume || s.ip)).join(",") + "|" + timeQuery();
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
    // Dragging across the plot zooms the page to that window;
    // a double click zooms back out.
    cursor: { points: { size: 8 }, drag: { x: true, y: false, setScale: false } },
    select: { show: true },
    legend: { live: true },
    hooks: {
      setSelect: [zoomTo],
      ready: [(u) => u.over.addEventListener("dblclick", zoomOut)],
    },
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
// highest first, with the line nearest the cursor in bold.
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
