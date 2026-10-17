"use strict";

const $ = (sel, root = document) => root.querySelector(sel);
const el = (tag, attrs = {}, ...children) => {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") n.className = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) n.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    n.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return n;
};

const STATE_LABEL = { approval: "需授权", input: "等待输入", error: "出错", running: "进行中", done: "已完成", idle: "空闲" };
const STATE_ORDER = ["approval", "input", "error", "running", "done", "idle"];
const PROVIDER_NAME = { claude: "Claude", codex: "Codex" };

let state = null;
let previewPage = 0;

// ---------- formatting ----------

function toDate(v) {
  if (!v) return null;
  const d = typeof v === "number" ? new Date(v * 1000) : new Date(v);
  return isNaN(d) || d.getFullYear() < 2000 ? null : d;
}

function fmtDuration(sec) {
  sec = Math.max(0, Math.round(sec));
  if (sec < 60) return `${sec}秒`;
  const m = Math.floor(sec / 60);
  if (m < 60) return `${m}分钟`;
  const h = Math.floor(m / 60);
  if (h < 24) return m % 60 ? `${h}小时${m % 60}分` : `${h}小时`;
  const d = Math.floor(h / 24);
  return h % 24 ? `${d}天${h % 24}小时` : `${d}天`;
}

function ago(v, now = Date.now()) {
  const d = toDate(v);
  if (!d) return "—";
  const s = (now - d.getTime()) / 1000;
  return s < 5 ? "刚刚" : `${fmtDuration(s)}前`;
}

function until(v, now = Date.now()) {
  const d = toDate(v);
  if (!d) return "";
  const s = (d.getTime() - now) / 1000;
  return s <= 0 ? "已重置" : `${fmtDuration(s)}后重置`;
}

function fmtTokens(n) {
  if (!n) return "0";
  if (n >= 1e9) return (n / 1e9).toFixed(2) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(n >= 1e7 ? 1 : 2) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(n >= 1e4 ? 0 : 1) + "K";
  return String(n);
}

const totalTokens = (t) => (t ? t.input + t.output + t.cache_read + t.cache_create : 0);
const level = (pct) => (pct >= 90 ? "high" : pct >= 70 ? "mid" : "");
const winLabel = (w, fallback) => {
  const m = w && w.window_minutes;
  if (!m) return fallback;
  return m % 1440 === 0 ? `${m / 1440}d` : m % 60 === 0 ? `${m / 60}h` : `${m}m`;
};

// ---------- API ----------

async function api(path, opts = {}) {
  const init = { method: opts.method || "GET", headers: {} };
  if (opts.body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(opts.body);
  }
  const res = await fetch(path, init);
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) throw new Error((data && data.error) || res.statusText);
  return data;
}

// ---------- tabs ----------

function showTab(name) {
  for (const b of document.querySelectorAll(".tabs button")) {
    b.setAttribute("aria-selected", String(b.dataset.tab === name));
  }
  for (const s of document.querySelectorAll(".tab")) s.hidden = s.id !== `tab-${name}`;
  history.replaceState(null, "", `#/${name}`); // "#/x" never matches an element id, so no scroll jump
  try { localStorage.setItem("tab", name); } catch {}
  if (name === "integrations") loadIntegrations();
  if (name === "settings") { loadSettings(); loadSystem(); }
  if (name === "logs") loadLogs();
  if (name === "preview") fitScreen();
}

for (const b of document.querySelectorAll(".tabs button")) b.addEventListener("click", () => showTab(b.dataset.tab));

// ---------- overview ----------

function renderMeter(label, w, now) {
  const pct = w ? w.pct : null;
  return el("div", { class: "meter" },
    el("span", { class: "label" }, label),
    el("div", { class: "bar" }, el("div", { class: `fill ${level(pct || 0)}`, style: `width:${Math.min(100, pct || 0)}%` })),
    el("span", { class: "pct" }, pct === null ? "—" : `${Math.round(pct)}%`),
    w && toDate(w.resets_at) ? el("span", { class: "reset" }, until(w.resets_at, now)) : null,
  );
}

