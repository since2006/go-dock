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
let previewFn = "agent"; // the page shown: "agent" or a function page id
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
  const n = (s.agents || []).filter((a) => ACTIVE.includes(a.state)).length;
  return n ? `等待 ${n} 个子任务完成` : "";
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
  moon: '<path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"/>', // also a function page icon
  bulb: '<path d="M9 18h6"/><path d="M10 21h4"/><path d="M12 3a6 6 0 0 0-3.6 10.8c.7.6 1.1 1.4 1.1 2.2h5c0-.8.4-1.6 1.1-2.2A6 6 0 0 0 12 3z"/>',
  // Function page icons (item.icon), lines and arcs only, as the firmware draws them.
  power: '<path d="M12 3v8"/><path d="M6.4 6.4a8 8 0 1 0 11.2 0"/>',
  cool: '<path d="M12 2.5v19M3.8 7.3l16.4 9.4M3.8 16.7l16.4-9.4"/><path d="m9.5 4 2.5 2 2.5-2M9.5 20l2.5-2 2.5 2"/>',
  heat: '<circle cx="12" cy="12" r="4"/><path d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M5.3 18.7l1.4-1.4M17.3 6.7l1.4-1.4"/>',
  dry: '<path d="M12 3 6.8 11.5A6 6 0 1 0 17.2 11.5z"/>',
  fan: '<path d="M3 8h9a2.5 2.5 0 1 0-2.5-2.5"/><path d="M3 12h15a2.5 2.5 0 1 1-2.5 2.5"/><path d="M3 16h7a2 2 0 1 1-2 2"/>',
  auto: '<path d="M20 12a8 8 0 0 1-14 5.3"/><path d="M4 12a8 8 0 0 1 14-5.3"/><path d="M18 3v3.7h-3.7M6 21v-3.7h3.7"/>',
  bell: '<path d="M6 14V9a6 6 0 0 1 12 0v5l2 2.5H4z"/><path d="M10 19.5a2 2 0 0 0 4 0"/>',
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
// assistant's latest text; older hosts only send text, which may be the
// title. None leaves the project line alone, centred in the row.
function scrText(s) {
  return s.detail || s.msg || (s.text && s.text !== s.title ? s.text : "");
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
        el("span", { class: "end" }, el("span", { class: "label" }, s.label))),
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

// The strip on top of every page: title and summary (or why the page cannot
// be used now) on the left; page dots, link and clock on the right.
function scrStrip(title, summary, now, dots, error) {
  const clock = new Date(now * 1000).toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });
  return el("div", { class: "scr-strip" },
    el("div", { class: "left" }, el("span", { class: "title" }, title),
      error ? el("span", { class: "summary err" }, error) : el("span", { class: "summary" }, summary)),
    el("div", { class: "right" }, dots, el("span", { class: `link ${hostConnected ? "" : "off"}` }), el("span", { class: "clock mono" }, clock)),
  );
}

// Horizontal page dots, the current one stretched; none for a single page.
function scrDots(n, cur) {
  if (n < 2) return null;
  return el("span", { class: "scr-dots" }, ...Array.from({ length: n }, (_, i) => el("i", { class: i === cur ? "on" : "" })));
}

function scrAgentBody(frame, dots) {
  const now = frame.now;
  const sessions = frame.sessions || [];
  const counts = frame.counts || {};
  const summary = SCR_ACTIVE.filter((k) => counts[k]).map((k) => `${STATE_LABEL[k]} ${counts[k]}`).join(" · ")
    || (sessions.length ? "全部完成" : "");
  const strip = scrStrip("Agent", summary, now, dots);

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
  return [strip, usage, tasks];
}

// Function page layout, shared with the firmware (docs/protocol-v1.md §7.1):
// under the strip a 564x380 area holds the items as cards in a grid.
const SCR_PAGE_W = 564, SCR_PAGE_H = 380, SCR_CARD_GAP = 12;

