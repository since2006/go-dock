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
let previewPage = 0; // task page on the Agent page
let scrTaskPages = 1;

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

// Cache reads are left out, as on the screen (model.Tokens.Fresh).
const freshTokens = (t) => (t ? t.input + t.output + t.cache_create : 0);
const tokensTitle = (t) => (t ? `输入 ${fmtTokens(t.input)} · 缓存写入 ${fmtTokens(t.cache_create)} · 输出 ${fmtTokens(t.output)}\n不含缓存读取 ${fmtTokens(t.cache_read)}` : "");
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
  if (name === "plugins") loadPlugins();
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
        el("div", { title: tokensTitle(today) }, el("b", {}, fmtTokens(freshTokens(today))), el("span", {}, "今日 token")),
        today && today.has_cost ? el("div", {}, el("b", {}, `$${today.cost_usd.toFixed(2)}`), el("span", {}, "今日 API 等价费用")) : null,
        el("div", { title: tokensTitle(u.week) }, el("b", {}, fmtTokens(freshTokens(u.week))), el("span", {}, "近 7 天 token")),
      ),
      el("p", { class: "tokens-note" }, "token 数不含缓存读取，鼠标移到数字上可看明细"),
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
  list.replaceChildren(...rows.map((s) => { const st = effectiveState(s); return el("div", { class: "group" },
    el("div", { class: `session ${st}`, title: s.cwd || "" },
      el("span", {}, el("span", { class: `badge ${st}` }, STATE_LABEL[st] || st)),
      el("span", { class: `prov ${s.provider}` }, PROVIDER_NAME[s.provider] || s.provider),
      el("span", { class: "project" }, el("span", { class: "name" }, s.project || s.session_id.slice(0, 8)),
        s.title ? el("span", { class: "sname", title: s.title }, s.title) : null),
      el("span", { class: "text" },
        ...sessionLines(s).map((t, i, all) => el("span", { class: all.length > 1 && i === 0 ? "line msg" : "line", title: t }, t)),
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
  ); }));
}

// The assistant's latest text above what the session is doing now; the
// session name sits under the project.
function sessionLines(s) {
  const lines = [s.message, waitingText(s) || s.detail].filter(Boolean);
  return lines.length ? lines : [""];
}

// A session whose own turn ended while background subagents still work
// is waiting for them, not done.
function waitingText(s) {
  if (s.state !== "done" && s.state !== "idle") return "";
  const n = (s.agents || []).filter((a) => BUSY.includes(a.state)).length;
  return n ? `等待 ${n} 个子任务完成` : "";
}

const ACTIVE = ["approval", "input", "error", "running"];
const BUSY = ["approval", "input", "running"]; // a failed subagent has stopped

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
    unset: ["设备：未设置", "warn"],
    searching: ["设备：未连接", "warn"],
    connected: ["设备：未握手", "warn"],
    ready: ["设备：已连接", "ok"],
  };
  const [text, cls] = map[dev && dev.state] || ["设备：—", ""];
  pill.textContent = text;
  pill.className = `pill ${cls}`;
  pill.title = (dev && (dev.error || dev.addr)) || "";
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
  moon: '<path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"/>', // the empty task list
  // Reading icons of the line (item.icon), lines and arcs only, as the firmware draws them.
  cpu: '<path d="M7 7h10v10H7z"/><path d="M10 10h4v4h-4z"/><path d="M10 3v4M14 3v4M10 17v4M14 17v4M3 10h4M3 14h4M17 10h4M17 14h4"/>',
  gpu: '<path d="M2.5 6h19v11h-19z"/><circle cx="9" cy="11.5" r="3"/><path d="M15 9.5h3.5M15 13.5h3.5"/><path d="M5.5 17v3M9.5 17v3"/>',
  memory: '<path d="M3 7h18v9H3z"/><path d="M6.5 10h3v3h-3zM14.5 10h3v3h-3z"/><path d="M6 16v3M10 16v3M14 16v3M18 16v3"/>',
  rotor: '<circle cx="12" cy="12" r="10"/><circle cx="12" cy="12" r="1.6"/><path d="M12 10.4A4 4 0 0 1 12 4A4 4 0 0 1 12 10.4"/><path d="M13.39 12.8A4 4 0 0 1 18.93 16A4 4 0 0 1 13.39 12.8"/><path d="M10.61 12.8A4 4 0 0 1 5.07 16A4 4 0 0 1 10.61 12.8"/>',
  temp: '<path d="M12.5 13.76V5a2.5 2.5 0 0 0-5 0v8.76a4.5 4.5 0 1 0 5 0z"/><path d="M15.5 6h4M15.5 10.5h4"/>',
  disk: '<path d="M4 4.5h16v15H4z"/><path d="M4 14h16"/><circle cx="16.5" cy="16.75" r="1"/>',
  unplug: '<path d="M9 7V3M15 7V3"/><path d="M6 7h12v4a6 6 0 0 1-6 6 6 6 0 0 1-6-6z"/><path d="M12 17v4"/><path d="M3 3l18 18"/>',
};

function scrIcon(name, size, width = 2.2, color = "currentColor") {
  const t = document.createElement("template");
  t.innerHTML = `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="${color}" stroke-width="${width}" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${SCR_ICONS[name]}</svg>`;
  return t.content.firstChild;
}

const SCR_ACTIVE = ["approval", "input", "error", "running"];
let hostConnected = false;

// Time left in a usage window, as claude-hud prints it: "33m", "4h 33m", "4d 14h".
function scrReset(resetsAt, now) {
  if (!resetsAt) return "—";
  const mins = Math.ceil(Math.max(0, resetsAt - now) / 60);
  if (mins < 60) return `${mins}m`;
  const h = Math.floor(mins / 60), m = mins % 60;
  if (h >= 24) return h % 24 ? `${Math.floor(h / 24)}d ${h % 24}h` : `${Math.floor(h / 24)}d`;
  return m ? `${h}h ${m}m` : `${h}h`;
}