function renderUsage(snap) {
  const now = new Date(snap.now).getTime();
  const box = $("#usage");
  box.replaceChildren();
  for (const p of ["claude", "codex"]) {
    const u = (snap.usage && snap.usage[p]) || {};
    const updated = toDate(u.limits_updated);
    const today = u.today || null;
    box.append(el("div", { class: "panel usage-card" },
      el("h3", {}, el("span", { class: "dot", style: `background:var(--${p})` }), PROVIDER_NAME[p],
        el("span", { class: "meta" }, updated ? `额度更新于 ${ago(updated, now)}（${u.limits_source}）` : "暂无额度数据")),
      renderMeter(winLabel(u.five_hour, "5h"), u.five_hour, now),
      renderMeter(winLabel(u.seven_day, "7d"), u.seven_day, now),
      el("div", { class: "tokens" },
        el("div", {}, el("b", {}, fmtTokens(totalTokens(today))), el("span", {}, "今日 token")),
        today && today.has_cost ? el("div", {}, el("b", {}, `$${today.cost_usd.toFixed(2)}`), el("span", {}, "今日 API 等价费用")) : null,
        el("div", {}, el("b", {}, fmtTokens(totalTokens(u.week))), el("span", {}, "近 7 天 token")),
      ),
    ));
  }
}

function renderSessions(snap) {
  const showIdle = $("#show-idle").checked;
  const now = new Date(snap.now).getTime();
  const counts = $("#counts");
  counts.replaceChildren(...STATE_ORDER.filter((s) => snap.counts && snap.counts[s])
    .map((s) => el("span", { class: `badge ${s}` }, `${STATE_LABEL[s]} ${snap.counts[s]}`)));

  const list = $("#sessions");
  const rows = (snap.sessions || []).filter((s) => showIdle || effectiveState(s) !== "idle");
  if (!rows.length) {
    list.replaceChildren(el("div", { class: "empty" },
      snap.sessions && snap.sessions.length ? "没有活跃任务（勾选「显示空闲会话」查看全部）" : "暂无会话。启动 Claude Code 或 Codex 后会出现在这里。"));
    return;
  }
  list.replaceChildren(...rows.map((s) => el("div", { class: "group" },
    el("div", { class: `session ${s.state}`, title: s.cwd || "" },
      el("span", {}, el("span", { class: `badge ${s.state}` }, STATE_LABEL[s.state] || s.state)),
      el("span", { class: `prov ${s.provider}` }, PROVIDER_NAME[s.provider] || s.provider),
      el("span", { class: "project" }, s.project || s.session_id.slice(0, 8)),
      el("span", { class: "text" },
        el("span", { class: "line" }, s.detail || s.title || ""),
        s.tools && s.tools.length ? el("span", { class: "tally" }, s.tools.map((t) => `${t.name} ×${t.count}`).join(" · ")) : null),
      el("span", { class: "time", title: `来源：${s.source || "—"}` }, ago(s.since, now)),
    ),
    ...(s.agents || []).map((a) => el("div", { class: `session sub ${a.state}` },
      el("span", { class: "branch", "aria-hidden": "true" }, "└"),
      el("span", {}, el("span", { class: `badge small ${a.state}` }, STATE_LABEL[a.state] || a.state)),
      el("span", { class: "agent-type" }, a.type || "agent"),
      el("span", { class: "text" }, [a.desc, a.detail].filter(Boolean).join(" · ")),
      el("span", { class: "time" }, ACTIVE.includes(a.state) ? fmtDuration((now - toDate(a.since)) / 1000) : ago(a.updated, now)),
    )),
  )));
}

const ACTIVE = ["approval", "input", "error", "running"];

// The most urgent of a session's own state and its agents' states.
function effectiveState(s) {
  let st = s.state;
  for (const a of s.agents || []) {
    if (ACTIVE.includes(a.state) && STATE_ORDER.indexOf(a.state) < STATE_ORDER.indexOf(st)) st = a.state;
  }
  return st;
}

function renderDevicePill(dev) {
  const pill = $("#device-pill");
  const map = {
    disabled: ["设备：已禁用", ""],
    searching: ["设备：未连接", "warn"],
    connected: ["设备：未握手", "warn"],
    ready: ["设备：已连接", "ok"],
  };
  const [text, cls] = map[dev && dev.state] || ["设备：—", ""];
  pill.textContent = text;
  pill.className = `pill ${cls}`;
  pill.title = (dev && (dev.error || dev.port)) || "";
}

// ---------- device preview ----------