// Columns and rows for n cards: one row up to three, then 2x2 and 3x2.
function scrGrid(n) {
  if (n <= 3) return [Math.max(n, 1), 1];
  return n === 4 ? [2, 2] : [3, 2];
}

function scrFnBody(page, now, dots) {
  const items = page.items || [];
  const [cols, rows] = scrGrid(items.length);
  const w = (SCR_PAGE_W - (cols - 1) * SCR_CARD_GAP) / cols;
  const h = (SCR_PAGE_H - (rows - 1) * SCR_CARD_GAP) / rows;
  const grid = items.length
    ? el("div", { class: "scr-grid", style: `grid-template-columns:repeat(${cols},${w}px);grid-template-rows:repeat(${rows},${h}px)` },
      ...items.map((it) => scrCard(page, it, rows === 1)))
    : el("div", { class: "scr-empty" }, el("span", { class: "s" }, "这一页还没有内容"));
  return [scrStrip(page.title, page.summary || "", now, dots, page.error), grid];
}

// A card: its icon (a lamp unless the item names another), label and
// text. A switch shows on / off and toggles, or with buttons takes only
// 开 and 关 (the one matching its state stands out); a select takes the
// next option, coloured by its icon; a stepper is drawn by scrStepper.
// Cards of kinds the screen does not know show their texts but take no
// taps.
function scrCard(page, it, big) {
  if (it.kind === "stepper") return scrStepper(page, it, big);
  const sw = it.kind === "switch";
  const btns = sw && it.buttons;
  const st = it.busy ? "busy" : !sw ? "plain" : it.on === true ? "on" : it.on === false ? "off" : "unknown";
  const live = !it.disabled && hostConnected;
  const icon = SCR_ICONS[it.icon] ? it.icon : "bulb";
  const content = [
    el("span", { class: "chip" }, scrIcon(icon, big ? 46 : btns ? 24 : 30, 2)),
    el("span", { class: "label" }, it.label),
    el("span", { class: "text" }, it.text),
  ];
  const cls = `scr-card ${st} icon-${icon} ${big ? "big" : ""} ${it.disabled ? "disabled" : ""}`;
  if (btns) {
    const button = (act, name, lit) => el("button", {
      type: "button", class: `sw-btn ${act} ${lit ? "lit" : ""}`, disabled: live ? null : "",
      onclick: () => previewTap(page.id, it, act),
    }, name);
    return el("div", { class: `${cls} with-btns` }, ...content, el("span", { class: "sw-btns" },
      button("on", "开", !it.busy && it.on === true), button("off", "关", !it.busy && it.on === false)));
  }
  return el("button", {
    type: "button", class: cls,
    disabled: (sw || it.kind === "select") && live ? null : "",
    onclick: () => previewTap(page.id, it, sw ? "toggle" : "next"),
  }, ...content);
}

// A stepper card: the value between − and +, its label below; a button
// at the end of the range is greyed.
function scrStepper(page, it, big) {
  const dead = it.disabled || !hostConnected;
  const step = (act, sign, end) => el("button", {
    type: "button", class: "step", disabled: dead || end ? "" : null, "aria-label": `${it.label}${sign}`,
    onclick: () => previewTap(page.id, it, act),
  }, sign);
  return el("div", { class: `scr-card stepper ${big ? "big" : ""} ${it.disabled ? "disabled" : ""}` },
    el("span", { class: "steps" }, step("dec", "−", it.at_min), el("span", { class: "value mono" }, it.text), step("inc", "+", it.at_max)),
    el("span", { class: "label" }, it.label));
}

function fnPages() { return (state && state.pages && state.pages.pages) || []; }

// The pages left to right: Agent first, then the function pages in order.
function previewIds() { return ["agent", ...fnPages().map((p) => p.id)]; }

function goFn(delta) {
  const ids = previewIds();
  const i = Math.max(0, Math.min(ids.length - 1, ids.indexOf(previewFn) + delta));
  if (ids[i] === previewFn) return;
  previewFn = ids[i];
  if (state) renderScreen(state.frame);
}

