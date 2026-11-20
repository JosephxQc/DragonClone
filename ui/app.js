// DragonClone front end. No framework: state lives in `S`, views are plain
// template strings, and clicks are routed through data-act attributes.
"use strict";

// ---- talking to the backend -------------------------------------------------

const token = (() => {
  const t = new URLSearchParams(location.search).get("t");
  if (t) {
    sessionStorage.setItem("dc-token", t);
    history.replaceState(null, "", "/"); // keep the token out of sight
  }
  return t || sessionStorage.getItem("dc-token") || "";
})();

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: { "X-DragonClone-Token": token, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let data = null;
  try { data = await res.json(); } catch (_) { /* empty body */ }
  if (!res.ok) {
    const err = new Error((data && data.error) || res.statusText || "Request failed");
    err.hint = data && data.hint;
    throw err;
  }
  return data;
}

// ---- state ------------------------------------------------------------------

const S = {
  info: null,
  disks: [],
  view: "disks",
  flow: null,
  job: null,       // last job snapshot
  jobLogs: [],
  ackedJob: 0,     // id of the finished job the user dismissed
  showJob: false,
  history: null,
  diskError: null,
};

const settings = {
  get(k, d) { try { const v = localStorage.getItem("dc-" + k); return v === null ? d : JSON.parse(v); } catch (_) { return d; } },
  set(k, v) { try { localStorage.setItem("dc-" + k, JSON.stringify(v)); } catch (_) { /* ignore */ } },
};

// ---- little helpers -----------------------------------------------------------

const $ = (sel, root = document) => root.querySelector(sel);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

// Sizes the way Explorer shows them (GB = 1024^3).
function bytes(n) {
  if (n == null || n < 0) return "-";
  const u = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(n >= 100 ? 0 : 1)) + " " + u[i];
}

function dur(sec) {
  if (!isFinite(sec) || sec < 0) return "-";
  sec = Math.round(sec);
  const h = Math.floor(sec / 3600), m = Math.floor(sec / 60) % 60, s = sec % 60;
  if (h) return `${h} h ${String(m).padStart(2, "0")} min`;
  if (m) return `${m} min ${String(s).padStart(2, "0")} s`;
  return `${s} s`;
}