function fitScreen() {
  const frame = $(".screen-frame");
  const screen = $("#screen");
  if (!frame || frame.offsetParent === null) return;
  const avail = frame.clientWidth - 40;
  const s = Math.min(1, avail / 600);
  screen.style.setProperty("--s", s);
  frame.style.height = `${450 * s + 40}px`;
}
window.addEventListener("resize", fitScreen);

// Icons drawn on the screen; static markup only.
const SCR_ICONS = {
  approval: '<path d="M12 3.5 2.8 19.5h18.4z"/><path d="M12 10v4"/><path d="M12 16.8h.01"/>',
  input: '<path d="M4 5h16v11H10l-6 4z"/><path d="M9 10.5h.01M12 10.5h.01M15 10.5h.01"/>',
  error: '<circle cx="12" cy="12" r="8.5"/><path d="m9 9 6 6M15 9l-6 6"/>',
  running: '<path d="M12 3.5a8.5 8.5 0 1 1-8.5 8.5"/>',
  done: '<path d="m5 12.5 4.5 4.5L19 7.5"/>',
  idle: '<circle cx="12" cy="12" r="2.5"/>',
  reset: '<path d="M3 12a9 9 0 1 0 3-6.7"/><path d="M3 4v5h5"/>',
  moon: '<path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"/>',
  unplug: '<path d="M9 7V3M15 7V3"/><path d="M6 7h12v4a6 6 0 0 1-6 6 6 6 0 0 1-6-6z"/><path d="M12 17v4"/><path d="M3 3l18 18"/>',
};

function scrIcon(name, size, width = 2.2, color = "currentColor") {
  const t = document.createElement("template");
  t.innerHTML = `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="${color}" stroke-width="${width}" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${SCR_ICONS[name]}</svg>`;
  return t.content.firstChild;
}

const SCR_ACTIVE = ["approval", "input", "error", "running"];
let hostConnected = false;
let previewRows = 4;
try { previewRows = localStorage.getItem("previewRows") === "3" ? 3 : 4; } catch {}

// "2:13" until a reset within a day, "3d" beyond.
function scrReset(resetsAt, now) {
  if (!resetsAt) return "—";
  const sec = Math.max(0, resetsAt - now);
  if (sec >= 86400) return `${Math.round(sec / 86400)}d`;
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60);
  return `${h}:${String(m).padStart(2, "0")}`;
}

// Running states count up as m:ss (or 1h05m); finished ones say how long ago.
function scrTime(state, since, now) {
  const sec = Math.max(0, now - since);
  if (SCR_ACTIVE.includes(state)) {
    if (sec >= 3600) return `${Math.floor(sec / 3600)}h${String(Math.floor((sec % 3600) / 60)).padStart(2, "0")}m`;
    return `${Math.floor(sec / 60)}:${String(sec % 60).padStart(2, "0")}`;
  }
  if (sec < 60) return "刚刚";
  if (sec < 3600) return `${Math.floor(sec / 60)}分钟前`;
  if (sec < 86400) return `${Math.floor(sec / 3600)}小时前`;
  return `${Math.floor(sec / 86400)}天前`;
}

function scrMeter(label, w, provider, now) {
  const pct = w ? Math.round(w.pct) : null;
  const color = pct >= 90 ? "#FF6B5E" : pct >= 70 ? "#FFB020" : `var(--scr-${provider})`;
  return el("div", { class: "scr-meter" },
    el("div", { class: "top" },
      el("span", { class: "lbl" }, label,
        el("span", { class: "reset mono" }, scrIcon("reset", 12, 2.5), scrReset(w && w.resets_at, now))),
      el("span", { class: "pct mono" }, pct === null ? "—" : `${pct}%`)),
    el("div", { class: "bar" },
      el("div", { class: "fill", style: `width:${pct === null ? 0 : Math.max(pct, 2)}%;background:${color}` })),
  );
}