// Running states count up as m:ss (or 1h05m); finished ones say how long ago.
function scrTime(state, since, now) {
  const sec = Math.max(0, now - since);
  if (SCR_ACTIVE.includes(state)) return scrClock(sec);
  if (sec < 60) return "刚刚";
  if (sec < 3600) return `${Math.floor(sec / 60)}分钟前`;
  if (sec < 86400) return `${Math.floor(sec / 3600)}小时前`;
  return `${Math.floor(sec / 86400)}天前`;
}

function scrClock(sec) {
  if (sec >= 3600) return `${Math.floor(sec / 3600)}h${String(Math.floor((sec % 3600) / 60)).padStart(2, "0")}m`;
  return `${Math.floor(sec / 60)}:${String(sec % 60).padStart(2, "0")}`;
}

// A session row's time: while the turn runs or waits for the user, its work
// time as Claude Code's spinner counts it (held during the wait; a screen
// keeps counting from turn_secs while running); otherwise scrTime.
function scrRowTime(s, now) {
  const turn = ["running", "approval", "input"].includes(s.state) && s.turn_secs;
  return turn ? scrClock(s.turn_secs) : scrTime(s.state, s.since, now);
}

function scrMeter(label, w, provider, now) {
  const pct = w ? Math.round(w.pct) : null;
  const color = pct >= 90 ? "#FF6B5E" : pct >= 70 ? "#FFB020" : `var(--scr-${provider})`;
  return el("div", { class: "scr-meter" },
    el("div", { class: "top" },
      el("span", { class: "reset" }, el("b", { class: "mono" }, scrReset(w && w.resets_at, now)), ` / ${label}`),
      el("span", { class: "pct mono" }, pct === null ? "—" : `${pct}%`)),
    el("div", { class: "bar" },
      el("div", { class: "fill", style: `width:${pct === null ? 0 : Math.max(pct, 2)}%;background:${color}` })),
  );
}

// Task list layout, shared with the firmware (docs/protocol-v1.md §7.1): a
// session row (126 px, two to a page) has the project and state, then the
// session name and turn time, then one line of text (scrText); each
// subagent adds a 36 px row under it; groups never split across pages.
const SCR_LIST_H = 258, SCR_MAIN = 126, SCR_SUB = 36, SCR_SUB_GAP = 4, SCR_GROUP_GAP = 6;

// The line under the project line: what the session is doing, else the
// assistant's latest text. None leaves the project line alone, centred in
// the row.
function scrText(s) {
  return s.detail || s.msg || "";
}

function scrGroupHeight(s) {
  return SCR_MAIN + (s.agents || []).length * (SCR_SUB + SCR_SUB_GAP);
}

function scrPages(sessions) {
  const pages = [[]];
  let used = 0;
  for (const s of sessions) {
    const h = scrGroupHeight(s);
    let page = pages[pages.length - 1];
    if (page.length && used + SCR_GROUP_GAP + h > SCR_LIST_H) {
      page = [];
      pages.push(page);
      used = 0;
    }
    used += (page.length ? SCR_GROUP_GAP : 0) + h;
    page.push(s);
  }
  return pages;
}

function scrGroup(s, now) {
  const text = scrText(s);
  const row = el("div", { class: `scr-row ${s.state}` },
    el("span", { class: "chip" }, scrIcon(SCR_ICONS[s.state] ? s.state : "idle", 24, s.state === "running" || s.state === "done" ? 2.4 : 2.2)),
    el("div", { class: "mid" },
      // Project and provider, the state at the right end; then the session
      // name, this turn's time and output tokens ("1:55 ↓6.5K", like Claude
      // Code's spinner) at the right end.
      el("div", { class: "l1" }, el("span", { class: "project" }, s.project || "—"),
        el("span", { class: `prov ${s.p}` }, PROVIDER_NAME[s.p] || s.p),
        el("span", { class: "end" }, el("span", { class: "label" }, STATE_LABEL[s.state] || s.state))),
      el("div", { class: "l2" }, el("span", { class: "sname" }, s.title || ""),
        el("span", { class: "end" },
          el("span", { class: "time mono" }, scrRowTime(s, now),
            s.turn_output ? el("span", { class: "tok" }, ` ↓${fmtTokens(s.turn_output)}`) : null))),
      text ? el("span", { class: "detail" }, text) : null),
  );
  const subs = (s.agents || []).map((a) => el("div", { class: `scr-sub ${a.state}` },
    el("span", { class: "dot" }, scrIcon(SCR_ICONS[a.state] ? a.state : "idle", 16, 2.6)),
    el("span", { class: "type" }, a.type),
    el("span", { class: "text" }, a.text || ""),
    el("span", { class: "end" },
      a.state !== "running" ? el("span", { class: "label" }, STATE_LABEL[a.state] || a.state) : null,
      el("span", { class: "time mono" }, scrTime(a.state, a.since, now))),
  ));
  return el("div", { class: "scr-group" }, row, ...subs);
}

// The strip on top: title and summary on the left; link and clock on the
// right.
function scrStrip(title, summary, now) {
  const clock = new Date(now * 1000).toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });
  return el("div", { class: "scr-strip" },
    el("div", { class: "left" }, el("span", { class: "title" }, title), el("span", { class: "summary" }, summary)),
    el("div", { class: "right" }, el("span", { class: `link ${hostConnected ? "" : "off"}` }), el("span", { class: "clock mono" }, clock)),
  );
}

// The Agent page's title: the name of the computer it shows, as go-dock
// tells the screen in its hello.
function scrHostName() {
  return (state && state.device && state.device.name) || "Agent";
}