function goTaskPage(delta) {
  if (previewFn !== "agent" || scrTaskPages < 2) return;
  previewPage = (previewPage + delta + scrTaskPages) % scrTaskPages;
  if (state) renderScreen(state.frame);
}

async function previewTap(pageId, it, act) {
  if (it.kind === "switch") {
    it.busy = true; // until the host answers
    renderScreen(state.frame);
  }
  try {
    state.pages = await api(`/api/plugins/${pageId}/${it.id}`, { method: "POST", body: { act } });
    showMsg("#preview-msg", "");
  } catch (e) {
    showMsg("#preview-msg", e.message, true);
  }
  render();
}

function renderScreen(frame) {
  const screen = $("#screen");
  if (!frame) { screen.replaceChildren(); return; }
  const ids = previewIds();
  if (!ids.includes(previewFn)) previewFn = "agent";
  const cur = ids.indexOf(previewFn);
  const page = fnPages().find((p) => p.id === previewFn);
  const dots = scrDots(ids.length, cur);
  const body = el("div", { class: `scr-body ${hostConnected ? "" : "dim"}` },
    ...(page ? scrFnBody(page, frame.now, dots) : scrAgentBody(frame, dots)));
  const parts = [body];
  if (!hostConnected) {
    parts.push(el("div", { class: "scr-offline" }, el("div", { class: "card" },
      scrIcon("unplug", 44, 1.9, "#FF6B5E"),
      el("span", { class: "t" }, "主机离线"),
      el("span", { class: "s" }, "网页与 go-dock 的连接已断开"))));
  }
  screen.replaceChildren(...parts);
  fitScreen();

  $("#fn-name").textContent = `${page ? page.title : "Agent"}  ${cur + 1} / ${ids.length}`;
  $("#fn-prev").disabled = cur === 0;
  $("#fn-next").disabled = cur === ids.length - 1;
}

$("#fn-prev").addEventListener("click", () => goFn(-1));
$("#fn-next").addEventListener("click", () => goFn(1));
document.addEventListener("keydown", (e) => {
  if ($("#tab-preview").hidden || e.altKey || e.ctrlKey || e.metaKey || e.target.closest("input, select, textarea")) return;
  if (e.key === "ArrowLeft") { e.preventDefault(); goFn(-1); }
  if (e.key === "ArrowRight") { e.preventDefault(); goFn(1); }
});

// Swipes on the preview, as on the screen: left/right switches pages,
// up/down turns task pages. A swipe never also taps a card.
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
    if (Math.abs(dx) > Math.abs(dy)) goFn(dx < 0 ? 1 : -1);
    else goTaskPage(dy < 0 ? 1 : -1);
  });
  screen.addEventListener("click", (e) => { if (swallow) { e.preventDefault(); e.stopPropagation(); } }, true);
}