function renderScreen(frame) {
  const screen = $("#screen");
  screen.classList.toggle("rows-3", previewRows === 3);
  for (const b of document.querySelectorAll(".seg [data-rows]")) {
    b.setAttribute("aria-pressed", String(Number(b.dataset.rows) === previewRows));
  }
  if (!frame) { screen.replaceChildren(); return; }
  const now = frame.now;
  const sessions = frame.sessions || [];
  const counts = frame.counts || {};

  const summary = SCR_ACTIVE.filter((k) => counts[k]).map((k) => `${STATE_LABEL[k]} ${counts[k]}`).join(" · ")
    || (sessions.length ? "全部完成" : "");
  const clock = new Date(now * 1000).toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });
  const strip = el("div", { class: "scr-strip" },
    el("div", { class: "left" }, el("span", { class: "title" }, "Agent"), el("span", { class: "summary" }, summary)),
    el("div", { class: "right" }, el("span", { class: `link ${hostConnected ? "" : "off"}` }), el("span", { class: "clock mono" }, clock)),
  );

  const usage = el("div", { class: "scr-usage" }, ...["claude", "codex"].map((p) => {
    const u = (frame.usage && frame.usage[p]) || {};
    return el("div", { class: "scr-urow" },
      el("span", { class: `name ${p}` }, PROVIDER_NAME[p]),
      scrMeter("5h", u.five_hour, p, now),
      scrMeter("7d", u.seven_day, p, now),
      el("div", { class: "scr-today" }, el("span", { class: "n mono" }, fmtTokens(u.today_tokens || 0)), el("span", { class: "l" }, "今日")),
    );
  }));

  const per = previewRows;
  const pages = Math.max(1, Math.ceil(sessions.length / per));
  previewPage = Math.min(previewPage, pages - 1);
  const shown = sessions.slice(previewPage * per, previewPage * per + per);
  const list = el("div", { class: "scr-list" }, shown.length
    ? shown.map((s) => el("div", { class: `scr-row ${s.state}` },
      el("span", { class: "chip" }, scrIcon(SCR_ICONS[s.state] ? s.state : "idle", 22, s.state === "running" || s.state === "done" ? 2.4 : 2.2)),
      el("div", { class: "mid" },
        el("div", { class: "l1" }, el("span", { class: "project" }, s.project || "—"), el("span", { class: `prov ${s.p}` }, PROVIDER_NAME[s.p] || s.p),
          s.agents_total ? el("span", { class: "subs" }, `子任务 ${s.agents_active || s.agents_total}`) : null),
        el("span", { class: "detail" }, s.text || "\u00a0")),
      el("div", { class: "end" },
        el("span", { class: "label" }, s.label),
        el("span", { class: "time mono" }, scrTime(s.state, s.since, now))),
    ))
    : el("div", { class: "scr-empty" }, scrIcon("moon", 44, 1.8, "#5C5B57"),
      el("span", { class: "t" }, "没有进行中的任务"), el("span", { class: "s" }, "Claude 和 Codex 都在待命")));
  const tasks = el("div", { class: "scr-tasks" }, list,
    pages > 1 ? el("button", {
      class: "scr-pager", type: "button", "aria-label": `下一页任务（第 ${previewPage + 1} 页，共 ${pages} 页）`,
      onclick: () => { previewPage = (previewPage + 1) % pages; renderScreen(frame); },
    }, ...Array.from({ length: pages }, (_, i) => el("i", { class: i === previewPage ? "on" : "" }))) : null,
  );

  const body = el("div", { class: `scr-body ${hostConnected ? "" : "dim"}` }, strip, usage, tasks);
  const parts = [body];
  if (!hostConnected) {
    parts.push(el("div", { class: "scr-offline" }, el("div", { class: "card" },
      scrIcon("unplug", 44, 1.9, "#FF6B5E"),
      el("span", { class: "t" }, "主机离线"),
      el("span", { class: "s" }, "网页与 go-dock 的连接已断开"))));
  }
  screen.replaceChildren(...parts);
  fitScreen();
}

for (const b of document.querySelectorAll(".seg [data-rows]")) {
  b.addEventListener("click", () => {
    previewRows = Number(b.dataset.rows);
    previewPage = 0;
    try { localStorage.setItem("previewRows", String(previewRows)); } catch {}
    if (state) renderScreen(state.frame);
  });
}

function renderDeviceDetail(dev) {
  const dl = $("#device-detail");
  const rows = [
    ["状态", { disabled: "已禁用", searching: "查找中", connected: "已打开串口，等待设备握手", ready: "已连接" }[dev.state] || dev.state],
    ["串口", dev.port || "—"],
    ["固件", dev.firmware || "—"],
    ["分辨率", dev.width ? `${dev.width}×${dev.height}` : "—"],
    ["已发送", `${dev.sent || 0} 帧`],
    ["最后发送", ago(dev.last_sent)],
    ["最后收到", ago(dev.last_heard)],
  ];
  if (dev.error) rows.push(["错误", dev.error]);
  dl.replaceChildren(...rows.flatMap(([k, v]) => [el("dt", {}, k), el("dd", {}, v)]));
}