function scrAgentBody(frame) {
  const now = frame.now;
  const sessions = frame.sessions || [];
  const counts = frame.counts || {};
  const summary = SCR_ACTIVE.filter((k) => counts[k]).map((k) => `${STATE_LABEL[k]} ${counts[k]}`).join(" · ")
    || (sessions.length ? "全部完成" : "");
  const strip = scrStrip(scrHostName(), summary, now);

  const line = lineItems();
  const usage = el("div", { class: "scr-usage" }, ...["claude", "codex"].map((p) => {
    const u = (frame.usage && frame.usage[p]) || {};
    return el("div", { class: "scr-urow" },
      el("span", { class: `name ${p}` }, PROVIDER_NAME[p]),
      scrMeter("5h", u.five_hour, p, now),
      scrMeter("7d", u.seven_day, p, now),
      el("div", { class: "scr-today" }, el("span", { class: "n mono" }, fmtTokens(u.today_tokens || 0)), el("span", { class: "l" }, "今日")),
    );
  }));

  const pages = scrPages(sessions);
  scrTaskPages = pages.length;
  previewPage = Math.min(previewPage, pages.length - 1);
  const shown = pages[previewPage];
  const list = el("div", { class: "scr-list" }, shown.length
    ? shown.map((s) => scrGroup(s, now))
    : el("div", { class: "scr-empty" }, scrIcon("moon", 44, 1.8, "#5C5B57"),
      el("span", { class: "t" }, "没有进行中的任务"), el("span", { class: "s" }, "Claude 和 Codex 都在待命")));
  const tasks = el("div", { class: "scr-tasks" }, list,
    pages.length > 1 ? el("button", {
      class: "scr-pager", type: "button", "aria-label": `下一页任务（第 ${previewPage + 1} 页，共 ${pages.length} 页）`,
      onclick: () => goTaskPage(1),
    }, ...pages.map((_, i) => el("i", { class: i === previewPage ? "on" : "" }))) : null,
  );
  return [strip, line.length ? scrLine(line) : null, usage, tasks];
}

// The line above the usage, shared with the firmware (docs/protocol-v1.md
// §7.1): the readings three at a time, each its icon, label and reading,
// then a bar (or a note, like the upload), in its colour, amber or red when
// its level says so. Up to three share the line evenly. More take turns in
// groups of three on fixed columns: a group stays SCR_LINE_DWELL ms, then
// the next slides in from the right over SCR_LINE_SLIDE ms, and after the
// last the first comes round again. The turning follows the clock rather
// than the redraws, so new readings never jolt it.
const SCR_LINE_N = 3, SCR_LINE_DWELL = 6000, SCR_LINE_SLIDE = 500, SCR_LINE_GAP = 12;
let scrLineIds = "", scrLineStart = 0, scrLineTicking = false;

function lineItems() { return (state && state.line && state.line.items) || []; }

// How far the line has turned at t, in groups: whole while a group stays,
// eased in between while the next slides in.
function scrLineOffset(groups, t) {
  const period = SCR_LINE_DWELL + SCR_LINE_SLIDE;
  const e = Math.max(0, t - scrLineStart);
  const k = Math.floor(e / period) % groups, r = e % period;
  if (r < SCR_LINE_DWELL) return k;
  const x = (r - SCR_LINE_DWELL) / SCR_LINE_SLIDE;
  return k + (x < 0.5 ? 4 * x * x * x : 1 - (-2 * x + 2) ** 3 / 2);
}

function scrLineShift(groups, t) {
  const o = scrLineOffset(groups, t);
  return `translateX(calc(${-o * 100}% - ${o * SCR_LINE_GAP}px))`;
}

function scrLine(items) {
  const ids = items.map((it) => it.id).join(",");
  if (ids !== scrLineIds) { scrLineIds = ids; scrLineStart = performance.now(); } // other readings: start over
  const groups = Math.ceil(items.length / SCR_LINE_N);
  const cols = groups > 1 ? SCR_LINE_N : items.length;
  const group = (k) => el("div", { class: "scr-lgroup", style: `grid-template-columns:repeat(${cols},minmax(0,1fr))` },
    ...items.slice(k * SCR_LINE_N, (k + 1) * SCR_LINE_N).map(scrReading));
  if (groups < 2) return el("div", { class: "scr-line" }, group(0));
  // The first group again after the last, for it to slide in from the right.
  const track = el("div", { class: "track", "data-groups": groups, style: `transform:${scrLineShift(groups, performance.now())}` },
    ...Array.from({ length: groups }, (_, k) => group(k)), group(0));
  if (!scrLineTicking) { scrLineTicking = true; requestAnimationFrame(scrLineTick); }
  return el("div", { class: "scr-line" }, track);
}

// Moves the line on screen each frame while it turns; stops when no line
// has more than one group.
function scrLineTick() {
  const track = $("#screen .scr-line .track");
  if (!track) { scrLineTicking = false; return; }
  const shift = scrLineShift(Number(track.dataset.groups), performance.now());
  if (track.style.transform !== shift) track.style.transform = shift;
  requestAnimationFrame(scrLineTick);
}

function scrReading(it) {
  const icon = SCR_ICONS[it.icon] ? it.icon : null;
  const pct = it.pct == null ? null : Math.max(0, Math.min(100, it.pct));
  return el("div", { class: `seg icon-${icon || "none"} ${it.level || ""}` },
    icon && scrIcon(icon, 20, 2),
    el("span", { class: "l" }, it.label),
    el("span", { class: "v mono" }, it.text, it.unit ? el("span", { class: "u" }, it.unit) : null),
    pct === null
      ? el("span", { class: "sub" }, it.sub || "")
      : el("span", { class: "bar" }, el("span", { class: "fill", style: `width:${Math.max(pct, 2)}%` })));
}