function when(iso) {
  const d = new Date(iso);
  return d.toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" }) + ", " +
    d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

function toast(msg, isErr) {
  const el = document.createElement("div");
  el.className = "toast" + (isErr ? " err" : "");
  el.textContent = msg;
  $("#toasts").append(el);
  setTimeout(() => el.remove(), isErr ? 7000 : 3500);
}

const isWin = () => S.info && S.info.os === "windows";
const sep = () => (isWin() ? "\\" : "/");

// ---- icons --------------------------------------------------------------------

const ICON = {
  disks: '<path d="M3 6.5A2.5 2.5 0 0 1 5.5 4h13A2.5 2.5 0 0 1 21 6.5v3a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 9.5zM3 15.5A2.5 2.5 0 0 1 5.5 13h13a2.5 2.5 0 0 1 2.5 2.5v2a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 17.5z" fill="none" stroke="currentColor" stroke-width="1.6"/><circle cx="17" cy="8" r="1.2" fill="currentColor"/><circle cx="17" cy="16.5" r="1.2" fill="currentColor"/>',
  clone: '<rect x="3" y="4" width="11" height="13" rx="2" fill="none" stroke="currentColor" stroke-width="1.6"/><rect x="10" y="8" width="11" height="13" rx="2" fill="none" stroke="currentColor" stroke-width="1.6"/>',
  backup: '<path d="M12 3v11m0 0-4-4m4 4 4-4M4 15v3a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-3" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>',
  restore: '<path d="M12 21V10m0 0-4 4m4-4 4 4M4 9V6a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v3" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>',
  tools: '<path d="M14.5 6.5a4 4 0 0 0-5.3 4.9L4 16.6 7.4 20l5.2-5.2a4 4 0 0 0 4.9-5.3l-2.4 2.4-2.6-.4-.4-2.6z" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/>',
  history: '<path d="M4 12a8 8 0 1 0 2.4-5.7M4 4v4h4M12 8v4l3 2" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>',
  compare: '<path d="M8 4v16M16 4v16M4 8h8M12 16h8" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/>',
  check: '<path d="M12 3 4 6v6c0 4.4 3.4 8.2 8 9 4.6-.8 8-4.6 8-9V6z M8.5 12l2.5 2.5 4.5-5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/>',
  scan: '<circle cx="11" cy="11" r="6.5" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="m16 16 4.5 4.5" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/>',
  wipe: '<path d="M5 7h14M10 7V4h4v3M7 7l1 13h8l1-13" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/>',
  mount: '<rect x="4" y="11" width="16" height="9" rx="2" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M12 3v7m-3-3 3 3 3-3" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>',
  folder: '<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" fill="currentColor"/>',
  file: '<path d="M6 3h8l4 4v14H6z" fill="none" stroke="currentColor" stroke-width="1.6"/>',
};
const icon = (name) => `<svg viewBox="0 0 24 24" aria-hidden="true">${ICON[name] || ""}</svg>`;

// ---- navigation -----------------------------------------------------------------

const NAV = [
  ["disks", "Disks"], ["clone", "Clone"], ["backup", "Back up"], ["restore", "Restore"],
  ["tools", "Tools"], ["history", "History"],
];

function jobRunning() { return !!S.job && S.job.state === "running"; }
// The job screen shows while a job runs (unless the user stepped away to look
// at disks or history) and once more for its result, until dismissed.
function jobVisible() { return !!S.job && S.showJob && (S.job.state === "running" || S.ackedJob !== S.job.id); }

function renderNav() {
  const busy = jobRunning();
  let html = NAV.map(([id, label]) => {
    const current = !jobVisible() && (S.view === id || S.view.startsWith(id + ":"));
    const lock = busy && ["clone", "backup", "restore", "tools"].includes(id);
    return `<button class="nav-item" data-act="nav" data-id="${id}" ${current ? 'aria-current="page"' : ""} ${lock ? "disabled" : ""}>${icon(id)}<span>${label}</span></button>`;
  }).join("");
  if (busy) {
    const st = S.job.status || {};
    const pct = st.total ? Math.min(100, (st.done / st.total) * 100) : 0;
    html += `<div class="nav-sep"></div><button class="running-pill" data-act="show-job"><span>${esc(S.job.title)} - ${pct.toFixed(0)}%</span><div class="bar"><i style="width:${pct}%"></i></div></button>`;
  }
  $("#nav").innerHTML = html;
}

function go(view) {
  S.view = view;
  S.showJob = false;
  if (["clone", "backup", "restore"].includes(view)) newFlow(view);
  if (view.startsWith("tool:")) newFlow(view.slice(5));
  if (view === "history") loadHistory();
  if (S.job && S.job.state !== "running") S.ackedJob = S.job.id;
  render();
  $("#main").scrollTop = 0;
}

// ---- rendering ---------------------------------------------------------------------

function render() {
  renderNav();
  const main = $("#main");
  if (jobVisible()) {
    if (!main.querySelector(".job") || main.dataset.job !== String(S.job.id)) {
      main.innerHTML = viewJob();
      main.dataset.job = S.job.id;
      setupJobView();
    }
    updateJob();
    return;
  }
  main.dataset.job = "";
  const v = S.view;
  let html;
  if (v === "disks") html = viewDisks();
  else if (v === "history") html = viewHistory();
  else if (v === "tools") html = viewTools();
  else html = viewFlow();
  main.innerHTML = html;
  afterRender();
}

function pageHead(title, lede, right = "") {
  return `<div class="page-head"><div><h1>${title}</h1>${lede ? `<p class="lede">${lede}</p>` : ""}</div>${right}</div>`;
}

// ---- disks -------------------------------------------------------------------------

function partClass(p) {
  if (p.system || /EFI|Reserved|Recovery|System/i.test(p.type)) return "sys";
  if (/Basic|NTFS|FAT|exFAT|Linux data|Linux$/i.test(p.type) || p.fs) return "data";
  return "";
}

function partLabel(p) {
  const bits = [];
  if (p.letter) bits.push(p.letter);
  if (p.label) bits.push(p.label);
  if (!bits.length) bits.push(p.name || p.type);
  return bits.join(" ");
}

// One disk drawn as a track: partitions are segments sized by capacity,
// unallocated gaps are hatched.
function diskTrack(d, opts = {}) {
  const parts = [...(d.partitions || [])].sort((a, b) => a.offset - b.offset);
  const segs = [];
  let pos = 0;
  const minGap = Math.max(64 << 20, d.size * 0.01);
  for (const p of parts) {
    if (p.offset - pos > minGap) segs.push({ free: true, size: p.offset - pos });
    segs.push({ p, size: p.size });
    pos = Math.max(pos, p.offset + p.size);
  }
  if (d.size - pos > minGap) segs.push({ free: true, size: d.size - pos });
  if (!segs.length) segs.push({ free: true, size: d.size || 1, empty: true });

  return `<div class="track">${segs.map((s) => {
    const grow = Math.max(s.size / (d.size || 1), 0.035);
    if (s.free) return `<div class="seg free" style="flex:${grow} 1 0"><span>${s.empty ? "No partitions" : "Unallocated " + bytes(s.size)}</span></div>`;
    const p = s.p;
    const why = opts.pickParts ? opts.disable?.({ kind: "part", disk: d, part: p }) : "";
    const pick = opts.pickParts && !why;
    const used = p.used >= 0 && p.size ? Math.min(100, (p.used / p.size) * 100) : -1;
    return `<button class="seg ${partClass(p)} ${pick ? "pickable" : ""} ${why ? "disabled" : ""} ${opts.selected === p.id ? "selected" : ""}"
      style="flex:${grow} 1 0" ${pick ? `data-act="pick" data-id="${esc(p.id)}"` : opts.pickParts ? 'data-act="noop" tabindex="-1"' : 'tabindex="-1"'}
      title="${esc(`${p.id}: ${partLabel(p)}, ${bytes(p.size)}${why ? " - " + why : ""}`)}">
      ${used >= 0 ? `<i class="used" style="width:${used}%"></i>` : ""}<span>${esc(partLabel(p))}</span></button>`;
  }).join("")}</div>`;
}

function diskCard(d, opts = {}) {
  const why = opts.pick ? opts.disable?.({ kind: "disk", disk: d }) : "";
  const pickable = opts.pick && opts.pickDisks !== false && !why;
  const cls = ["disk", pickable ? "pickable" : "", why && opts.pickDisks !== false ? "disabled" : "", opts.selected === d.id ? "selected" : ""].join(" ");
  const tags = [];
  if (d.system) tags.push(`<span class="tag sys">${isWin() ? "Windows disk" : "System disk"}</span>`);
  if (d.removable) tags.push('<span class="tag">Removable</span>');
  if (d.sectorSize && d.sectorSize !== 512) tags.push(`<span class="tag">${d.sectorSize}-byte sectors</span>`);
  const head = `<div class="disk-head">
      <span class="disk-num">${esc(d.id.startsWith("disk") ? "Disk " + d.number : d.id)}</span>
      <span class="disk-model">${esc(d.model)}</span>
      <span class="disk-meta">${d.bus ? `<span>${esc(d.bus)}</span>` : ""}${d.style && d.style !== "-" ? `<span>${esc(d.style)}</span>` : ""}${tags.join("")}</span>
      <span class="disk-size">${d.noMedia ? "No media" : bytes(d.size)}</span>
    </div>`;
  if (d.noMedia) return `<div class="disk disabled">${head}</div>`;

  const parts = (d.partitions || []).map((p) => `<div>
      <span class="pid">${esc(p.id)}</span><span>${esc(partLabel(p))}${p.fs ? ` <span class="muted">${esc(p.fs)}</span>` : ""}${p.letter || p.label ? ` <span class="muted">${esc(p.type)}</span>` : ""}</span>
      <span class="pused">${p.used >= 0 ? bytes(p.used) + " used" : ""}</span><span class="psize">${bytes(p.size)}</span></div>`).join("");

  return `<div class="${cls}" ${pickable ? `data-act="pick" data-id="${esc(d.id)}" tabindex="0" role="button"` : ""}>
    ${head}
    ${diskTrack(d, opts)}
    ${parts ? `<div class="parts">${parts}</div>` : ""}
    ${why && opts.pickDisks !== false ? `<div class="pick-hint">${esc(why)}</div>` : ""}
    ${opts.actions ? `<div class="disk-actions">${opts.actions(d)}</div>` : ""}
  </div>`;
}

function viewDisks() {
  let body;
  if (S.diskError) body = `<p class="note err">${esc(S.diskError)}</p>`;
  else if (!S.disks.length) body = `<div class="empty">No disks found. Plug one in and select Refresh.</div>`;
  else body = `<div class="disks">${S.disks.map((d) => diskCard(d, {
    actions: (d) => `
      <button class="btn small" data-act="start-flow" data-kind="backup" data-src="${esc(d.id)}">Back up</button>
      <button class="btn small" data-act="start-flow" data-kind="clone" data-src="${esc(d.id)}">Clone</button>
      <button class="btn small" data-act="start-flow" data-kind="scan" data-src="${esc(d.id)}">Scan for bad sectors</button>
      ${d.system ? "" : `<button class="btn small" data-act="start-flow" data-kind="wipe" data-tgt="${esc(d.id)}">Wipe</button>`}`,
  })).join("")}</div>`;
  const admin = S.info && !S.info.admin
    ? `<p class="note err">DragonClone isn't running as administrator, so Windows hides the disks from it. Close it and start it again, then choose Yes when Windows asks.</p>` : "";
  return `<div class="page">${pageHead("Disks", "Everything connected to this PC. Partitions are drawn to scale; the dark strip at the bottom of each shows how full it is.",
    `<button class="btn" data-act="refresh">Refresh</button>`)}${admin}${body}</div>`;
}

async function loadDisks() {
  try {
    S.disks = await api("GET", "/api/disks");
    S.diskError = null;
  } catch (e) {
    S.disks = [];
    S.diskError = e.message;
  }
}

const findDisk = (id) => S.disks.find((d) => d.id === id);
function findItem(id) {
  for (const d of S.disks) {
    if (d.id === id) return { kind: "disk", disk: d, size: d.size, id };
    for (const p of d.partitions || []) if (p.id === id) return { kind: "part", disk: d, part: p, size: p.size, id };
  }
  return null;
}
function itemLabel(id) {
  const it = findItem(id);
  if (!it) return esc(id);
  if (it.kind === "disk") return `<b>${esc(it.disk.id.startsWith("disk") ? "Disk " + it.disk.number : it.disk.id)}</b> ${esc(it.disk.model)}, ${bytes(it.size)}`;
  return `<b>${esc(it.part.id)}</b> ${esc(partLabel(it.part))}, ${bytes(it.size)}`;
}

// ---- flows (clone / backup / restore / tools) --------------------------------------

const FLOW_INFO = {
  clone: ["Clone a disk", "Copies every sector of a disk or partition onto another one, so the copy is identical: partitions, boot code, empty space and all."],
  backup: ["Back up to an image", "Saves a disk or partition into a single file that you can restore later or browse."],
  restore: ["Restore an image", "Writes an image file back onto a disk or partition. Works with .img, .img.gz, .vhd and .iso files."],
  compare: ["Compare", "Checks two disks, partitions or images byte by byte and tells you whether they're identical."],
  check: ["Check an image", "Reads an image from start to end and checks it against the checksum saved when it was made."],
  scan: ["Surface scan", "Reads every sector of a disk and maps out the ones that can't be read. Nothing is written."],
  wipe: ["Wipe a disk", "Overwrites a disk or partition completely, so nothing on it can be recovered."],
  mount: ["Mount an image", "Opens a .vhd or .iso image as a read-only drive so you can browse its files in Explorer."],
};

const STEPS = {
  clone: ["source", "target", "options", "review"],
  backup: ["source", "image-out", "options", "review"],
  restore: ["image-in", "target", "options", "review"],
  wipe: ["target", "wipe-opts", "review"],
  scan: ["source", "review"],
  check: ["image-in", "review"],
  compare: ["pair", "review"],
  mount: ["mount"],
};

function newFlow(kind, preset = {}) {
  S.flow = {
    kind, step: 0, source: null, target: null, image: null, imagePath: "",
    verify: settings.get("verify", true), skipBad: false, shutdown: false,
    level: settings.get("level", 1), format: settings.get("format", "gzip"),
    folder: settings.get("folder", ""), filename: "", wipeMode: "zeros",
    a: null, b: null, plan: null, planError: null, confirm: "", accept: false, busy: false, mounted: "",
    ...preset,
  };
  const f = S.flow;
  if (f.source && STEPS[kind][0] === "source") f.step = 1;
  if (f.target && STEPS[kind][0] === "target") f.step = 1;
  if (kind === "backup" && f.source) f.filename = defaultImageName(f.source);
}

function defaultImageName(src) {
  const it = findItem(src);
  let base = it && it.kind === "disk" && it.disk.model && it.disk.model !== "Unknown disk" ? it.disk.model : src;
  base = base.replace(/^\/dev\//, "").replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
  const d = new Date();
  return `${base || "disk"}-${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

const FORMATS = {
  gzip: { ext: ".img.gz", name: "Compressed", text: "Usually much smaller than the disk. Any gzip tool can unpack it." },
  raw: { ext: ".img", name: "Raw", text: "Exactly as big as the disk. Works with every disk tool there is." },
  vhd: { ext: ".vhd", name: "Virtual disk", text: "As big as the disk. Windows can open it like a drive to browse the files." },
};

function imageOutPath(f) {
  let name = (f.filename || "").trim();
  for (const fmt of Object.values(FORMATS)) if (name.toLowerCase().endsWith(fmt.ext)) name = name.slice(0, -fmt.ext.length);
  if (!name || !f.folder) return "";
  const folder = f.folder.endsWith(sep()) ? f.folder : f.folder + sep();
  return folder + name + FORMATS[f.format].ext;
}

// What may be picked where. Returns a reason when it can't be.
function disabler(role) {
  const f = S.flow;
  return (it) => {
    const d = it.disk;
    const size = it.kind === "disk" ? d.size : it.part.size;
    if (role === "source") return "";
    if (it.kind === "disk" && d.system) return (isWin() ? "Windows" : "The system") + " runs from this disk, so it can't be overwritten";
    if (it.kind === "part" && it.part.system) return (isWin() ? "Windows" : "The system") + " needs this partition";
    let need = 0;
    if (f.kind === "clone" && f.source) {
      const src = findItem(f.source);
      if (src) {
        need = src.size;
        if (src.disk.id === d.id && (it.kind === "disk" || src.kind === "disk" || src.id === it.part.id)) return "That's where the source is";
      }
    }
    if (f.kind === "restore" && f.image) need = f.image.size;
    if (need && size < need) return `Too small - needs ${bytes(need)}`;
    return "";
  };
}

function stepTitle(key) {
  const k = S.flow.kind;
  return {
    source: { clone: "What to copy", backup: "What to back up", scan: "What to scan" }[k],
    target: { clone: "Where to copy it", restore: "Where to restore it", wipe: "What to wipe" }[k],
    "image-out": "Where to save the image",
    "image-in": k === "check" ? "Image to check" : "Image to restore",
    options: "Options",
    "wipe-opts": "How to wipe it",
    pair: "What to compare",
    review: "Check and start",
    mount: "Image to mount",
  }[key];
}

function stepSummary(key) {
  const f = S.flow;
  switch (key) {
    case "source": return itemLabel(f.source);
    case "target": return itemLabel(f.target);
    case "image-out": return `<b>${esc(imageOutPath(f))}</b> ${FORMATS[f.format].name.toLowerCase()}`;
    case "image-in": return f.image ? `<b>${esc(f.image.path)}</b> ${bytes(f.image.size)} disk` : "";
    case "options": {
      const o = [f.verify ? "verify afterwards" : "no verify", f.skipBad ? "skip unreadable sectors" : "stop on read errors"];
      if (f.kind === "backup" && f.format === "gzip") o.push(f.level > 1 ? "smaller file" : "faster compression");
      if (f.shutdown) o.push("shut down when finished");
      return esc(o.join(", "));
    }
    case "wipe-opts": return esc((f.wipeMode === "random" ? "Random data" : "Zeros") + (f.verify ? ", verify afterwards" : ""));
    case "pair": return `${f.a ? esc(f.a.label) : ""} <span class="muted">and</span> ${f.b ? esc(f.b.label) : ""}`;
  }
  return "";
}

function viewFlow() {
  const f = S.flow;
  if (!f) return "";
  const [title, lede] = FLOW_INFO[f.kind];
  const keys = STEPS[f.kind];
  const back = f.kind in { compare: 1, check: 1, scan: 1, wipe: 1, mount: 1 } ? `<button class="btn" data-act="nav" data-id="tools">Back to tools</button>` : "";
  const steps = keys.map((key, i) => {
    const n = keys.length > 1 ? `<span class="step-n">${i + 1}</span>` : "";
    if (i < f.step) {
      return `<section class="step done"><div class="step-title">${n}<h2>${stepTitle(key)}</h2></div>
        <div class="step-summary">${stepSummary(key)}</div><button class="ghost small" data-act="goto-step" data-step="${i}">Change</button></section>`;
    }
    if (i > f.step) return `<section class="step later"><div class="step-title">${n}<h2>${stepTitle(key)}</h2></div></section>`;
    return `<section class="step current"><div class="step-title">${n}<h2>${stepTitle(key)}</h2></div>${stepBody(key)}</section>`;
  }).join("");
  return `<div class="page">${pageHead(title, lede, back)}<div class="flow">${steps}</div></div>`;
}

function stepBody(key) {
  const f = S.flow;
  switch (key) {
    case "source":
    case "target": {
      const role = key;
      const sel = f[role];
      const cards = S.disks.map((d) => diskCard(d, { pick: true, pickParts: true, selected: sel, disable: disabler(role) })).join("");
      return `<p class="muted" style="margin:0 0 12px">Select a whole disk, or one of its partitions in the bar.</p>
        ${S.disks.length ? `<div class="disks">${cards}</div>` : `<div class="empty">No disks found.</div>`}
        <div class="actions"><button class="ghost" data-act="refresh">Refresh list</button></div>`;
    }
    case "image-out":
      return `<div class="formats">${Object.entries(FORMATS).map(([id, fm]) => `
          <button class="format" data-act="format" data-id="${id}" aria-pressed="${f.format === id}">
            <b>${fm.name} <code>${fm.ext}</code></b><small>${fm.text}</small></button>`).join("")}</div>
        <div class="row wrap" style="gap:14px;align-items:flex-end">
          <label class="field grow"><span>Folder</span>
            <div class="row"><input type="text" id="folder" value="${esc(f.folder)}" placeholder="${isWin() ? "D:\\Backups" : "/mnt/backups"}" spellcheck="false">
            <button class="btn" data-act="browse-folder">Browse</button></div></label>
          <label class="field" style="flex:0 1 320px"><span>File name</span>
            <input type="text" id="filename" value="${esc(f.filename)}" spellcheck="false"></label>
        </div>
        <p class="muted" id="outpath" style="margin:10px 0 0">${esc(imageOutPath(f) || "Pick a folder and a name.")}</p>
        <div class="actions"><button class="btn primary" data-act="next" id="nextBtn">Next</button></div>`;
    case "image-in":
      return `<div class="row"><input type="text" id="imagePath" value="${esc(f.imagePath)}" placeholder="Path to an image file" spellcheck="false">
          <button class="btn" data-act="browse-image">Browse</button></div>
        ${f.imageError ? `<p class="note err">${esc(f.imageError)}</p>` : ""}
        ${f.image ? imageCard(f.image) : ""}
        <div class="actions"><button class="btn primary" data-act="inspect" ${f.busy ? "disabled" : ""}>${f.busy ? "Reading image..." : "Next"}</button></div>`;
    case "options": {
      const level = f.kind === "backup" && f.format === "gzip"
        ? toggle("level", "Smaller file", "Squeezes harder. Takes longer and uses more CPU.", f.level > 1) : "";
      return `<div>
        ${toggle("verify", "Verify afterwards", "Reads everything back and checks it matches. Takes about as long again.", f.verify)}
        ${toggle("skipBad", "Skip unreadable sectors", "For failing disks: saves everything readable, writes zeros where a sector can't be read, and lists those spots. Off means the copy stops at the first read error.", f.skipBad)}
        ${level}
        ${toggle("shutdown", "Shut down the PC when finished", "Only if everything went well. You get a minute to cancel.", f.shutdown)}
      </div><div class="actions"><button class="btn primary" data-act="next">Next</button></div>`;
    }
    case "wipe-opts":
      return `<div class="formats" style="grid-template-columns:repeat(2,1fr)">
          <button class="format" data-act="wipemode" data-id="zeros" aria-pressed="${f.wipeMode === "zeros"}"><b>Zeros</b><small>Fastest. Enough for modern drives.</small></button>
          <button class="format" data-act="wipemode" data-id="random" aria-pressed="${f.wipeMode === "random"}"><b>Random data</b><small>Leaves nothing recognisable behind.</small></button>
        </div>
        ${toggle("verify", "Verify afterwards", "Reads the disk back to make sure every sector took the new data.", f.verify)}
        <p class="muted small">Tip: for SSDs, the drive maker's secure-erase tool reaches spare areas a wipe can't.</p>
        <div class="actions"><button class="btn primary" data-act="next">Next</button></div>`;
    case "pair":
      return `${pairSide("a", "First")}${pairSide("b", "Second")}
        <div class="actions"><button class="btn primary" data-act="next" ${f.a && f.b ? "" : "disabled"}>Next</button></div>`;
    case "mount":
      return `<div class="row"><input type="text" id="imagePath" value="${esc(f.imagePath)}" placeholder="Path to a .vhd or .iso file" spellcheck="false">
          <button class="btn" data-act="browse-image">Browse</button></div>
        ${f.mounted ? `<p class="note" style="border-color:var(--good)">Mounted${f.mounted.trim() ? " as " + esc(f.mounted) : ""}. It's read-only, so nothing in the image can change.</p>` : ""}
        ${f.imageError ? `<p class="note err">${esc(f.imageError)}</p>` : ""}
        <div class="actions">
          <button class="btn" data-act="unmount" ${f.busy ? "disabled" : ""}>Unmount</button>
          <button class="btn primary" data-act="mount" ${f.busy ? "disabled" : ""}>${f.busy ? "Working..." : "Mount"}</button></div>`;
    case "review":
      return reviewBody();
  }
  return "";
}

function toggle(id, title, text, on) {
  return `<label class="toggle"><input type="checkbox" data-opt="${id}" ${on ? "checked" : ""}><span class="sw"></span>
    <span><b>${title}</b><small>${text}</small></span></label>`;
}

function pairSide(side, label) {
  const v = S.flow[side];
  return `<div class="field" style="margin-bottom:14px"><span>${label}</span>
    <div class="row"><input type="text" readonly value="${esc(v ? v.label : "")}" placeholder="Pick a disk, partition or image">
    <button class="btn" data-act="pair-disk" data-side="${side}">Disk or partition</button>
    <button class="btn" data-act="pair-image" data-side="${side}">Image file</button></div></div>`;
}

function imageCard(ep) {
  const m = ep.meta;
  const rows = [
    ["Format", { gzip: "Compressed image", vhd: "Virtual disk (VHD)", raw: "Raw image" }[ep.format] || ep.format],
    ["Disk inside", `${bytes(ep.size)} (${Number(ep.size).toLocaleString()} bytes)`],
  ];
  if (m) {
    rows.push(["Made", `${when(m.created)} with DragonClone ${esc(m.version)}`]);
    rows.push(["From", m.source]);
    if (m.sectorSize) rows.push(["Sector size", m.sectorSize + " bytes"]);
    rows.push(["Checksum", m.sha256 ? "Saved, will be checked during the restore" : "None saved"]);
    if (m.badBytes) rows.push(["Unreadable when made", bytes(m.badBytes) + " (written as zeros)"]);
  } else {
    rows.push(["Details", "No DragonClone info file next to this image, so where it came from is unknown."]);
  }
  return `<dl class="facts" style="margin-top:16px">${rows.map(([k, v]) => `<dt>${k}</dt><dd>${esc(v)}</dd>`).join("")}</dl>`;
}

function request() {
  const f = S.flow;
  const r = { kind: f.kind, verify: f.verify, skipBad: f.skipBad, shutdown: f.shutdown, level: f.level, retries: 1, wipeMode: f.wipeMode };
  switch (f.kind) {
    case "clone": r.source = f.source; r.target = f.target; break;
    case "backup": r.source = f.source; r.target = imageOutPath(f); break;
    case "restore": r.source = f.image.path; r.target = f.target; break;
    case "wipe": r.target = f.target; break;
    case "scan": r.source = f.source; break;
    case "check": r.source = f.image.path; break;
    case "compare": r.source = f.a.value; r.target = f.b.value; break;
  }
  r.acceptRisks = f.accept;
  r.confirm = f.confirm;
  r.allowLive = !!f.allowLive;
  r.forceDismount = !!f.forceDismount;
  return r;
}

async function loadPlan() {
  const f = S.flow;
  f.plan = null;
  f.planError = null;
  f.confirm = "";
  f.accept = false;
  render();
  try {
    f.plan = await api("POST", "/api/plan", request());
  } catch (e) {
    f.planError = e;
  }
  if (S.flow === f) render();
}

const START_LABEL = { clone: "Start cloning", backup: "Start backup", restore: "Start restore", wipe: "Wipe it", scan: "Start scan", check: "Check image", compare: "Compare" };

function reviewBody() {
  const f = S.flow;
  if (f.planError) {
    return `<p class="note err">${esc(f.planError.message)}${f.planError.hint ? `<span class="hint">${esc(f.planError.hint)}</span>` : ""}</p>
      <div class="actions"><button class="btn" data-act="replan">Try again</button></div>`;
  }
  if (!f.plan) return `<p class="muted">Checking...</p>`;
  const p = f.plan;
  const facts = `<dl class="facts">${p.rows.map((r) => `<dt>${esc(r.label)}</dt><dd class="${esc(r.tone || "")}">${esc(r.value)}</dd>`).join("")}</dl>`;
  const warns = (p.warnings || []).map((w) => `<p class="note">${esc(w)}</p>`).join("");
  const risks = (p.risks || []).map((w) => `<p class="note risk">${esc(w)}</p>`).join("");
  const accept = (p.risks || []).length
    ? `<label class="check"><input type="checkbox" id="accept" ${f.accept ? "checked" : ""}><span>I've read the warnings above and want to go ahead.</span></label>` : "";
  let erase = "";
  if (p.destructive) {
    const list = (p.erases || []).length ? `<ul>${p.erases.map((e) => `<li>${esc(e)}</li>`).join("")}</ul>` : "";
    erase = `<div class="erase"><h3>Everything on ${esc(p.confirmName)} will be overwritten.</h3>
      ${list ? `<p style="margin:0 0 6px">These will be erased:</p>${list}` : ""}
      <div class="confirm-row"><span>Type <b class="mono">${esc(p.confirmName)}</b> to confirm</span>
      <input type="text" id="confirm" autocomplete="off" spellcheck="false" value="${esc(f.confirm)}"></div></div>`;
  }
  return `${facts}${warns}${risks}${accept}${erase}
    ${f.startError ? `<p class="note err">${esc(f.startError.message)}${f.startError.hint ? `<span class="hint">${esc(f.startError.hint)}</span>` : ""}</p>` : ""}
    <div class="actions"><button class="btn ${p.destructive ? "danger" : "primary"}" data-act="start" id="startBtn">${START_LABEL[f.kind]}</button></div>`;
}

function canStart() {
  const f = S.flow;
  const p = f && f.plan;
  if (!p || f.busy) return false;
  if ((p.risks || []).length && !f.accept) return false;
  if (p.destructive && f.confirm.trim().toLowerCase() !== p.confirmName.toLowerCase()) return false;
  return true;
}

function afterRender() {
  const sb = $("#startBtn");
  if (sb) sb.disabled = !canStart();
  const f = S.flow;
  if (f && STEPS[f.kind] && STEPS[f.kind][f.step] === "review" && !f.plan && !f.planError && !f.loading) {
    f.loading = true;
    loadPlan().finally(() => { f.loading = false; });
  }
}

async function startJob(extra = {}) {
  const f = S.flow;
  f.busy = true;
  f.startError = null;
  Object.assign(f, extra);
  try {
    const r = await api("POST", "/api/start", request());
    if (r.ask === "live") {
      const ok = await ask("Source is in use",
        `<p>Other programs are using ${listOf(r.volumes)} on the source. Files they change during the copy may come out inconsistent on the copy.</p>
         <p class="muted">For the cleanest copy, close those programs (and Explorer windows) and try again.</p>`, "Copy anyway");
      f.busy = false;
      if (ok) return startJob({ allowLive: true });
      return render();
    }
    if (r.ask === "inuse") {
      const ok = await ask("Target is in use",
        `<p>DragonClone couldn't lock ${listOf(r.volumes)} on the target, because a program still has files open there.</p>
         <p class="muted">Close those programs and try again, or force them off. Programs with files open there will lose access to them.</p>`, "Force and continue", true);
      f.busy = false;
      if (ok) return startJob({ forceDismount: true });
      return render();
    }
    f.busy = false;
    S.jobLogs = [];
    S.showJob = true;
    await pollJob();
    render();
  } catch (e) {
    f.busy = false;
    f.startError = e;
    render();
  }
}

function listOf(vols) { return (vols || []).map((v) => `<b>${esc(v)}</b>`).join(", "); }


// ---- tools page ------------------------------------------------------------------

function viewTools() {
  const tools = [
    ["compare", "Compare", "Check that two disks, partitions or images are exactly the same."],
    ["check", "Check an image", "Make sure a backup still reads back intact."],
    ["scan", "Surface scan", "Find unreadable sectors on a disk. Read-only."],
    ["wipe", "Wipe a disk", "Overwrite a disk or partition so nothing can be recovered."],
  ];
  if (isWin()) tools.push(["mount", "Mount an image", "Open a .vhd or .iso as a drive to browse its files."]);
  return `<div class="page">${pageHead("Tools", "")}<div class="tools">${tools.map(([id, name, text]) => `
    <button class="tool" data-act="nav" data-id="tool:${id}">${icon(id === "check" ? "check" : id)}<h3>${name}</h3><small>${text}</small></button>`).join("")}</div></div>`;
}

// ---- history ---------------------------------------------------------------------

async function loadHistory() {
  try { S.history = await api("GET", "/api/history"); } catch (_) { S.history = []; }
  if (S.view === "history" && !jobVisible()) render();
}

function viewHistory() {
  const h = S.history;
  let body;
  if (h === null) body = `<p class="muted">Loading...</p>`;
  else if (!h.length) body = `<div class="empty">Nothing here yet. Clones, backups and restores you run will be listed here.</div>`;
  else body = `<table class="hist"><thead><tr><th>When</th><th>Operation</th><th>Details</th><th>Took</th><th>Result</th></tr></thead><tbody>
    ${h.map((e) => `<tr><td>${esc(when(e.time))}</td><td>${esc(e.title)}</td>
      <td class="wrapcell">${e.source ? esc(e.source) + (e.target ? `<br>to ${esc(e.target)}` : "") : esc(e.target || "")}${e.error ? `<br><span style="color:var(--bad)">${esc(e.error)}</span>` : ""}</td>
      <td>${dur(e.seconds)}</td><td><span class="state ${esc(e.state)}">${{ done: "Finished", failed: "Failed", cancelled: "Cancelled" }[e.state] || esc(e.state)}</span></td></tr>`).join("")}
    </tbody></table>`;
  return `<div class="page">${pageHead("History", "", `<button class="btn" data-act="view-log">Open the log</button>`)}${body}</div>`;
}

// ---- job view ---------------------------------------------------------------------

const MAP_CELLS = 600;
const speed = { samples: [], points: [], phase: "" };

function viewJob() {
  const j = S.job;
  const cells = "<i></i>".repeat(MAP_CELLS);
  const readOnly = ["scan", "check", "compare"].includes(j.kind);
  return `<div class="page job">
    <div class="page-head"><div><h1>${esc(j.title)}</h1>
      <p class="lede">${esc(j.source || "")}${j.target ? ` <span class="muted">to</span> ${esc(j.target)}` : ""}</p></div>
      <div id="jobActions"></div></div>
    <div id="jobResult"></div>
    <div class="job-top">
      <div><div class="phase" id="phase"></div><div class="pct" id="pct">0<small>%</small></div></div>
      <div class="stats">
        <div class="stat"><b id="sDone">-</b><span id="sDoneL">done</span></div>
        <div class="stat"><b id="sSpeed">-</b><span>speed</span></div>
        <div class="stat"><b id="sLeft">-</b><span>left</span></div>
        <div class="stat" id="sBadW"><b id="sBad">0</b><span>unreadable sectors</span></div>
      </div>
    </div>
    <div class="progress" id="bar"><i></i></div>
    <div class="map-wrap">
      <div class="map-head"><h3>Sector map</h3>
        <div class="legend"><span><i style="background:var(--cell)"></i>Waiting</span>
        <span><i style="background:${readOnly ? "color-mix(in srgb, var(--text) 45%, var(--cell))" : "var(--accent)"}"></i>${readOnly ? "Read" : j.kind === "wipe" ? "Wiped" : "Copied"}</span>
        ${readOnly ? "" : '<span><i style="background:var(--good)"></i>Verified</span>'}
        <span><i style="background:var(--bad)"></i>Unreadable</span></div></div>
      <div class="map" id="map">${cells}</div>
      <canvas class="spark" id="spark" height="56"></canvas>
    </div>
    <details class="log" id="logBox"><summary>Activity log</summary><ol id="log"></ol></details>
  </div>`;
}

function setupJobView() {
  speed.samples = [];
  speed.points = [];
  speed.phase = "";
  const ol = $("#log");
  ol.innerHTML = S.jobLogs.map(logItem).join("");
}

function logItem(l) {
  return `<li class="${esc(l.level)}"><time>${esc(l.time)}</time><span>${esc(l.text)}</span></li>`;
}

function updateJob() {
  const j = S.job;
  if (!j || !$("#map")) return;
  const st = j.status || {};
  const total = st.total || 0;
  const running = j.state === "running";
  const frac = st.phase === "Finishing" ? 1 : total ? Math.min(1, st.done / total) : (j.state === "done" ? 1 : 0);

  // speed over the last ~8 seconds
  const now = performance.now();
  if (st.phase !== speed.phase) { speed.samples = []; speed.phase = st.phase; }
  speed.samples.push([now, st.done]);
  while (speed.samples.length > 2 && now - speed.samples[0][0] > 8000) speed.samples.shift();
  let rate = 0;
  if (speed.samples.length > 1) {
    const [t0, d0] = speed.samples[0];
    rate = ((st.done - d0) / (now - t0)) * 1000;
  }
  if (running && rate >= 0) { speed.points.push(rate); if (speed.points.length > 240) speed.points.shift(); }

  $("#phase").textContent = running ? (st.phase || "Starting") : { done: "Finished", failed: "Stopped with an error", cancelled: "Cancelled" }[j.state];
  $("#pct").innerHTML = `${Math.floor(frac * 100)}<small>%</small>`;
  $("#sDone").textContent = total ? `${bytes(st.done)} of ${bytes(total)}` : "-";
  $("#sSpeed").textContent = running && rate > 0 ? bytes(rate) + "/s" : "-";
  $("#sLeft").textContent = running && rate > 0 && total ? dur((total - st.done) / rate) : running ? "-" : dur(0);
  const badSectors = Math.round((st.bad || 0) / 512);
  $("#sBad").textContent = badSectors.toLocaleString();
  $("#sBadW").classList.toggle("bad", badSectors > 0);
  const bar = $("#bar");
  const fill = bar.firstElementChild;
  if (bar.dataset.phase !== st.phase) { // new phase starts from zero: no sliding back
    bar.dataset.phase = st.phase;
    fill.style.transition = "none";
    fill.style.width = (frac * 100).toFixed(2) + "%";
    void fill.offsetWidth;
    fill.style.transition = "";
  }
  bar.classList.toggle("verify", st.phase === "Verifying");
  fill.style.width = (frac * 100).toFixed(2) + "%";

  // sector map
  const cells = $("#map").children;
  const n = cells.length;
  const filled = Math.floor(frac * n);
  const verifying = st.phase === "Verifying";
  const readKind = ["scan", "check", "compare"].includes(j.kind);
  const base = readKind ? "r" : "c";
  const bad = new Set((st.badAt || []).map((o) => Math.min(n - 1, Math.floor((o / Math.max(total, 1)) * n))));
  for (let i = 0; i < n; i++) {
    let c;
    if (verifying) c = i < filled ? "v" : base;
    else if (st.phase === "Finishing" || j.state === "done") c = base;
    else c = i < filled ? base : i === filled && running ? "now" : "";
    if (bad.has(i)) c = "b";
    if (cells[i].className !== c) cells[i].className = c;
  }
  drawSpark();

  // log
  const ol = $("#log");
  if (ol && ol.children.length !== S.jobLogs.length) {
    ol.innerHTML = S.jobLogs.slice(-800).map(logItem).join("");
    ol.scrollTop = ol.scrollHeight;
  }

  // actions + result
  const actions = $("#jobActions");
  const wantActions = running ? "running" : "finished";
  if (actions.dataset.mode !== wantActions) {
    actions.dataset.mode = wantActions;
    actions.innerHTML = running
      ? `<button class="btn" data-act="cancel-job">Cancel</button>`
      : `<button class="btn primary" data-act="ack-job">Done</button>`;
  }
  const res = $("#jobResult");
  if (!running && !res.dataset.shown) {
    res.dataset.shown = "1";
    res.innerHTML = resultCard(j);
    if (j.state !== "running") $("#logBox").open = j.state === "failed";
  }
}

function resultCard(j) {
  const words = { clone: "Clone", backup: "Backup", restore: "Restore", compare: "Comparison", check: "Image check", scan: "Scan", wipe: "Wipe" }[j.kind] || j.title;
  const head = { done: `${words} finished`, failed: `${words} failed`, cancelled: `${words} cancelled` }[j.state];
  const cls = j.state === "done" ? "ok" : j.state === "failed" ? "fail" : "";
  const facts = (j.summary || []).length
    ? `<dl class="facts">${j.summary.map((r) => `<dt>${esc(r.label)}</dt><dd class="${esc(r.tone || "")} ${r.label === "SHA-256" ? "mono" : ""}">${esc(r.value)}</dd>`).join("")}</dl>` : "";
  const err = j.error ? `<p class="note err">${esc(j.error)}${j.hint ? `<span class="hint">${esc(j.hint)}</span>` : ""}</p>` : "";
  const partly = j.state !== "done" && ["clone", "restore"].includes(j.kind)
    ? `<p class="note">The target is only partly written. Its old partition table was kept, so Windows won't mistake it for a finished copy. Run the operation again before using it.</p>` : "";
  const removed = j.state !== "done" && j.kind === "backup" ? `<p class="muted">The unfinished image file was deleted.</p>` : "";
  const btns = [];
  if (j.output) btns.push(`<button class="btn" data-act="show-file" data-path="${esc(j.output)}">Show in folder</button>`);
  if (j.output && /\.vhd$/i.test(j.output) && isWin()) btns.push(`<button class="btn" data-act="mount-output" data-path="${esc(j.output)}">Mount to browse files</button>`);
  const shutdown = j.shutdown ? `<div class="banner" style="margin-top:14px"><span class="grow">The PC will shut down in about a minute.</span><button class="btn" data-act="cancel-shutdown">Cancel shutdown</button></div>` : "";
  return `<div class="result ${cls}"><h2>${head}</h2>${facts}${err}${partly}${removed}${shutdown}
    ${btns.length ? `<div class="row" style="margin-top:16px">${btns.join("")}</div>` : ""}</div>`;
}

function drawSpark() {
  const c = $("#spark");
  if (!c) return;
  const w = c.clientWidth, h = c.clientHeight;
  if (!w) return;
  const dpr = window.devicePixelRatio || 1;
  if (c.width !== Math.round(w * dpr)) { c.width = Math.round(w * dpr); c.height = Math.round(h * dpr); }
  const g = c.getContext("2d");
  g.setTransform(dpr, 0, 0, dpr, 0, 0);
  g.clearRect(0, 0, w, h);
  const pts = speed.points;
  if (pts.length < 2) return;
  const max = Math.max(...pts) * 1.15 || 1;
  const step = w / (Math.max(pts.length, 60) - 1); // fill from the left, then scroll
  const x0 = 0;
  const css = getComputedStyle(document.documentElement);
  g.beginPath();
  pts.forEach((v, i) => { const x = x0 + i * step, y = h - 2 - (v / max) * (h - 8); i ? g.lineTo(x, y) : g.moveTo(x, y); });
  g.strokeStyle = css.getPropertyValue("--accent").trim();
  g.lineWidth = 1.6;
  g.stroke();
  g.lineTo(x0 + (pts.length - 1) * step, h);
  g.lineTo(x0, h);
  g.closePath();
  g.globalAlpha = 0.12;
  g.fillStyle = g.strokeStyle;
  g.fill();
  g.globalAlpha = 1;
  g.fillStyle = css.getPropertyValue("--muted").trim();
  g.font = "11px " + css.getPropertyValue("--ui");
  g.fillText("peak " + bytes(Math.max(...pts)) + "/s", 4, 12);
}

let pollTimer = null;
async function pollJob() {
  clearTimeout(pollTimer);
  try {
    const since = S.job && S.jobLogsFor === S.job.id ? S.jobLogs.length : 0;
    const snap = await api("GET", "/api/job?since=" + since);
    if (snap) {
      if (S.jobLogsFor !== snap.id) { S.jobLogs = []; S.jobLogsFor = snap.id; }
      if (snap.logs) S.jobLogs.push(...snap.logs);
      const wasRunning = jobRunning();
      S.job = snap;
      if (wasRunning && snap.state !== "running") {
        S.showJob = true; // bring the result up wherever the user is
        loadDisks();
        notifyDone(snap);
      }
    }
    if (jobVisible()) render(); else renderNav();
  } catch (_) { /* backend restarting or gone; try again */ }
  pollTimer = setTimeout(pollJob, jobRunning() ? 600 : 4000);
}

function notifyDone(j) {
  document.title = (j.state === "done" ? "Finished" : j.state === "failed" ? "Failed" : "Cancelled") + " - DragonClone";
  setTimeout(() => { document.title = "DragonClone"; }, 15000);
}

// ---- modals -------------------------------------------------------------------------

let modalResolve = null;
function openModal(html, wide) {
  const root = $("#modal");
  root.innerHTML = `<div class="dialog ${wide ? "wide" : ""}" role="dialog" aria-modal="true">${html}</div>`;
  root.hidden = false;
  const first = root.querySelector("[autofocus], input, button.primary, button");
  if (first) first.focus();
  return new Promise((res) => { modalResolve = res; });
}
function closeModal(value) {
  $("#modal").hidden = true;
  $("#modal").innerHTML = "";
  const r = modalResolve;
  modalResolve = null;
  if (r) r(value);
}

function ask(title, body, okLabel, danger) {
  return openModal(`<header><h2>${esc(title)}</h2></header><div class="body">${body}</div>
    <footer><button class="btn" data-act="modal-cancel">Cancel</button>
    <button class="btn ${danger ? "danger" : "primary"}" data-act="modal-ok">${esc(okLabel)}</button></footer>`);
}

async function showLog() {
  let text = "";
  try { text = await api("GET", "/api/log"); } catch (e) { text = e.message; }
  openModal(`<header><h2>Log</h2><p class="muted small mono">${esc(S.info?.logFile || "")}</p></header>
    <div class="body"><pre>${esc(text || "Empty so far.")}</pre></div>
    <footer><button class="btn" data-act="copy-log">Copy</button><button class="btn primary" data-act="modal-cancel">Close</button></footer>`, true);
  $("#modal").dataset.log = text;
}

function showAbout() {
  const i = S.info || {};
  openModal(`<header><h2>DragonClone ${esc(i.version || "")}</h2></header>
    <div class="body">
      <p>Sector-by-sector cloning, imaging and restoring. Free and open source.</p>
      <dl class="facts">
        <dt>Running as administrator</dt><dd>${i.admin ? "Yes" : "No"}</dd>
        <dt>Settings and logs</dt><dd class="mono">${esc(i.dataDir || "")}</dd>
        <dt>Computer</dt><dd>${esc(i.host || "")}</dd>
      </dl>
      <p class="muted small" style="margin-top:16px">Always keep a second backup of anything important. Disk tools are powerful, and an overwritten disk can't be undone.</p>
    </div>
    <footer><button class="btn" data-act="view-log">Open the log</button><button class="btn primary" data-act="modal-cancel">Close</button></footer>`);
}

// Disk picker in a dialog (used by Compare).
function pickDiskModal() {
  const cards = S.disks.map((d) => diskCard(d, { pick: true, pickParts: true, disable: () => "" })).join("");
  return openModal(`<header><h2>Pick a disk or partition</h2></header><div class="body"><div class="disks">${cards}</div></div>
    <footer><button class="btn" data-act="modal-cancel">Cancel</button></footer>`, true);
}

// ---- file browser -------------------------------------------------------------------

const fb = { mode: "file", path: "", roots: [], entries: [], selected: "", parent: "", error: "" };

async function browse(mode, start) {
  fb.mode = mode;
  fb.selected = "";
  fb.error = "";
  try { fb.roots = await api("GET", "/api/roots"); } catch (_) { fb.roots = []; }
  let dir = start || settings.get(mode === "folder" ? "folder" : "imageDir", "") || (fb.roots[0] && fb.roots[0].path) || "/";
  if (mode === "file" && start && /\.[a-z0-9]+$/i.test(start)) dir = start.replace(/[\\/][^\\/]*$/, "") || dir;
  const p = openModal(`<header><h2>${mode === "folder" ? "Pick a folder" : "Pick an image file"}</h2></header>
    <div class="body"><div class="fb"><div class="fb-roots" id="fbRoots"></div>
    <div class="fb-body"><input type="text" id="fbPath" spellcheck="false"><div class="fb-list" id="fbList"></div></div></div></div>
    <footer><span class="muted small grow" id="fbNote"></span><button class="btn" data-act="modal-cancel">Cancel</button>
    <button class="btn primary" data-act="fb-ok" id="fbOk">${mode === "folder" ? "Use this folder" : "Open"}</button></footer>`, true);
  await fbLoad(dir);
  return p;
}

async function fbLoad(dir) {
  try {
    const r = await api("GET", "/api/ls?path=" + encodeURIComponent(dir));
    fb.path = r.path; fb.parent = r.parent; fb.entries = r.entries; fb.error = "";
  } catch (e) {
    fb.error = e.message;
    if (!fb.path) fb.entries = [];
  }
  fbRender();
}

function fbRender() {
  const roots = $("#fbRoots");
  if (!roots) return;
  roots.innerHTML = fb.roots.map((r) => `<button class="${fb.path.startsWith(r.path) ? "on" : ""}" data-act="fb-go" data-path="${esc(r.path)}">
    ${esc(r.path)} ${esc(r.label)}<small>${r.free >= 0 ? bytes(r.free) + " free" : ""}</small></button>`).join("");
  $("#fbPath").value = fb.path;
  const rows = [];
  if (fb.parent) rows.push(`<button class="fb-item dir" data-act="fb-go" data-path="${esc(fb.parent)}">${icon("folder")}<span>..</span><small></small></button>`);
  for (const e of fb.entries) {
    const full = fb.path.replace(/[\\/]+$/, "") + sep() + e.name;
    if (e.dir) rows.push(`<button class="fb-item dir" data-act="fb-go" data-path="${esc(full)}">${icon("folder")}<span>${esc(e.name)}</span><small></small></button>`);
    else if (fb.mode === "file") rows.push(`<button class="fb-item ${e.image ? "" : "dim"} ${fb.selected === full ? "on" : ""}" data-act="fb-pick" data-path="${esc(full)}">${icon("file")}<span>${esc(e.name)}</span><small>${bytes(e.size)}</small></button>`);
    else if (e.image) rows.push(`<div class="fb-item dim">${icon("file")}<span>${esc(e.name)}</span><small>${bytes(e.size)}</small></div>`);
  }
  $("#fbList").innerHTML = rows.join("") || `<p class="muted" style="padding:12px">This folder is empty.</p>`;
  $("#fbNote").textContent = fb.error || (fb.mode === "file" ? (fb.selected ? fb.selected : "Image files: .img .img.gz .vhd .iso") : "");
  $("#fbOk").disabled = fb.mode === "file" && !fb.selected;
}

// ---- events -------------------------------------------------------------------------

document.addEventListener("click", async (ev) => {
  const el = ev.target.closest("[data-act]");
  if (!el || el.disabled) return;
  const act = el.dataset.act;
  const f = S.flow;
  switch (act) {
    case "nav": go(el.dataset.id); break;
    case "show-job": S.showJob = true; render(); break;
    case "noop": break;
    case "refresh": await loadDisks(); render(); break;
    case "theme": cycleTheme(); break;
    case "about": showAbout(); break;
    case "view-log": showLog(); break;
    case "copy-log": navigator.clipboard?.writeText($("#modal").dataset.log || "").then(() => toast("Log copied")); break;
    case "modal-cancel": closeModal(false); break;
    case "modal-ok": closeModal(true); break;

    case "start-flow": {
      const kind = el.dataset.kind;
      S.view = ["clone", "backup", "restore"].includes(kind) ? kind : "tool:" + kind;
      newFlow(kind, { source: el.dataset.src || null, target: el.dataset.tgt || null });
      render();
      break;
    }
    case "pick": {
      if ($("#modal").hidden === false && modalResolve) { closeModal(el.dataset.id); break; }
      const key = STEPS[f.kind][f.step];
      f[key] = el.dataset.id;
      if (key === "source" && f.kind === "backup") f.filename = defaultImageName(f.source);
      if (key === "source" && f.kind === "clone") f.target = null;
      f.step++;
      f.plan = null;
      render();
      break;
    }
    case "goto-step":
      f.step = Number(el.dataset.step);
      f.plan = null; f.planError = null; f.startError = null;
      render();
      break;
    case "format":
      f.format = el.dataset.id;
      settings.set("format", f.format);
      render();
      break;
    case "wipemode": f.wipeMode = el.dataset.id; render(); break;
    case "next": {
      const key = STEPS[f.kind][f.step];
      if (key === "image-out") {
        f.folder = $("#folder").value.trim();
        f.filename = $("#filename").value.trim();
        if (!imageOutPath(f)) { toast("Pick a folder and a file name first", true); break; }
        settings.set("folder", f.folder);
      }
      f.step++;
      f.plan = null; f.planError = null; f.startError = null;
      render();
      break;
    }
    case "browse-folder": {
      const folder = await browse("folder", $("#folder").value.trim());
      if (folder) { f.folder = folder; f.filename = $("#filename")?.value ?? f.filename; settings.set("folder", folder); render(); }
      break;
    }
    case "browse-image": {
      const file = await browse("file", $("#imagePath").value.trim());
      if (file) { f.imagePath = file; f.image = null; f.imageError = null; f.mounted = ""; render(); if (f.kind !== "mount") inspectImage(); }
      break;
    }
    case "inspect":
      f.imagePath = $("#imagePath").value.trim();
      if (f.image && f.image.path === f.imagePath) { f.step++; render(); break; }
      inspectImage(true);
      break;
    case "pair-disk": {
      const id = await pickDiskModal();
      if (id) { f[el.dataset.side] = { value: id, label: findItem(id) ? `${id} - ${findItem(id).kind === "disk" ? findItem(id).disk.model : partLabel(findItem(id).part)}, ${bytes(findItem(id).size)}` : id }; render(); }
      break;
    }
    case "pair-image": {
      const file = await browse("file", "");
      if (file) { f[el.dataset.side] = { value: file, label: file }; render(); }
      break;
    }
    case "fb-go": fb.selected = ""; fbLoad(el.dataset.path); break;
    case "fb-pick": {
      if (fb.selected === el.dataset.path && ev.detail >= 2) { closeModal(fb.selected); break; }
      fb.selected = el.dataset.path;
      fbRender();
      break;
    }
    case "fb-ok":
      if (fb.mode === "folder") { closeModal(fb.path); }
      else if (fb.selected) { settings.set("imageDir", fb.path); closeModal(fb.selected); }
      break;
    case "replan": loadPlan(); break;
    case "start": if (canStart()) startJob(); break;
    case "cancel-job": {
      const ok = await ask("Cancel this operation?", `<p>${["clone", "restore"].includes(S.job.kind)
        ? "The target will be left partly written. Its old partition table is kept, so it won't be mistaken for a finished copy."
        : S.job.kind === "backup" ? "The unfinished image file will be deleted." : "You can run it again later."}</p>`, "Cancel operation", true);
      if (ok) { await api("POST", "/api/cancel").catch(() => {}); toast("Stopping after the current block..."); }
      break;
    }
    case "ack-job":
      S.ackedJob = S.job.id;
      S.showJob = false;
      if (["clone", "backup", "restore"].includes(S.view)) newFlow(S.view);
      else if (S.view.startsWith("tool:")) S.view = "tools";
      render();
      break;
    case "show-file": api("POST", "/api/show", { path: el.dataset.path }).catch((e) => toast(e.message, true)); break;
    case "mount-output":
      el.disabled = true;
      api("POST", "/api/mount", { path: el.dataset.path })
        .then((r) => toast("Mounted" + (r.letters ? " as " + r.letters : "") + " (read-only)"))
        .catch((e) => toast(e.message, true)).finally(() => { el.disabled = false; });
      break;
    case "cancel-shutdown":
      api("POST", "/api/cancel-shutdown").then(() => { toast("Shutdown cancelled"); S.job.shutdown = false; $("#jobResult").dataset.shown = ""; updateJob(); })
        .catch((e) => toast(e.message, true));
      break;
    case "mount":
    case "unmount": {
      f.imagePath = $("#imagePath").value.trim();
      if (!f.imagePath) { toast("Pick an image first", true); break; }
      f.busy = true; f.imageError = null; render();
      try {
        const r = await api("POST", "/api/" + act, { path: f.imagePath });
        f.mounted = act === "mount" ? (r.letters || " ") : "";
        if (act === "unmount") toast("Unmounted");
      } catch (e) { f.imageError = e.message; }
      f.busy = false; render();
      break;
    }
  }
});

document.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape" && !$("#modal").hidden) closeModal(false);
  if (ev.key === "Enter" && ev.target.id === "fbPath") { fb.selected = ""; fbLoad(ev.target.value.trim()); }
  if (ev.key === "Enter" && ev.target.matches(".disk.pickable")) ev.target.click();
});