$("#reconnect").addEventListener("click", async () => {
  try { renderDeviceDetail(await api("/api/device/reconnect", { method: "POST", body: {} })); } catch (e) { alert(e.message); }
});

// ---------- state stream ----------

function render() {
  if (!state) return;
  $("#version").textContent = state.version;
  renderBanner(state.update);
  renderUsage(state.snapshot);
  renderSessions(state.snapshot);
  renderDevicePill(state.device);
  renderDeviceDetail(state.device || {});
  renderScreen(state.frame);
  $("#frame-json").textContent = JSON.stringify(state.frame, null, 2);
}

$("#show-idle").addEventListener("change", (e) => {
  try { localStorage.setItem("showIdle", e.target.checked ? "1" : ""); } catch {}
  render();
});

function connect() {
  const pill = $("#conn-pill");
  const es = new EventSource("/api/events");
  es.addEventListener("open", () => { pill.textContent = "已连接"; pill.className = "pill ok"; hostConnected = true; });
  es.addEventListener("error", () => {
    pill.textContent = "已断开，重连中…"; pill.className = "pill bad";
    hostConnected = false;
    if (state) renderScreen(state.frame);
  });
  es.addEventListener("state", (e) => {
    const next = JSON.parse(e.data);
    if (state && state.version !== next.version) upgradedTo = next.version;
    state = next;
    render();
  });
  es.addEventListener("log", (e) => appendLog(JSON.parse(e.data)));
}

// ---------- updates & system ----------

let upgradedTo = null;
let applying = false;

function dismissed(v) { try { return localStorage.getItem("dismissUpdate") === v; } catch { return false; } }

function renderBanner(u) {
  const box = $("#update-banner");
  if (!u) { box.hidden = true; return; }
  let parts = null;
  let cls = "banner";
  if (upgradedTo) {
    parts = [el("span", { class: "msg" }, `已升级到 ${upgradedTo}`),
      el("button", { class: "btn ghost", onclick: () => { upgradedTo = null; render(); } }, "知道了")];
  } else if (u.state === "restarting") {
    parts = [el("span", { class: "msg" }, "新版本已安装，正在重启…")];
  } else if (u.state === "downloading") {
    parts = [el("span", { class: "msg" }, `正在下载并校验 ${u.latest}…`)];
  } else if (applying && u.state === "error") {
    cls += " err";
    parts = [el("span", { class: "msg" }, `升级失败：${u.error}`),
      el("button", { class: "btn ghost", onclick: () => { applying = false; render(); } }, "关闭")];
  } else if (u.available && !dismissed(u.latest)) {
    parts = [
      el("span", { class: "msg" }, `发现新版本 ${u.latest}（当前 ${u.current}）`,
        u.dev ? "。开发版本不能在线升级，请到发布页下载。" : ""),
      u.url ? el("a", { href: u.url, target: "_blank", rel: "noopener" }, u.dev ? "发布页" : "更新说明") : null,
      u.dev ? null : el("button", { class: "btn primary", onclick: applyUpdate }, "升级并重启"),
      el("button", { class: "btn ghost", onclick: () => {
        try { localStorage.setItem("dismissUpdate", u.latest); } catch {}
        render();
      } }, "稍后"),
    ];
  }
  box.hidden = !parts;
  if (parts) { box.className = cls; box.replaceChildren(...parts); }
}

async function applyUpdate() {
  if (!confirm("下载新版本并重启 go-dock？升级期间屏幕和网页会断开几秒。")) return;
  applying = true;
  try {
    const u = await api("/api/update/apply", { method: "POST", body: {} });
    if (state) { state.update = u; render(); }
  } catch (e) {
    applying = false;
    alert(e.message);
  }
}