function goTaskPage(delta) {
  if (scrTaskPages < 2) return;
  previewPage = (previewPage + delta + scrTaskPages) % scrTaskPages;
  if (state) renderScreen(state.frame);
}

function renderScreen(frame) {
  const screen = $("#screen");
  if (!frame) { screen.replaceChildren(); return; }
  const lined = lineItems().length > 0;
  const body = el("div", { class: `scr-body ${hostConnected ? "" : "dim"} ${lined ? "lined" : ""}` }, ...scrAgentBody(frame));
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

// Swipes up and down on the preview turn task pages, as on the screen. A
// swipe never also taps the pager.
{
  const screen = $("#screen");
  let start = null, swallow = false;
  screen.addEventListener("pointerdown", (e) => { start = { x: e.clientX, y: e.clientY }; });
  screen.addEventListener("pointercancel", () => { start = null; });
  screen.addEventListener("pointerup", (e) => {
    if (!start) return;
    const s = Number(screen.style.getPropertyValue("--s")) || 1;
    const dx = (e.clientX - start.x) / s, dy = (e.clientY - start.y) / s;
    start = null;
    if (Math.max(Math.abs(dx), Math.abs(dy)) < 50) return; // a tap
    swallow = true;
    setTimeout(() => { swallow = false; });
    if (Math.abs(dy) > Math.abs(dx)) goTaskPage(dy < 0 ? 1 : -1);
  });
  screen.addEventListener("click", (e) => { if (swallow) { e.preventDefault(); e.stopPropagation(); } }, true);
}

function renderDeviceDetail(dev) {
  const dl = $("#device-detail");
  const fw = el("span", {}, dev.current || dev.firmware || "—");
  if (FLASH_BUSY.includes(dev.flash && dev.flash.state)) fw.append(" · 正在升级");
  else if (dev.upgradable) fw.append(" ", el("button", { class: "btn small", onclick: applyFirmware }, `升级到 ${dev.target}`), dev.confirm ? " · 经 WiFi，需在屏幕上确认" : "");
  const usb = dev.usb || {};
  const rows = [
    ["WiFi", { disabled: "已禁用", unset: "还没设置屏幕地址", searching: "正在连接", connected: "已连上，等待握手", ready: "已连接" }[dev.state] || dev.state],
    ["屏幕地址", dev.addr || "—"],
    ["USB 线", { disabled: "已禁用", searching: "未插", connected: "已插上，等待握手", ready: `已插上（${usb.port}）` }[usb.state] || usb.state || "—"],
    ["固件", fw],
    ["分辨率", dev.width ? `${dev.width}×${dev.height}` : "—"],
    ["已发送", `${dev.sent || 0} 帧`],
    ["最后发送", ago(dev.last_sent)],
    ["最后收到", ago(dev.last_heard)],
  ];
  if (dev.error) rows.push(["错误", dev.error]);
  if (usb.error) rows.push(["USB 错误", usb.error]);
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
  renderFirmwareBanner(state.device || {});
  renderUsage(state.snapshot);
  renderSessions(state.snapshot);
  renderDevicePill(state.device);
  renderDeviceDetail(state.device || {});
  renderWiFi(state.device || {});
  renderScreen(state.frame);
  $("#frame-json").textContent = JSON.stringify(state.frame, null, 2);
  $("#line-json").textContent = JSON.stringify(state.line, null, 2);
  renderPluginsLive();
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
      el("span", { class: "msg" }, `发现新${u.prerelease ? "测试版" : "版本"} ${u.latest}（当前 ${u.current}）`,
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

// ---------- device firmware ----------

const FLASH_BUSY = ["downloading", "confirming", "preparing", "sending", "restarting"];
let firmwareClosed = null; // the finished update whose banner was closed

function firmwareDismissed(key) { try { return localStorage.getItem("dismissFirmware") === key; } catch { return false; } }

function firmwareLater(key) {
  return el("button", { class: "btn ghost", onclick: () => {
    try { localStorage.setItem("dismissFirmware", key); } catch {}
    render();
  } }, "稍后");
}

// The firmware banner follows an update's progress, then offers the
// firmware that goes with this go-dock.
function renderFirmwareBanner(dev) {
  const box = $("#firmware-banner");
  const f = dev.flash || {};
  const finished = f.state === "done" || f.state === "error" ? `${f.state}:${f.version}:${f.error || ""}` : null;
  const msg = (...c) => el("span", { class: "msg" }, ...c);
  let parts = null;
  let cls = "banner";
  const wifi = f.via === "wifi";
  if (f.state === "downloading") {
    parts = [msg(`正在下载固件 ${dev.target || ""}…`)];
  } else if (f.state === "confirming") {
    parts = [msg(`请在屏幕上点「升级」，确认升级到 ${f.version}（60 秒内不确认就取消）`)];
  } else if (f.state === "preparing") {
    parts = [msg(`正在准备升级固件 ${f.version}，设备在擦除存储，约需 10 秒…`)];
  } else if (f.state === "sending") {
    const pct = f.total ? Math.floor((f.sent * 100) / f.total) : 0;
    parts = [msg(`正在升级固件 ${f.version}：${pct}%，${wifi ? "请勿给屏幕断电" : "请勿拔 USB 线"}`)];
  } else if (f.state === "restarting") {
    parts = [msg(wifi ? "固件已写入，屏幕正在重启并重新连接 WiFi…" : "固件已写入，设备正在重启…")];
  } else if (finished && firmwareClosed !== finished) {
    const close = el("button", { class: "btn ghost", onclick: () => { firmwareClosed = finished; render(); } }, "关闭");
    if (f.state === "done") {
      parts = [msg(`设备固件已升级到 ${f.version}`), close];
    } else {
      cls += " err";
      parts = [msg(`固件升级失败：${f.error}`), dev.upgradable ? el("button", { class: "btn primary", onclick: applyFirmware }, "重试") : null, close];
    }
  } else if (dev.upgradable && !firmwareDismissed(dev.target)) {
    parts = [
      msg(`设备固件可以升级到 ${dev.target}（当前 ${dev.current}）`),
      el("button", { class: "btn primary", onclick: applyFirmware }, "升级固件"),
      firmwareLater(dev.target),
    ];
  }
  box.hidden = !parts;
  if (parts) { box.className = cls; box.replaceChildren(...parts); }
}

async function applyFirmware() {
  const dev = (state && state.device) || {};
  const ask = dev.confirm
    ? `通过 WiFi 把屏幕固件升级到 ${dev.target}？屏幕上会先弹出确认，点「升级」后开始，大约需要 1–2 分钟，期间不要给屏幕断电。`
    : `把设备固件升级到 ${dev.target}？大约需要 1 分钟，屏幕会显示进度，期间不要拔 USB 线。`;
  if (!confirm(ask)) return;
  firmwareClosed = null;
  try {
    const d = await api("/api/device/firmware", { method: "POST", body: {} });
    if (state) { state.device = d; render(); }
  } catch (e) {
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
  else if (u.available) desc.textContent = `有新${u.prerelease ? "测试版" : "版本"} ${u.latest}` + (u.dev ? "，开发版本请到发布页下载" : "");
  else desc.textContent = `已是最新${u.beta ? "版本（含测试版）" : "版本"}（检查于 ${ago(u.checked_at)}）`;
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

// Inputs named with dotted config paths ("device.port") fill from and read
// into a config object; each form saves only its own fields, which the
// server merges into the rest.
function fillForm(f, cfg) {
  for (const input of f.elements) {
    if (!input.name) continue;
    const v = getPath(cfg, input.name);
    if (input.type === "checkbox") input.checked = !!v;
    else if (input.tagName === "SELECT" && v != null && ![...input.options].some((o) => o.value === String(v))) {
      input.append(el("option", { value: v }, String(v))); // a value written by hand stays as it is
      input.value = v;
    } else input.value = v ?? "";
  }
}

function readForm(f) {
  const out = {};
  for (const input of f.elements) {
    if (!input.name) continue;
    let v = input.type === "checkbox" ? input.checked : input.type === "password" ? input.value : input.value.trim();
    if (input.type === "number" || "num" in input.dataset) v = Number(v);
    setPath(out, input.name, v);
  }
  return out;
}

function showMsg(sel, text, err) {
  const m = $(sel);
  m.textContent = text;
  m.className = `form-msg ${err ? "err" : ""}`;
}

async function loadSettings() {
  try {
    await loadSounds(); // the select needs its options before it can show the value
    loadedConfig = await api("/api/config");
    fillForm(form, loadedConfig);
    showCustomSound();
    showMsg("#settings-msg", "");
  } catch (e) {
    showMsg("#settings-msg", e.message, true);
  }
}

form.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    loadedConfig = await api("/api/config", { method: "POST", body: readForm(form) });
    showMsg("#settings-msg", "已保存");
  } catch (e) {
    showMsg("#settings-msg", `保存失败：${e.message}`, true);
  }
});

// ---------- screen WiFi ----------

// The screen joins a WiFi network when told so over the USB cable; it then
// reports its address and pairing code (state.device.usb.net), which the
// settings below take.
const wifiForm = $("#wifi-form");
function renderWiFi(dev) {
  const u = dev.usb || {};
  const usb = u.state === "ready";
  const n = u.net;
  let text;
  if (!usb) {
    text = "用 USB 线连接屏幕后，可以在这里让屏幕连上 WiFi。";
  } else if (!n || n.state === "off") {
    text = "屏幕还没有连 WiFi。填好下面的信息发给屏幕。";
  } else if (n.state === "connecting") {
    text = `屏幕正在连接 WiFi「${n.ssid}」…`;
  } else if (n.state === "failed") {
    text = `屏幕连不上 WiFi「${n.ssid}」：${n.err || "原因不明"}`;
  } else {
    text = `屏幕已连上 WiFi「${n.ssid}」，地址 ${wifiAddr(n)}${n.fixed ? "（固定 IP）" : ""}，配对码 ${n.code}。`;
  }
  $("#wifi-status").textContent = text;
  // Offered while the settings name another screen, or none.
  $("#wifi-use").hidden = !(usb && n && n.state === "connected" && n.ip && !screenInSettings(n));
  $("#wifi-fields").disabled = !usb;
  drawJoin();
}

function wifiAddr(n) {
  return n.port && n.port !== 47322 ? `${n.ip}:${n.port}` : n.ip;
}

function showWiFiStatic() {
  $("#wifi-static").hidden = !wifiForm.elements.fixed.checked;
}
wifiForm.elements.fixed.addEventListener("change", showWiFiStatic);

// Turning the WiFi off needs no dialog: the status line above shows it.
async function wifiOff() {
  try {
    const { device } = await api("/api/device/wifi", { method: "POST", body: { ssid: "" } });
    wifiJoin = null;
    joinText = "";
    showMsg("#wifi-msg", "屏幕已关闭 WiFi");
    if (state) { state.device = device; render(); }
  } catch (e) {
    showMsg("#wifi-msg", e.message, true);
  }
}

wifiForm.addEventListener("submit", (ev) => {
  ev.preventDefault();
  const f = wifiForm.elements;
  const ssid = f.ssid.value.trim();
  if (!ssid) {
    showMsg("#wifi-msg", "请填写 WiFi 名称，或点「扫描并选择」", true);
    return;
  }
  joinWiFi({ ssid, open: !f.pass.value }, f.pass.value);
});

$("#wifi-off").addEventListener("click", () => {
  if (confirm("让屏幕断开 WiFi 并忘掉 WiFi 设置？之后只能用 USB 连接。")) wifiOff();
});

// The settings name this screen: go-dock reaches it over WiFi.
function screenInSettings(n) {
  const f = form.elements;
  return f["device.addr"].value.trim() === wifiAddr(n) && f["device.code"].value.trim() === n.code;
}

async function useScreen(n) {
  loadedConfig = await api("/api/config", { method: "POST", body: { device: { addr: wifiAddr(n), code: n.code } } });
  fillForm(form, loadedConfig);
  render();
}

$("#wifi-switch").addEventListener("click", async () => {
  const n = state && state.device && state.device.usb && state.device.usb.net;
  if (!n) return;
  try {
    await useScreen(n);
    showMsg("#wifi-msg", "已保存，go-dock 正通过 WiFi 连接屏幕；之后可以拔掉 USB 线（屏幕要另外供电）");
  } catch (e) {
    showMsg("#wifi-msg", e.message, true);
  }
});

// ---------- WiFi dialog ----------

// The dialog walks through picking a network the screen sees (list),
// its password (pass) and the joining (join). The screen scans with its
// own radio, so the list holds what it can join: 2.4 GHz networks in its
// range.
const wifiDlg = $("#wifi-dialog");
let dlg = { view: "list" };
let scanSeq = 0;
// wifiJoin is the last settings sent: the network (ap), phase (sending,
// refused or sent), the try number whose outcome usb.net reports, and
// when. joinText is what the form last said about it.
let wifiJoin = null;
let joinText = "";
const SLOW_JOIN_MS = 30000;

wifiDlg.addEventListener("click", (ev) => { if (ev.target === wifiDlg) wifiDlg.close(); });
wifiDlg.addEventListener("close", () => { dlg = { view: "list" }; scanSeq++; });
$("#wifi-dlg-close").addEventListener("click", () => wifiDlg.close());
$("#wifi-scan").addEventListener("click", () => { openDialog(); scan(); });

function openDialog() {
  if (!wifiDlg.open) wifiDlg.showModal();
}

function show(view, extra = {}) {
  dlg = { ...dlg, ...extra, view };
  drawDialog();
}

async function scan() {
  const seq = ++scanSeq;
  show("list", { scanning: true, scanErr: "" });
  try {
    const { aps } = await api("/api/device/wifi/scan", { method: "POST", body: {} });
    if (seq === scanSeq) show("list", { scanning: false, aps });
  } catch (e) {
    if (seq === scanSeq) show("list", { scanning: false, scanErr: e.message });
  }
}

function pick(ap) {
  wifiForm.elements.ssid.value = ap.ssid;
  show("pass", { ap, passErr: "", passVal: "" });
}

async function joinWiFi(ap, pass) {
  const f = wifiForm.elements;
  const body = { ssid: ap.ssid, pass };
  if (f.fixed.checked) Object.assign(body, { ip: f.ip.value.trim(), mask: f.mask.value.trim(), gw: f.gw.value.trim() });
  wifiJoin = { ap, phase: "sending" };
  openDialog();
  show("join", { ap, used: false, useErr: "" });
  try {
    const { try: n, device } = await api("/api/device/wifi", { method: "POST", body });
    f.pass.value = "";
    wifiJoin = { ap, phase: "sent", try: n, at: Date.now() };
    setTimeout(drawJoin, SLOW_JOIN_MS + 100);
    if (state) { state.device = device; render(); }
  } catch (e) {
    wifiJoin = { ap, phase: "refused", err: e.message };
  }
  drawJoin();
}

// joinOutcome reads how the last settings sent are doing: sending,
// refused, connecting (slow after a while), ok, failed, or unknown when the
// cable is gone or newer settings took over.
function joinOutcome() {
  const j = wifiJoin;
  if (!j) return null;
  if (j.phase !== "sent") return { kind: j.phase, err: j.err };
  const u = (state && state.device && state.device.usb) || {};
  const n = u.net;
  if (u.state !== "ready" || !n) return { kind: "unknown", why: "USB 线断开了，看不到连接结果。插回 USB 线后，这里会显示屏幕的 WiFi 状态。" };
  if (n.try > j.try) return { kind: "unknown", why: "屏幕已经改用了别的 WiFi 设置。" };
  if (n.try === j.try && n.state === "connected") return { kind: "ok", n };
  if (n.try === j.try && n.state === "failed") return { kind: "failed", n };
  return { kind: "connecting", slow: Date.now() - j.at > SLOW_JOIN_MS };
}

// drawJoin shows the outcome under the form, and in the dialog while it
// is on the join view.
function drawJoin() {
  const o = joinOutcome();
  if (o) {
    const name = `「${wifiJoin.ap.ssid}」`;
    const [text, cls] = {
      sending: ["正在发给屏幕…", ""],
      refused: [o.err, "err"],
      connecting: [`屏幕正在连接 WiFi${name}…`, ""],
      ok: [`屏幕已连上 WiFi${name}`, "ok"],
      failed: [`屏幕连不上 WiFi${name}：${(o.n && o.n.err) || "原因不明"}`, "err"],
      unknown: [o.why, ""],
    }[o.kind];
    // Only news: other messages stay until the outcome changes.
    if (text !== joinText) {
      joinText = text;
      showMsg("#wifi-msg", text, cls === "err");
      if (cls === "ok") $("#wifi-msg").classList.add("ok");
    }
  }
  // State news come every few seconds; redrawing for nothing new would
  // restart the spinner and take the focus.
  if (wifiDlg.open && dlg.view === "join" && joinKey() !== dlg.key) drawDialog();
}

function joinKey() {
  const o = joinOutcome();
  return JSON.stringify([o, dlg.used, dlg.useErr, o && o.n ? screenInSettings(o.n) : null]);
}

function wifiSignal(rssi) {
  return rssi >= -60 ? "信号强" : rssi >= -75 ? "信号中" : "信号弱";
}

function bars(rssi) {
  const on = rssi >= -55 ? 4 : rssi >= -67 ? 3 : rssi >= -78 ? 2 : 1;
  return el("span", { class: "bars", "aria-hidden": "true" }, [1, 2, 3, 4].map((i) => el("i", { class: i <= on ? "on" : "" })));
}

function lockIcon(open) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 16 16");
  svg.setAttribute("class", open ? "lock open" : "lock");
  svg.setAttribute("aria-hidden", "true");
  svg.innerHTML = '<rect x="3" y="7" width="10" height="7" rx="1.5" fill="currentColor"/><path d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2" fill="none" stroke="currentColor" stroke-width="1.6"/>';
  return svg;
}

const waiting = (text, sub) => el("div", { class: "wait" }, el("div", { class: "spinner" }), el("div", {}, el("div", {}, text), sub ? el("div", { class: "dlg-note" }, sub) : null));

function outcome(ok, what, why) {
  return el("div", { class: `outcome ${ok ? "ok" : "err"}`, role: "status" },
    el("div", { class: "mark", "aria-hidden": "true" }, ok ? "✓" : "✕"),
    el("div", {}, el("div", { class: "what" }, what), why ? el("div", { class: "why" }, why) : null));
}

// Back to the list, scanning first when the network was typed in.
const back = () => (dlg.aps ? show("list") : scan());

const btn = (label, onclick, cls = "") => el("button", { type: "button", class: `btn ${cls}`, onclick }, label);

function drawDialog() {
  const body = [], foot = [];
  let title = "选择 WiFi";
  const n = state && state.device && state.device.usb && state.device.usb.net;
  if (dlg.view === "list") {
    if (dlg.scanning) {
      body.push(waiting("屏幕正在扫描附近的 WiFi…", "大约需要 3 秒"));
    } else if (dlg.scanErr) {
      body.push(outcome(false, "扫描失败", dlg.scanErr));
    } else if (!dlg.aps || !dlg.aps.length) {
      body.push(el("p", { class: "dlg-note" }, "屏幕没有找到 WiFi。屏幕只能连 2.4 GHz 的网络，只有 5 GHz 的 WiFi 不会列出来；离路由器太远也收不到。"));
    } else {
      body.push(el("div", { class: "ap-list" }, dlg.aps.map((a) => {
        const here = n && n.state === "connected" && n.ssid === a.ssid;
        return el("button", { type: "button", class: "ap", title: `${wifiSignal(a.rssi)}（${a.rssi} dBm）· ${a.open ? "无密码" : "有密码"}`, onclick: () => pick(a) },
          bars(a.rssi), el("span", { class: "name" }, a.ssid), here ? el("span", { class: "tag" }, "已连接") : null, lockIcon(a.open));
      })));
    }
    foot.push(btn("手动输入", () => { wifiDlg.close(); wifiForm.elements.ssid.focus(); }, "ghost left"));
    if (!dlg.scanning) foot.push(btn("重新扫描", scan, "ghost"));
    foot.push(btn("取消", () => wifiDlg.close()));
  } else if (dlg.view === "pass") {
    const ap = dlg.ap;
    title = `连接「${ap.ssid}」`;
    const f = wifiForm.elements;
    const addr = f.fixed.checked ? `使用固定 IP ${f.ip.value.trim() || "（未填写）"}，可以在页面上的「固定 IP」里修改。` : "地址由路由器自动分配（DHCP）。";
    const pass = el("input", { name: "pass", type: "password", maxlength: "63", autocomplete: "new-password", spellcheck: "false" });
    pass.value = dlg.passVal || "";
    const fm = el("form", { onsubmit: (ev) => {
      ev.preventDefault();
      const p = pass.value;
      if (!ap.open && p.length < 8) {
        show("pass", { passErr: p ? "WiFi 密码至少 8 个字符" : "请输入 WiFi 密码", passVal: p });
        return;
      }
      joinWiFi(ap, ap.open ? "" : p);
    } });
    if (ap.open) {
      fm.append(el("p", { class: "dlg-note" }, "这个 WiFi 没有密码，任何人都能连上。"));
    } else {
      fm.append(el("label", {}, "WiFi 密码", pass),
        el("label", { class: "check" }, el("input", { type: "checkbox", onchange: (ev) => { pass.type = ev.target.checked ? "text" : "password"; } }), " 显示密码"));
    }
    if (dlg.passErr) fm.append(el("p", { class: "dlg-note err", role: "alert" }, dlg.passErr));
    fm.append(el("p", { class: "dlg-note" }, addr, ap.open ? "" : "密码只发给屏幕保存，go-dock 不记录。"));
    body.push(fm);
    const go = btn("连接", () => fm.requestSubmit(), "primary");
    foot.push(btn("返回", back, "ghost left"), go);
    requestAnimationFrame(() => (ap.open ? go : pass).focus());
  } else {
    const o = joinOutcome() || { kind: "sending" };
    const ap = dlg.ap || (wifiJoin && wifiJoin.ap);
    title = `连接「${ap.ssid}」`;
    const retry = () => show("pass", { ap, passErr: "", passVal: "" });
    dlg.key = joinKey();
    if (o.kind === "sending") {
      body.push(waiting("正在把设置发给屏幕…"));
    } else if (o.kind === "refused") {
      body.push(outcome(false, "屏幕没有接受这个设置", o.err));
      foot.push(btn("返回", ap.open ? back : retry, "ghost left"), btn("关闭", () => wifiDlg.close()));
    } else if (o.kind === "connecting") {
      body.push(waiting(`屏幕正在连接「${ap.ssid}」…`, o.slow
        ? "30 秒了还没连上，屏幕仍在尝试。可能是信号太弱，或者路由器没有给屏幕分配地址。"
        : "通常需要 5–15 秒"));
      foot.push(btn("关闭", () => wifiDlg.close()));
    } else if (o.kind === "ok") {
      const facts = el("dl", { class: "facts" },
        el("dt", {}, "地址"), el("dd", {}, wifiAddr(o.n) + (o.n.fixed ? "（固定 IP）" : "")),
        el("dt", {}, "配对码"), el("dd", {}, o.n.code));
      body.push(outcome(true, `已连上「${ap.ssid}」`), facts);
      if (screenInSettings(o.n)) {
        body.push(el("p", { class: "dlg-note" }, dlg.used
          ? "已保存，go-dock 正通过 WiFi 连接屏幕。之后可以拔掉 USB 线（屏幕要另外供电）。"
          : "go-dock 的设置里已经是这块屏幕，会自动通过 WiFi 连接它。"));
        foot.push(btn("完成", () => wifiDlg.close(), "primary"));
      } else {
        body.push(el("p", { class: "dlg-note" }, "让 go-dock 通过 WiFi 显示到这块屏幕上，需要把它的地址和配对码填进设置。"));
        if (dlg.useErr) body.push(el("p", { class: "dlg-note err", role: "alert" }, dlg.useErr));
        const use = btn("使用这块屏幕", async () => {
          use.disabled = true;
          try {
            await useScreen(o.n);
            show("join", { used: true, useErr: "" });
          } catch (e) {
            show("join", { useErr: e.message });
          }
        }, "primary");
        foot.push(btn("以后再说", () => wifiDlg.close(), "ghost"), use);
      }
    } else if (o.kind === "failed") {
      body.push(outcome(false, `连不上「${ap.ssid}」`, (o.n && o.n.err) || "原因不明"),
        el("p", { class: "dlg-note" }, "屏幕会每隔几秒自动重试，连上后这里会更新。"));
      if (!ap.open) foot.push(btn("重新输入密码", retry, "ghost"));
      foot.unshift(btn("选择其他 WiFi", scan, "ghost left"));
      foot.push(btn("关闭", () => wifiDlg.close()));
    } else {
      body.push(el("p", { class: "dlg-note" }, o.why));
      foot.push(btn("关闭", () => wifiDlg.close()));
    }
  }
  $("#wifi-dlg-title").textContent = title;
  $("#wifi-dlg-body").replaceChildren(...body);
  $("#wifi-dlg-foot").replaceChildren(...foot);
}

// Alert sounds come from the host, so the lists and the browser play the
// same notes as the device. Each sound select names its custom notes field
// (data-custom).
const soundSelects = [...form.querySelectorAll("select[data-custom]")];
let sounds = null;
async function loadSounds() {
  if (sounds) return;
  sounds = (await api("/api/device/sounds")) || [];
  for (const select of soundSelects) {
    select.replaceChildren(
      ...sounds.map((s) => el("option", { value: s.id }, s.name)),
      el("option", { value: "custom" }, "自定义…"));
  }
}

// A notes field only matters for the custom sound.
function showCustomSound() {
  for (const select of soundSelects) {
    form.elements[select.dataset.custom].closest("label").hidden = select.value !== "custom";
  }
}
for (const select of soundSelects) select.addEventListener("change", showCustomSound);

// playNotes stands in for the buzzer with a quiet square wave. Each volume
// percent below 100 takes 0.5 dB off, as the firmware's beep.c does.
let audio = null;
function playNotes(notes, volume) {
  audio = audio || new AudioContext();
  const level = 0.05 * 10 ** ((volume - 100) * 0.5 / 20);
  let t = audio.currentTime + 0.05;
  for (const [hz, ms] of notes) {
    if (hz > 0) {
      const osc = audio.createOscillator();
      const gain = audio.createGain();
      osc.type = "square";
      osc.frequency.value = hz;
      gain.gain.value = level;
      osc.connect(gain).connect(audio.destination);
      osc.start(t);
      osc.stop(t + ms / 1000);
    }
    t += ms / 1000;
  }
}

// Tries the sound chosen, custom notes as written and the volume chosen
// (saved or not), on the screen's buzzer or in the browser.
async function trySound(button) {
  const select = form.elements[button.dataset.sound];
  const volume = Number(form.elements["device.volume"].value);
  const body = { sound: select.value, custom: form.elements[select.dataset.custom].value, volume };
  try {
    if (button.dataset.try === "browser") {
      playNotes((await api("/api/device/notes", { method: "POST", body })).notes, volume);
      showMsg("#settings-msg", "正在电脑上播放");
      return;
    }
    await api("/api/device/beep", { method: "POST", body });
    showMsg("#settings-msg", "已发到屏幕播放；没接蜂鸣器时听不到");
  } catch (e) {
    showMsg("#settings-msg", e.message, true);
  }
}
for (const b of form.querySelectorAll("button[data-try]")) b.addEventListener("click", () => trySound(b));

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

// ---------- plugins ----------

const perfForm = $("#perf-form");

async function loadPlugins() {
  try {
    loadedConfig = await api("/api/config");
    fillForm(perfForm, loadedConfig);
    showMsg("#perf-msg", "");
  } catch (e) {
    showMsg("#perf-msg", e.message, true);
  }
}

perfForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    loadedConfig = await api("/api/config", { method: "POST", body: readForm(perfForm) });
    showMsg("#perf-msg", "已保存");
  } catch (e) {
    showMsg("#perf-msg", `保存失败：${e.message}`, true);
  }
  render();
});

// Live state only: the inputs are left alone so typing is never lost.
function renderPluginsLive() {
  if (!state) return;
  const line = lineItems();
  $("#perf-summary").textContent = line.length
    ? line.map((it) => `${it.label} ${it.text}${it.unit || ""}`).join(" · ")
    : (loadedConfig && loadedConfig.plugins && loadedConfig.plugins.perf && loadedConfig.plugins.perf.enabled ? "读取中…" : "未启用");
}

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