function renderDeviceDetail(dev) {
  const dl = $("#device-detail");
  const fw = el("span", {}, dev.firmware || "—");
  if (FLASH_BUSY.includes(dev.flash && dev.flash.state)) fw.append(" · 正在升级");
  else if (dev.upgradable) fw.append(" ", el("button", { class: "btn small", onclick: applyFirmware }, `升级到 ${dev.target}`));
  else if (dev.manual) fw.append(" · 需要用 USB 刷一次才能在线升级");
  const rows = [
    ["状态", { disabled: "已禁用", searching: "查找中", connected: "已打开串口，等待设备握手", ready: "已连接" }[dev.state] || dev.state],
    ["串口", dev.port || "—"],
    ["固件", fw],
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
  renderFirmwareBanner(state.device || {});
  renderUsage(state.snapshot);
  renderSessions(state.snapshot);
  renderDevicePill(state.device);
  renderDeviceDetail(state.device || {});
  renderScreen(state.frame);
  $("#frame-json").textContent = JSON.stringify(state.frame, null, 2);
  $("#pages-json").textContent = JSON.stringify(state.pages, null, 2);
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

const FLASH_BUSY = ["downloading", "preparing", "sending", "restarting"];
const FIRMWARE_HELP = "https://github.com/since2006/go-dock/blob/main/firmware/README.md#方式一直接烧录发布的固件";
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
  if (f.state === "downloading") {
    parts = [msg(`正在下载固件 ${dev.target || ""}…`)];
  } else if (f.state === "preparing") {
    parts = [msg(`正在准备升级固件 ${f.version}，设备在擦除存储，约需 10 秒…`)];
  } else if (f.state === "sending") {
    const pct = f.total ? Math.floor((f.sent * 100) / f.total) : 0;
    parts = [msg(`正在升级固件 ${f.version}：${pct}%，请勿拔 USB 线`)];
  } else if (f.state === "restarting") {
    parts = [msg("固件已写入，设备正在重启…")];
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
      msg(`设备固件可以升级到 ${dev.target}（当前 ${dev.firmware}）`),
      el("button", { class: "btn primary", onclick: applyFirmware }, "升级固件"),
      firmwareLater(dev.target),
    ];
  } else if (dev.manual && !firmwareDismissed(`manual:${dev.target}`)) {
    parts = [
      msg(`设备固件 ${dev.firmware} 还不支持在线升级，需要用 USB 刷一次新固件，以后就能在这里升级`),
      el("a", { href: FIRMWARE_HELP, target: "_blank", rel: "noopener" }, "刷写说明"),
      firmwareLater(`manual:${dev.target}`),
    ];
  }
  box.hidden = !parts;
  if (parts) { box.className = cls; box.replaceChildren(...parts); }
}