document.addEventListener("change", (ev) => {
  const t = ev.target;
  const f = S.flow;
  if (t.dataset.opt && f) {
    if (t.dataset.opt === "level") f.level = t.checked ? 6 : 1;
    else f[t.dataset.opt] = t.checked;
    if (t.dataset.opt === "verify") settings.set("verify", t.checked);
    if (t.dataset.opt === "level") settings.set("level", f.level);
  }
  if (t.id === "accept" && f) { f.accept = t.checked; afterRender(); }
});

document.addEventListener("input", (ev) => {
  const t = ev.target;
  const f = S.flow;
  if (!f) return;
  if (t.id === "confirm") { f.confirm = t.value; afterRender(); }
  if (t.id === "folder" || t.id === "filename") {
    f[t.id] = t.value;
    const out = $("#outpath");
    if (out) out.textContent = imageOutPath(f) || "Pick a folder and a name.";
  }
  if (t.id === "imagePath") { f.imagePath = t.value; }
});

async function inspectImage(advance) {
  const f = S.flow;
  if (!f.imagePath) { f.imageError = "Pick an image file first."; render(); return; }
  f.busy = true; f.imageError = null; render();
  try {
    f.image = await api("POST", "/api/inspect", { path: f.imagePath });
    if (advance) f.step++;
  } catch (e) {
    f.image = null;
    f.imageError = e.message;
  }
  f.busy = false;
  render();
}