function renderSystem(info) {
  const u = info.update;
  $("#sys-version").textContent = info.version + (u.dev ? "（开发版本）" : "");
  const desc = $("#sys-update");
  desc.className = "sys-desc";
  if (u.state === "checking") desc.textContent = "正在检查…";
  else if (u.state === "error" && u.error) { desc.textContent = u.error; desc.className = "sys-desc err"; }
  else if (!u.checked_at) desc.textContent = "尚未检查更新";
  else if (!u.latest) desc.textContent = `还没有发布版本（检查于 ${ago(u.checked_at)}）`;
  else if (u.available) desc.textContent = `有新版本 ${u.latest}` + (u.dev ? "，开发版本请到发布页下载" : "");
  else desc.textContent = `已是最新版本（检查于 ${ago(u.checked_at)}）`;
  const apply = $("#apply-update");
  apply.hidden = !(u.available && !u.dev);
  apply.textContent = `升级到 ${u.latest}`;

  const a = info.autostart;
  const toggle = $("#autostart-toggle");
  const ad = $("#sys-autostart");
  toggle.checked = !!a.enabled;
  toggle.disabled = !a.supported || (!!a.error && !a.enabled);
  ad.className = "sys-desc";
  const how = { launchd: "macOS 登录项（LaunchAgent）", systemd: "systemd 用户服务", xdg: "桌面环境自启动项", registry: "Windows 注册表启动项" }[a.method] || "";
  if (!a.supported) ad.textContent = "当前系统不支持";
  else if (a.error) { ad.textContent = a.error; ad.className = "sys-desc err"; }
  else if (a.enabled && a.stale) { ad.textContent = "自启项指向的不是当前程序，请关闭后重新开启"; ad.className = "sys-desc warn"; }
  else if (a.enabled) ad.textContent = `已开启，登录后自动在后台运行（${how}：${a.path}）`;
  else ad.textContent = `登录系统后自动在后台运行 go-dock（${how}）`;
}

async function loadSystem() {
  try { renderSystem(await api("/api/system")); } catch (e) { $("#sys-update").textContent = e.message; }
}

$("#check-update").addEventListener("click", async (ev) => {
  ev.target.disabled = true;
  $("#sys-update").textContent = "正在检查…";
  try { renderSystem(await api("/api/update/check", { method: "POST", body: {} })); }
  catch (e) { $("#sys-update").textContent = e.message; $("#sys-update").className = "sys-desc err"; }
  ev.target.disabled = false;
});
$("#apply-update").addEventListener("click", applyUpdate);
$("#autostart-toggle").addEventListener("change", async (ev) => {
  const want = ev.target.checked;
  ev.target.disabled = true;
  try { renderSystem(await api("/api/autostart", { method: "POST", body: { enabled: want } })); }
  catch (e) { alert(e.message); ev.target.checked = !want; ev.target.disabled = false; }
});

// ---------- integrations ----------

const INTEGRATION_DESC = {
  "claude-hooks": "实时上报会话开始、提问、工具调用、授权请求和完成事件。「需授权」会立刻显示，并带出工具和命令。",
  "claude-statusline": "从 Claude Code 状态栏数据中读取官方提供的 5 小时 / 7 天额度。会保留并继续调用你现有的状态栏命令（如 claude-hud）。",
  "codex-hooks": "实时上报 Codex 的提问、工具调用、授权请求和完成事件。安装后需在 Codex 中执行 /hooks 信任。",
};

async function loadIntegrations() {
  const box = $("#integrations");
  try {
    renderIntegrations(await api("/api/integrations"));
  } catch (e) {
    box.replaceChildren(el("div", { class: "panel note err" }, e.message));
  }
}

function renderIntegrations(list) {
  const box = $("#integrations");
  box.replaceChildren(...list.map((it) => {
    const [text, cls] = it.error ? ["异常", "bad"] : it.stale ? ["需更新", "warn"] : it.installed ? ["已安装", "ok"] : ["未安装", ""];
    const act = (action) => async (ev) => {
      ev.target.disabled = true;
      try { renderIntegrations(await api(`/api/integrations/${it.name}/${action}`, { method: "POST", body: {} })); }
      catch (e) { alert(e.message); ev.target.disabled = false; }
    };
    return el("div", { class: "panel card" },
      el("h3", {}, it.title, el("span", { class: `pill ${cls}` }, text)),
      el("div", { class: "file" }, it.file),
      el("p", { class: "desc" }, INTEGRATION_DESC[it.name] || ""),
      it.note ? el("div", { class: "note" }, it.note) : null,
      it.error ? el("div", { class: "note err" }, it.error) : null,
      el("div", { class: "row" },
        el("button", { class: "btn primary", onclick: act("install") }, it.installed ? "重新安装" : "安装"),
        it.installed ? el("button", { class: "btn danger", onclick: act("uninstall") }, "卸载") : null,
      ),
    );
  }));
}

// ---------- settings ----------