async function applyFirmware() {
  const dev = (state && state.device) || {};
  if (!confirm(`把设备固件升级到 ${dev.target}？大约需要 1 分钟，屏幕会显示进度，期间不要拔 USB 线。`)) return;
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

// playNotes stands in for the buzzer with a quiet square wave.
let audio = null;
function playNotes(notes) {
  audio = audio || new AudioContext();
  let t = audio.currentTime + 0.05;
  for (const [hz, ms] of notes) {
    if (hz > 0) {
      const osc = audio.createOscillator();
      const gain = audio.createGain();
      osc.type = "square";
      osc.frequency.value = hz;
      gain.gain.value = 0.05;
      osc.connect(gain).connect(audio.destination);
      osc.start(t);
      osc.stop(t + ms / 1000);
    }
    t += ms / 1000;
  }
}

// Tries the sound chosen, custom notes as written (saved or not), on the
// screen's buzzer or in the browser.
async function trySound(button) {
  const select = form.elements[button.dataset.sound];
  const body = { sound: select.value, custom: form.elements[select.dataset.custom].value };
  try {
    if (button.dataset.try === "browser") {
      playNotes((await api("/api/device/notes", { method: "POST", body })).notes);
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

const mqttForm = $("#mqtt-form");
const lightsForm = $("#lights-form");
const airconForm = $("#aircon-form");

// Editable list rows: each field is [config key, label, placeholder, type].
// Inputs use data-k, not name, so the dotted-name form helpers skip them;
// data-id on the row is the id it was saved under, empty for a new one.
function rowField(entry, [k, label, ph, type]) {
  if (type === "checkbox") {
    return el("label", { class: "check" }, el("input", { type: "checkbox", "data-k": k, checked: entry[k] ? "" : null }), label);
  }
  const saved = k === "id" && entry.saved;
  return el("label", {}, label, el("input", {
    "data-k": k, value: entry[k] || "", placeholder: ph, type: type || null,
    readonly: saved ? "" : null, title: saved ? "保存后不能修改：插件用它引用这一项" : null,
    autocomplete: type === "password" ? "new-password" : "off",
  }));
}

function readRow(row) {
  const out = {};
  for (const input of row.querySelectorAll("[data-k]")) {
    const k = input.dataset.k;
    out[k] = input.type === "checkbox" ? input.checked : input.type === "password" ? input.value : input.value.trim();
  }
  return out;
}

// A fresh id like broker1 or light1 that no row uses yet.
function nextID(list, prefix) {
  const ids = new Set([...list.querySelectorAll('[data-k="id"]')].map((i) => i.value));
  let n = 1;
  while (ids.has(`${prefix}${n}`)) n++;
  return `${prefix}${n}`;
}

// Broker fields, shown three to a row.
const BROKER_FIELDS = [
  ["name", "名称", "如：办公室"],
  ["id", "ID", "小写字母、数字、- 或 _"],
  ["url", "地址", "192.168.1.10:1883"],
  ["username", "用户名", ""],
  ["password", "密码", "", "password"],
  ["client_id", "Client ID", "留空自动生成"],
];

// Light fields by config key; lightRow lays them out.
const LIGHT_FIELDS = Object.fromEntries([
  ["name", "名称", "如：办公室主灯"],
  ["id", "ID", "小写字母、数字、- 或 _"],
  ["command_topic", "命令主题", "如：office/light/set"],
  ["payload_on", "开灯时发送", 'ON / {"state":"ON"}'],
  ["payload_off", "关灯时发送", 'OFF / {"state":"OFF"}'],
  ["state_topic", "状态主题（可选）", "灯回报状态的主题"],
  ["state_path", "状态字段（可选）", "如 state、params.Onoff"],
  ["state_on", "表示「开」的状态值", "见上方说明"],
  ["state_off", "表示「关」的状态值", "见上方说明"],
  ["query_topic", "查询主题（可选）", "如 office/light/get"],
  ["query_payload", "查询时发送", '留空 / {"state":""}'],
].map((f) => [f[0], f]));
const LIGHT_QUERY_NOTE = "填了查询主题，每次连上 broker 都会发一次查询，让灯回报当前状态。";

// fieldRow lays fields out in n equal columns; fewer fields leave the
// rest empty, so a lone field lines up with the row above.
function fieldRow(n, ...fields) {
  return el("div", { class: "field-row", style: `--n:${n}` }, ...fields);
}

const savedBrokers = () => (loadedConfig && loadedConfig.mqtt && loadedConfig.mqtt.brokers) || [];
const brokerLabel = (b) => (b.name || "").trim() || b.id;

async function loadPlugins() {
  try {
    loadedConfig = await api("/api/config");
    fillForm(lightsForm, loadedConfig);
    fillForm(airconForm, loadedConfig);
    renderBrokerRows(savedBrokers());
    renderLightRows(loadedConfig.plugins.lights.lights || []);
    showMsg("#mqtt-msg", "");
    showMsg("#lights-msg", "");
    showMsg("#aircon-msg", "");
  } catch (e) {
    showMsg("#lights-msg", e.message, true);
  }
}

function brokerRow(b) {
  const row = el("div", { class: "light-row", "data-id": b.saved ? b.id : "" },
    el("div", { class: "head" },
      el("b", {}, b.saved ? brokerLabel(b) : "新的 broker"),
      el("span", { class: "pill live" }, b.saved ? "—" : "未保存"),
      rowField(b, ["enabled", "启用", "", "checkbox"]),
      el("button", { type: "button", class: "btn small danger", onclick: () => { row.remove(); updateAddButtons(); } }, "删除")),
    el("div", { class: "grid three" }, ...BROKER_FIELDS.map((f) => rowField(b, f))),
  );
  return row;
}

function renderBrokerRows(list) {
  $("#broker-list").replaceChildren(...list.map((b) => brokerRow({ ...b, saved: true })));
  updateAddButtons();
  renderPluginsLive();
}

// The broker a light uses. With one broker or none the choice is hidden
// and an empty value means "the first broker"; with more every light names
// its own, and a broker that is gone stays visible so it can be changed.
function fillBrokerSelect(sel, current) {
  const bs = savedBrokers();
  const known = bs.some((b) => b.id === current);
  const hide = bs.length <= 1 && (current === "" || known);
  const opts = hide ? [[current, ""]] : bs.map((b) => [b.id, brokerLabel(b)]);
  if (!hide && current && !known) opts.push([current, `${current}（已删除）`]);
  sel.replaceChildren(...opts.map(([v, t]) => el("option", { value: v }, t)));
  sel.value = !hide && current === "" && bs.length ? bs[0].id : current;
  sel.closest("label").hidden = hide;
}

function lightRow(l, saved) {
  const field = (k) => rowField({ ...l, saved }, LIGHT_FIELDS[k]);
  const pick = el("select", { "data-k": "broker" });
  const row = el("div", { class: "light-row", "data-id": saved ? l.id : "" },
    el("div", { class: "head" },
      el("b", {}, l.name || "新的灯"),
      el("span", { class: "pill live" }, saved ? "—" : "未保存"),
      el("button", { type: "button", class: "btn small", "data-act": "on" }, "开"),
      el("button", { type: "button", class: "btn small", "data-act": "off" }, "关"),
      el("button", { type: "button", class: "btn small danger", onclick: () => { row.remove(); updateAddButtons(); } }, "删除")),
    fieldRow(3, field("name"), field("id"), el("label", {}, "MQTT broker", pick)),
    fieldRow(1, field("command_topic")),
    fieldRow(2, field("payload_on"), field("payload_off")),
    el("hr"),
    fieldRow(1, field("state_topic")),
    fieldRow(3, field("state_path"), field("state_on"), field("state_off")),
    el("details", {}, el("summary", {}, "更多选项"),
      el("div", { class: "rows" }, fieldRow(1, field("query_topic")), fieldRow(2, field("query_payload"))),
      el("small", { class: "muted" }, LIGHT_QUERY_NOTE)),
  );
  fillBrokerSelect(pick, l.broker || "");
  for (const b of row.querySelectorAll("[data-act]")) b.addEventListener("click", () => lightAct(row.dataset.id, b.dataset.act, b));
  return row;
}

function renderLightRows(list) {
  $("#lights-list").replaceChildren(...list.map((l) => lightRow(l, true)));
  updateAddButtons();
  renderPluginsLive();
}

function updateAddButtons() {
  $("#add-broker").disabled = $("#broker-list").children.length >= 8;
  $("#add-light").disabled = $("#lights-list").children.length >= 6;
}

$("#add-broker").addEventListener("click", () => {
  $("#broker-list").append(brokerRow({ id: nextID($("#broker-list"), "broker"), enabled: true }));
  updateAddButtons();
  renderPluginsLive();
});

$("#add-light").addEventListener("click", () => {
  $("#lights-list").append(lightRow({ id: nextID($("#lights-list"), "light") }, false));
  updateAddButtons();
  renderPluginsLive();
});

async function lightAct(id, act, btn) {
  if (!id) return;
  btn.disabled = true;
  try {
    state.pages = await api(`/api/plugins/lights/${id}`, { method: "POST", body: { act } });
    showMsg("#lights-msg", "");
  } catch (e) {
    showMsg("#lights-msg", e.message, true);
  }
  render();
}

mqttForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const brokers = [...$("#broker-list").children].map(readRow);
  try {
    loadedConfig = await api("/api/config", { method: "POST", body: { mqtt: { brokers } } });
    renderBrokerRows(savedBrokers());
    for (const sel of lightsForm.querySelectorAll('select[data-k="broker"]')) fillBrokerSelect(sel, sel.value);
    showMsg("#mqtt-msg", "已保存");
  } catch (e) {
    showMsg("#mqtt-msg", `保存失败：${e.message}`, true);
  }
});

airconForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    loadedConfig = await api("/api/config", { method: "POST", body: readForm(airconForm) });
    showMsg("#aircon-msg", "已保存");
  } catch (e) {
    showMsg("#aircon-msg", `保存失败：${e.message}`, true);
  }
  render();
});

