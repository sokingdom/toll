// toll dashboard. Plain JS, no build step; served from the binary.
"use strict";

const view = document.getElementById("view");
const tz = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
let timer = null;

// ---------- helpers ----------

// Every value from the API is escaped: user IDs, models and tags come from
// callers and must never be interpreted as HTML.
function esc(v) {
  return String(v ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
const num = n => (n == null ? "–" : Number(n).toLocaleString());
const compact = n => (n == null ? "–" : Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 }).format(n));
const pad = n => String(n).padStart(2, "0");
const ymd = d => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
const parseYMD = s => { const [y, m, d] = s.split("-").map(Number); return new Date(y, m - 1, d); };
const addDays = (d, n) => new Date(d.getFullYear(), d.getMonth(), d.getDate() + n);
const time = ms => new Date(ms).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" });

function since(ms) {
  let s = Math.max(0, Math.floor((Date.now() - ms) / 1000));
  const d = Math.floor(s / 86400); s %= 86400;
  const h = Math.floor(s / 3600); s %= 3600;
  const m = Math.floor(s / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${s % 60}s`;
  return `${s}s`;
}

function bytes(n) {
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${u[i]}`;
}

async function api(path, params = {}) {
  const q = new URLSearchParams(Object.entries(params).filter(([, v]) => v !== "" && v != null));
  const res = await fetch(`/api/${path}${q.toString() ? "?" + q : ""}`);
  if (!res.ok) throw new Error((await res.text()) || res.statusText);
  return res.json();
}

function statusBadge(r) {
  if (r.status == null) return `<span class="badge warn">open</span>`;
  const cls = r.status >= 200 && r.status < 300 ? "ok" : r.status === 499 ? "warn" : "err";
  return `<span class="badge ${cls}">${esc(r.status)}</span>`;
}

const userLink = u => (u == null ? `<span class="muted">–</span>` : `<a href="#/users/${encodeURIComponent(u)}">${esc(u)}</a>`);
const modelOf = r => r.model ?? r.requested_model;

function flags(r) {
  const f = [];
  if (r.stream) f.push("stream");
  if (r.partial) f.push("partial");
  if (r.rejected) f.push("rejected");
  if (r.usage_source === "missing") f.push("no usage");
  return f.map(x => `<span class="flag">${x}</span>`).join("") + (r.error ? ` <span class="muted">${esc(r.error)}</span>` : "");
}

// ---------- request table (Requests page, user page, open requests) ----------

const reqHead = `<tr><th>Time</th><th>User</th><th>Provider</th><th>Model</th><th>Key</th><th>Status</th>
  <th class="num">Input</th><th class="num">Output</th><th class="num">Cache read</th><th class="num">Reasoning</th>
  <th class="num">Latency</th><th></th></tr>`;

function reqRow(r) {
  const key = r.key_alias ?? r.key_id;
  return `<tr class="row" data-id="${esc(r.request_id)}">
    <td title="${esc(r.request_id)}">${time(r.started_at)}</td>
    <td>${userLink(r.user)}</td>
    <td>${esc(r.provider)}</td>
    <td>${esc(modelOf(r) ?? "–")}</td>
    <td class="mono">${key ? esc(key) : "–"}</td>
    <td>${statusBadge(r)}</td>
    <td class="num">${num(r.input_tokens)}</td>
    <td class="num">${num(r.output_tokens)}</td>
    <td class="num">${num(r.cache_read_tokens)}</td>
    <td class="num">${num(r.reasoning_tokens)}</td>
    <td class="num">${r.latency_ms == null ? since(r.started_at) : num(r.latency_ms) + " ms"}</td>
    <td>${flags(r)}</td>
  </tr>`;
}

// requestTable renders a paged, newest-first list into el.
function requestTable(el, params) {
  let next = "";
  const rows = new Map();
  el.innerHTML = `<div class="panel"><table><thead>${reqHead}</thead><tbody></tbody></table></div><div class="more"></div>`;
  const tbody = el.querySelector("tbody");
  const more = el.querySelector(".more");

  tbody.addEventListener("click", e => {
    if (e.target.closest("a")) return;
    const tr = e.target.closest("tr.row");
    if (!tr) return;
    if (tr.nextElementSibling?.classList.contains("detail")) { tr.nextElementSibling.remove(); return; }
    const d = document.createElement("tr");
    d.className = "detail";
    d.innerHTML = `<td colspan="12">${esc(JSON.stringify(rows.get(tr.dataset.id), null, 2))}</td>`;
    tr.after(d);
  });

  async function load() {
    more.innerHTML = `<span class="muted">Loading…</span>`;
    try {
      const res = await api("requests", { ...params, before: next });
      for (const r of res.requests) rows.set(r.request_id, r);
      tbody.insertAdjacentHTML("beforeend", res.requests.map(reqRow).join(""));
      if (!tbody.children.length) tbody.innerHTML = `<tr><td class="empty" colspan="12">No requests</td></tr>`;
      next = res.next;
      more.innerHTML = next ? `<button>Load older</button>` : rows.size ? `<span class="muted">End of kept history</span>` : "";
      more.querySelector("button")?.addEventListener("click", load);
    } catch (err) {
      more.innerHTML = `<div class="error">${esc(err.message)}</div>`;
    }
  }
  load();
}

// ---------- Overview ----------

function group(rows, field) {
  const out = new Map();
  for (const r of rows) {
    const k = r[field];
    const g = out.get(k) ?? { name: k, requests: 0, errors: 0, input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0 };
    g.requests += r.requests;
    g.errors += r.errors;
    for (const t of ["input", "output", "cache_read", "cache_write", "reasoning"]) g[t] += r.tokens[t] ?? 0;
    out.set(k, g);
  }
  return [...out.values()].sort((a, b) => b.requests - a.requests);
}

function groupTable(title, rows) {
  const body = rows.length
    ? rows.map(g => `<tr><td>${esc(g.name)}</td><td class="num">${num(g.requests)}</td><td class="num">${num(g.errors)}</td>
        <td class="num">${num(g.input)}</td><td class="num">${num(g.output)}</td><td class="num">${num(g.cache_read)}</td>
        <td class="num">${num(g.reasoning)}</td></tr>`).join("")
    : `<tr><td class="empty" colspan="7">No requests yet</td></tr>`;
  return `<div><h2>${title}</h2><div class="panel"><table><thead><tr><th>Name</th><th class="num">Requests</th>
    <th class="num">Errors</th><th class="num">Input</th><th class="num">Output</th><th class="num">Cache read</th>
    <th class="num">Reasoning</th></tr></thead><tbody>${body}</tbody></table></div></div>`;
}

function openTable(rows, empty) {
  return `<div class="panel"><table><thead>${reqHead}</thead><tbody>${
    rows.length ? rows.map(reqRow).join("") : `<tr><td class="empty" colspan="12">${empty}</td></tr>`
  }</tbody></table></div>`;
}

async function overview() {
  const [o, open] = await Promise.all([api("overview"), api("open")]);
  const s = o.summary;
  const sum = (f) => s.rows.reduce((a, r) => a + f(r), 0);
  const requests = sum(r => r.requests);
  const errors = sum(r => r.errors);
  const tok = t => sum(r => r.tokens[t] ?? 0);
  const card = (label, value, cls = "") => `<div class="card ${cls}"><div class="label">${label}</div><div class="value">${value}</div></div>`;
  const st = o.store;

  view.innerHTML = `
    <h2>Since toll started</h2>
    <p class="sub">Started ${time(o.started_at)} · up ${since(o.started_at)} · counters reset on restart</p>
    <div class="cards">
      ${card("Requests", compact(requests))}
      ${card("Errors", compact(errors), errors ? "err" : "")}
      ${card("In flight", num(s.inflight))}
      ${card("Input tokens", compact(tok("input")))}
      ${card("Output tokens", compact(tok("output")))}
      ${card("Cache read", compact(tok("cache_read")))}
      ${card("Reasoning", compact(tok("reasoning")))}
      ${card("Usage missing", num(s.usage_missing), s.usage_missing ? "warn" : "")}
      ${card("Event write errors", num(s.write_errors), s.write_errors ? "err" : "")}
    </div>
    <div class="grid2">
      ${groupTable("By provider", group(s.rows, "provider"))}
      ${groupTable("By model", group(s.rows, "model"))}
      ${groupTable("By key", group(s.rows, "key"))}
      <div><h2>By status</h2><div class="panel"><table><thead><tr><th>Status</th><th class="num">Requests</th></tr></thead><tbody>${
        Object.keys(s.status).length
          ? Object.entries(s.status).sort().map(([k, v]) => `<tr><td>${statusBadge({ status: Number(k) })}</td><td class="num">${num(v)}</td></tr>`).join("")
          : `<tr><td class="empty" colspan="2">No requests yet</td></tr>`
      }</tbody></table></div></div>
    </div>
    <h2>In flight</h2>
    ${openTable(open.in_flight, "Nothing in flight")}
    <h2>Lost</h2>
    <p class="sub">Started but never completed, usually because a previous toll process was killed. Check the event files for these requests.</p>
    ${openTable(open.lost, "No lost requests")}
    <h2>Dashboard store</h2>
    <div class="panel"><dl class="kv">
      <dt>File</dt><dd>${esc(st.path)} (${bytes(st.size_bytes)})</dd>
      <dt>Retention</dt><dd>${esc(st.retention)}</dd>
      <dt>Oldest request</dt><dd>${st.oldest ? time(st.oldest) : "–"}</dd>
      <dt>Dropped / write errors</dt><dd>${num(st.dropped)} / ${num(st.write_errors)}</dd>
    </dl></div>`;
}

// ---------- Requests ----------

function requestsPage(query) {
  const p = Object.fromEntries(new URLSearchParams(query));
  view.innerHTML = `
    <h2>Requests</h2>
    <p class="sub">Newest first, from the dashboard store. Click a row for all fields.</p>
    <form class="filters">
      <input name="user" placeholder="User" value="${esc(p.user)}">
      <input name="provider" placeholder="Provider" value="${esc(p.provider)}">
      <input name="model" placeholder="Model" value="${esc(p.model)}">
      <select name="status">
        <option value="">All statuses</option>
        <option value="ok" ${p.status === "ok" ? "selected" : ""}>Success (2xx)</option>
        <option value="error" ${p.status === "error" ? "selected" : ""}>Errors</option>
      </select>
      <button class="primary">Apply</button>
    </form>
    <div id="list"></div>`;
  view.querySelector("form").addEventListener("submit", e => {
    e.preventDefault();
    const q = new URLSearchParams([...new FormData(e.target)].filter(([, v]) => v));
    location.hash = `#/requests${q.toString() ? "?" + q : ""}`;
  });
  requestTable(document.getElementById("list"), p);
}

// ---------- Users ----------

function usersPage() {
  view.innerHTML = `
    <h2>Users</h2>
    <p class="sub">Look up one end user's usage by the ID your service sends in X-Toll-User or the provider user field.</p>
    <form class="filters"><input name="user" placeholder="User ID" size="40" autofocus><button class="primary">Look up</button></form>`;
  view.querySelector("form").addEventListener("submit", e => {
    e.preventDefault();
    const u = new FormData(e.target).get("user").trim();
    if (u) location.hash = `#/users/${encodeURIComponent(u)}`;
  });
}

function chart(data) {
  const W = 1000, H = 200, top = 10, bottom = 22;
  const max = Math.max(1, ...data.series.map(b => b.input_tokens + b.output_tokens));
  const bw = W / data.series.length;
  const label = ms => {
    const d = new Date(ms);
    return data.bucket === "hour" ? `${pad(d.getHours())}` : `${d.getMonth() + 1}/${d.getDate()}`;
  };
  const step = Math.ceil(data.series.length / 16);
  const bars = data.series.map((b, i) => {
    const hin = (b.input_tokens / max) * (H - top - bottom);
    const hout = (b.output_tokens / max) * (H - top - bottom);
    const x = i * bw + bw * 0.15, w = bw * 0.7, base = H - bottom;
    const title = `${label(b.start)} · ${num(b.requests)} requests · ${num(b.input_tokens)} in · ${num(b.output_tokens)} out`;
    return `<g><title>${esc(title)}</title>
      <rect class="bar-in" x="${x}" y="${base - hin}" width="${w}" height="${hin}"></rect>
      <rect class="bar-out" x="${x}" y="${base - hin - hout}" width="${w}" height="${hout}"></rect>
      ${i % step === 0 ? `<text x="${x + w / 2}" y="${H - 6}" text-anchor="middle">${label(b.start)}</text>` : ""}</g>`;
  }).join("");
  return `<div class="chart"><div class="legend"><span><i style="background:#fcd690"></i>Input</span>
    <span><i style="background:var(--accent)"></i>Output</span><span>max ${compact(max)} tokens per ${data.bucket}</span></div>
    <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">${bars}</svg></div>`;
}

async function userPage(user, query) {
  const p = Object.fromEntries(new URLSearchParams(query));
  const mode = p.mode || "day";
  const today = new Date();
  let from, to;
  if (mode === "week") {
    const anchor = p.date ? parseYMD(p.date) : today;
    from = addDays(anchor, -((anchor.getDay() + 6) % 7)); // Monday
    to = addDays(from, 6);
  } else if (mode === "custom") {
    from = p.from ? parseYMD(p.from) : addDays(today, -6);
    to = p.to ? parseYMD(p.to) : today;
  } else {
    from = to = p.date ? parseYMD(p.date) : today;
  }

  const go = params => { location.hash = `#/users/${encodeURIComponent(user)}?${new URLSearchParams(params)}`; };
  const shift = n => go({ mode, date: ymd(addDays(from, n)) });

  view.innerHTML = `
    <h2>${esc(user)}</h2>
    <p class="sub">Times in ${esc(tz)}</p>
    <div class="controls">
      <div class="seg">
        <button data-mode="day" class="${mode === "day" ? "on" : ""}">Day</button>
        <button data-mode="week" class="${mode === "week" ? "on" : ""}">Week</button>
        <button data-mode="custom" class="${mode === "custom" ? "on" : ""}">Custom</button>
      </div>
      ${mode === "custom"
        ? `<input type="date" id="from" value="${ymd(from)}"> – <input type="date" id="to" value="${ymd(to)}"> <button id="apply" class="primary">Apply</button>`
        : `<button id="prev">‹</button><input type="date" id="date" value="${ymd(from)}"><button id="next">›</button>
           <span class="muted">${mode === "week" ? `${ymd(from)} – ${ymd(to)}` : ""}</span>`}
    </div>
    <div id="usage"><span class="muted">Loading…</span></div>
    <h2>Requests</h2>
    <div id="list"></div>`;

  view.querySelectorAll(".seg button").forEach(b => b.addEventListener("click", () => go({ mode: b.dataset.mode })));
  if (mode === "custom") {
    view.querySelector("#apply").addEventListener("click", () => go({ mode, from: view.querySelector("#from").value, to: view.querySelector("#to").value }));
  } else {
    const n = mode === "week" ? 7 : 1;
    view.querySelector("#prev").addEventListener("click", () => shift(-n));
    view.querySelector("#next").addEventListener("click", () => shift(n));
    view.querySelector("#date").addEventListener("change", e => e.target.value && go({ mode, date: e.target.value }));
  }

  const el = document.getElementById("usage");
  try {
    const u = await api("usage", { user, from: ymd(from), to: ymd(to), tz });
    const t = u.total;
    const card = (label, value) => `<div class="card"><div class="label">${label}</div><div class="value">${value}</div></div>`;
    el.innerHTML = `
      <div class="cards">
        ${card("Requests", num(t.requests))}
        ${card("Errors", num(t.errors))}
        ${card("Input tokens", num(t.input_tokens))}
        ${card("Output tokens", num(t.output_tokens))}
        ${card("Cache read", num(t.cache_read_tokens))}
        ${card("Cache write", num(t.cache_write_tokens))}
        ${card("Reasoning", num(t.reasoning_tokens))}
      </div>
      <h2>${u.bucket === "hour" ? "By hour" : "By day"}</h2>
      ${chart(u)}
      <h2>By model</h2>
      <div class="panel"><table><thead><tr><th>Model</th><th class="num">Requests</th><th class="num">Errors</th><th class="num">Input</th>
        <th class="num">Output</th><th class="num">Cache read</th><th class="num">Cache write</th><th class="num">Reasoning</th></tr></thead><tbody>${
        u.models.length
          ? u.models.map(m => `<tr><td>${esc(m.model || "unknown")}</td><td class="num">${num(m.requests)}</td><td class="num">${num(m.errors)}</td>
              <td class="num">${num(m.input_tokens)}</td><td class="num">${num(m.output_tokens)}</td><td class="num">${num(m.cache_read_tokens)}</td>
              <td class="num">${num(m.cache_write_tokens)}</td><td class="num">${num(m.reasoning_tokens)}</td></tr>`).join("")
          : `<tr><td class="empty" colspan="8">No requests in this period</td></tr>`
      }</tbody></table></div>`;
    requestTable(document.getElementById("list"), { user, since: u.since, until: u.until });
  } catch (err) {
    el.innerHTML = `<div class="error">${esc(err.message)}</div>`;
  }
}

// ---------- Config ----------

function kv(obj) {
  return `<div class="panel"><dl class="kv">${Object.entries(obj).map(([k, v]) =>
    `<dt>${esc(k)}</dt><dd>${esc(typeof v === "object" ? JSON.stringify(v) : v)}</dd>`).join("")}</dl></div>`;
}

async function configPage() {
  const c = await api("config");
  c.server.auth_token = c.server.auth_token ? "set (hidden)" : "not set";
  const providers = Object.entries(c.providers).sort();
  const aliases = Object.entries(c.key_aliases ?? {});
  view.innerHTML = `
    <h2>Server</h2>${kv(c.server)}
    <h2>Providers</h2>
    <div class="panel"><table><thead><tr><th>Route</th><th>Upstream</th><th>Format</th></tr></thead><tbody>${
      providers.map(([n, p]) => `<tr><td class="mono">/${esc(n)}/</td><td class="mono">${esc(p.upstream)}</td><td>${esc(p.format)}</td></tr>`).join("")
    }</tbody></table></div>
    <h2>Metering</h2>${kv(c.metering)}
    <h2>Key aliases</h2>
    <div class="panel"><table><thead><tr><th>Key ID</th><th>Alias</th></tr></thead><tbody>${
      aliases.length ? aliases.map(([k, v]) => `<tr><td class="mono">${esc(k)}</td><td>${esc(v)}</td></tr>`).join("")
        : `<tr><td class="empty" colspan="2">No aliases</td></tr>`
    }</tbody></table></div>
    <h2>Events</h2>${kv(c.events)}
    <h2>Dashboard</h2>${kv(c.dashboard)}`;
}

// ---------- router ----------

async function route() {
  clearInterval(timer);
  timer = null;
  const [path, query = ""] = location.hash.replace(/^#/, "").split("?");
  const parts = path.split("/").filter(Boolean);
  const page = parts[0] || "overview";
  document.querySelectorAll("nav a").forEach(a => a.classList.toggle("active", a.dataset.nav === page));
  try {
    switch (page) {
      case "requests": requestsPage(query); break;
      case "users": parts[1] ? await userPage(decodeURIComponent(parts.slice(1).join("/")), query) : usersPage(); break;
      case "config": await configPage(); break;
      default:
        await overview();
        timer = setInterval(() => overview().catch(() => {}), 5000);
    }
  } catch (err) {
    view.innerHTML = `<div class="error">${esc(err.message)}</div>`;
  }
}

api("overview").then(o => {
  document.getElementById("instance").textContent = `instance ${o.instance}`;
}).catch(() => {});
window.addEventListener("hashchange", route);
route();