const form = $("#settings");
let loadedConfig = null;

function getPath(obj, path) { return path.split(".").reduce((o, k) => (o ? o[k] : undefined), obj); }
function setPath(obj, path, v) {
  const keys = path.split(".");
  let o = obj;
  for (const k of keys.slice(0, -1)) o = o[k] = o[k] || {};
  o[keys.at(-1)] = v;
}

async function loadSettings() {
  try {
    loadedConfig = await api("/api/config");
    for (const input of form.elements) {
      if (!input.name) continue;
      const v = getPath(loadedConfig, input.name);
      if (input.type === "checkbox") input.checked = !!v;
      else input.value = v ?? "";
    }
    $("#settings-msg").textContent = "";
  } catch (e) {
    showFormMsg(e.message, true);
  }
}

function showFormMsg(text, err) {
  const m = $("#settings-msg");
  m.textContent = text;
  m.className = `form-msg ${err ? "err" : ""}`;
}

form.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const cfg = structuredClone(loadedConfig || {});
  for (const input of form.elements) {
    if (!input.name) continue;
    let v = input.type === "checkbox" ? input.checked : input.value.trim();
    if (input.type === "number") v = Number(v);
    setPath(cfg, input.name, v);
  }
  try {
    loadedConfig = await api("/api/config", { method: "PUT", body: cfg });
    showFormMsg("已保存");
  } catch (e) {
    showFormMsg(`保存失败：${e.message}`, true);
  }
});

$("#scan-ports").addEventListener("click", async () => {
  const box = $("#ports");
  box.replaceChildren(el("span", { class: "muted" }, "扫描中…"));
  try {
    const ports = (await api("/api/device/ports")) || [];
    $("#port-list").replaceChildren(el("option", { value: "auto" }), ...ports.map((p) => el("option", { value: p.name })));
    box.replaceChildren(...(ports.length ? ports.map((p) => el("button", {
      type: "button", class: "btn ghost",
      onclick: () => { form.elements["device.port"].value = p.name; },
    }, `${p.candidate ? "★ " : ""}${p.name}${p.vid ? `  ${p.vid}:${p.pid}` : ""}${p.product ? `  ${p.product}` : ""}`))
      : [el("span", { class: "muted" }, "没有找到串口")]));
  } catch (e) {
    box.replaceChildren(el("span", { class: "form-msg err" }, e.message));
  }
});

// ---------- logs ----------

let logLines = [];

function logRow(e) {
  const t = new Date(e.time);
  return el("div", { class: "log" },
    el("span", { class: "t" }, t.toLocaleTimeString("zh-CN", { hour12: false })),
    el("span", { class: `lv ${e.level}` }, e.level),
    el("span", { class: "m" }, e.msg, e.attrs ? el("span", { class: "a" }, " " + e.attrs) : null),
  );
}

const levelRank = { DEBUG: 0, INFO: 1, WARN: 2, ERROR: 3 };
const passes = (e) => (levelRank[e.level] ?? 1) >= levelRank[$("#log-level").value];

function renderLogs() {
  const box = $("#logs");
  box.replaceChildren(...logLines.filter(passes).map(logRow));
  if ($("#log-follow").checked) box.scrollTop = box.scrollHeight;
}

async function loadLogs() {
  try {
    logLines = await api("/api/logs?level=DEBUG&limit=2000");
    renderLogs();
  } catch {}
}

function appendLog(e) {
  if (logLines.length && e.seq <= logLines.at(-1).seq) return;
  logLines.push(e);
  if (logLines.length > 2000) logLines.shift();
  if ($("#tab-logs").hidden || !passes(e)) return;
  const box = $("#logs");
  box.append(logRow(e));
  if ($("#log-follow").checked) box.scrollTop = box.scrollHeight;
}

$("#log-level").addEventListener("change", renderLogs);
$("#log-clear").addEventListener("click", () => { logLines = []; renderLogs(); });

// ---------- boot ----------

try { $("#show-idle").checked = localStorage.getItem("showIdle") === "1"; } catch {}
let initialTab = location.hash.replace(/^#\/?/, "");
if (!initialTab) { try { initialTab = localStorage.getItem("tab") || "overview"; } catch { initialTab = "overview"; } }
showTab(document.querySelector(`[data-tab="${initialTab}"]`) ? initialTab : "overview");
connect();
setInterval(render, 15000); // keep relative times fresh