for (const b of airconForm.querySelectorAll("[data-ac]")) {
  b.addEventListener("click", async () => {
    const [item, act] = b.dataset.ac.split(":");
    b.disabled = true;
    try {
      state.pages = await api(`/api/plugins/aircon/${item}`, { method: "POST", body: { act } });
      showMsg("#aircon-msg", "");
    } catch (e) {
      showMsg("#aircon-msg", e.message, true);
    }
    render();
  });
}

lightsForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const body = readForm(lightsForm);
  body.plugins.lights.lights = [...$("#lights-list").children].map(readRow);
  try {
    loadedConfig = await api("/api/config", { method: "POST", body });
    renderLightRows(loadedConfig.plugins.lights.lights || []);
    showMsg("#lights-msg", "已保存");
  } catch (e) {
    showMsg("#lights-msg", `保存失败：${e.message}`, true);
  }
});

const MQTT_STATE = { disabled: ["未启用", ""], connecting: ["连接中…", "warn"], connected: ["已连接", "ok"], error: ["连接失败", "bad"] };
const LIGHT_TEXTS_OK = ["已开", "已关", "未知", "切换中…"];

// Live state only: the inputs are left alone so typing is never lost.
function renderPluginsLive() {
  if (!state) return;
  const brokers = Array.isArray(state.mqtt) ? state.mqtt : [];
  const up = brokers.filter((b) => b.state === "connected").length;
  $("#mqtt-summary").textContent = brokers.length ? `${up} / ${brokers.length} 个已连接` : "";
  for (const row of $("#broker-list").children) {
    const b = row.dataset.id ? brokers.find((x) => x.id === row.dataset.id) : null;
    const live = row.querySelector(".live");
    const [text, cls] = b ? MQTT_STATE[b.state] || ["—", ""] : ["未保存", ""];
    live.textContent = b && b.state === "error" && b.error ? `${text}：${b.error}` : text;
    live.className = `pill live ${cls}`;
    live.title = b && b.client_id ? `client id：${b.client_id}` : "";
  }

  const page = fnPages().find((p) => p.id === "lights");
  $("#lights-summary").textContent = page ? page.summary || "" : "";
  for (const row of $("#lights-list").children) {
    const it = page && row.dataset.id ? (page.items || []).find((i) => i.id === row.dataset.id) : null;
    const live = row.querySelector(".live");
    if (!row.dataset.id) {
      live.textContent = "未保存";
      live.className = "pill live";
    } else if (!it) {
      live.textContent = "未启用";
      live.className = "pill live";
    } else {
      live.textContent = it.disabled ? (brokers.length ? "MQTT 未连接" : "没有配置 MQTT") : it.text;
      live.className = `pill live ${it.disabled || !LIGHT_TEXTS_OK.includes(it.text) ? "bad" : it.busy || it.on === undefined ? "warn" : it.on ? "ok" : ""}`;
    }
    for (const b of row.querySelectorAll("[data-act]")) b.disabled = !it || it.disabled;
  }

  // The air conditioner: what the page says, and its buttons greyed as its
  // items are (no infrared, temperature held in auto, fan held in dry).
  const ac = fnPages().find((p) => p.id === "aircon");
  $("#aircon-summary").textContent = ac ? ac.summary || "" : "";
  const acLive = $("#aircon-state");
  acLive.textContent = !ac ? "未启用" : ac.error || ac.summary || "";
  acLive.className = `pill live ${!ac ? "" : ac.error ? "bad" : (ac.items || []).some((i) => i.id === "power" && i.on) ? "ok" : ""}`;
  for (const b of airconForm.querySelectorAll("[data-ac]")) {
    const [id, act] = b.dataset.ac.split(":");
    const it = ac && (ac.items || []).find((i) => i.id === id);
    b.disabled = !it || it.disabled || (act === "dec" && it.at_min) || (act === "inc" && it.at_max);
  }
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