// ---- theme ---------------------------------------------------------------------------

function applyTheme() {
  const t = settings.get("theme", "system");
  const dark = t === "dark" || (t === "system" && matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.dataset.theme = dark ? "dark" : "light";
  $("#themeBtn").textContent = { system: "Theme: auto", dark: "Theme: dark", light: "Theme: light" }[t];
}
function cycleTheme() {
  const order = ["system", "dark", "light"];
  settings.set("theme", order[(order.indexOf(settings.get("theme", "system")) + 1) % 3]);
  applyTheme();
  if (jobVisible()) drawSpark();
}
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", applyTheme);

// ---- start-up --------------------------------------------------------------------------

// Keep the backend alive while the window is open, and tell it when we leave.
setInterval(() => { if (!jobRunning()) api("POST", "/api/ping").catch(() => {}); }, 15000);
window.addEventListener("pagehide", () => {
  fetch("/api/bye", { method: "POST", keepalive: true, headers: { "X-DragonClone-Token": token } }).catch(() => {});
});
window.addEventListener("beforeunload", (ev) => {
  if (jobRunning()) { ev.preventDefault(); ev.returnValue = ""; }
});

(async function init() {
  applyTheme();
  try {
    S.info = await api("GET", "/api/info");
  } catch (e) {
    $("#main").innerHTML = `<div class="page"><h1>Can't reach DragonClone</h1><p class="lede">${esc(e.message)}. Close this window and start DragonClone again.</p></div>`;
    return;
  }
  await loadDisks();
  await pollJob(); // picks up a job that kept running while the window was closed
  if (S.job && S.job.state !== "running") S.ackedJob = S.job.id;
  S.showJob = jobRunning();
  render();
})();
