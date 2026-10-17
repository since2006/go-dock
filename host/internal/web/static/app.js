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
  const rows = (snap.sessions || []).filter((s) => showIdle || s.state !== "idle");
  if (!rows.length) {
    list.replaceChildren(el("div", { class: "empty" },
      snap.sessions && snap.sessions.length ? "没有活跃任务（勾选「显示空闲会话」查看全部）" : "暂无会话。启动 Claude Code 或 Codex 后会出现在这里。"));
    return;
  }
  list.replaceChildren(...rows.map((s) => el("div", { class: `session ${s.state}`, title: s.cwd || "" },
    el("span", {}, el("span", { class: `badge ${s.state}` }, STATE_LABEL[s.state] || s.state)),
    el("span", { class: `prov ${s.provider}` }, PROVIDER_NAME[s.provider] || s.provider),
    el("span", { class: "project" }, s.project || s.session_id.slice(0, 8)),
    el("span", { class: "text" }, s.detail || s.title || ""),
    el("span", { class: "time", title: `来源：${s.source || "—"}` }, ago(s.since, now)),
  )));
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

function scrWindow(label, w, now) {
  const pct = w ? w.pct : null;
  const reset = w && w.resets_at ? until(w.resets_at, now) : "";
  return el("div", { class: "scr-win" },
    el("div", { class: "top" }, el("span", {}, label), el("b", {}, pct === null ? "—" : `${Math.round(pct)}%`)),
    el("div", { class: "bar" }, el("div", { class: `fill ${level(pct || 0)}`, style: `width:${Math.min(100, pct || 0)}%` })),
    el("div", { class: "reset" }, reset || " "),
  );
}

function renderScreen(frame) {
  const screen = $("#screen");
  if (!frame) { screen.replaceChildren(); return; }
  const now = frame.now * 1000;
  const usage = el("div", { class: "scr-usage" }, ...["claude", "codex"].map((p) => {
    const u = (frame.usage && frame.usage[p]) || {};
    return el("div", { class: "scr-urow" },
      el("span", { class: `scr-name ${p}` }, PROVIDER_NAME[p]),
      scrWindow("5h", u.five_hour, now),
      scrWindow("7d", u.seven_day, now),
      el("div", { class: "scr-today" }, el("b", {}, fmtTokens(u.today_tokens || 0)), "今日"),
    );
  }));

  const sessions = frame.sessions || [];
  const pages = Math.max(1, Math.ceil(sessions.length / 4));
  previewPage = Math.min(previewPage, pages - 1);
  const shown = sessions.slice(previewPage * 4, previewPage * 4 + 4);
  const list = el("div", { class: "scr-list" }, shown.length ? shown.map((s) => el("div", { class: `scr-item ${s.state}` },
    el("span", { class: "stripe" }),
    el("div", { class: "main" },
      el("div", { class: "l1" }, el("span", { class: `p ${s.p}` }, PROVIDER_NAME[s.p] || s.p), s.project),
      el("div", { class: "l2" }, s.text || " "),
    ),
    el("div", { class: "right" },
      el("div", { class: "state" }, s.label),
      el("div", { class: "ago" }, fmtDuration(frame.now - s.since)),
    ),
  )) : el("div", { class: "scr-empty" }, "暂无任务"));

  const foot = el("div", { class: "scr-foot" },
    el("button", { onclick: () => { previewPage = (previewPage - 1 + pages) % pages; renderScreen(frame); }, "aria-label": "上一页" }, "‹"),
    el("span", { class: "dots" }, ...Array.from({ length: pages }, (_, i) => el("i", { class: i === previewPage ? "on" : "" }))),
    el("button", { onclick: () => { previewPage = (previewPage + 1) % pages; renderScreen(frame); }, "aria-label": "下一页" }, "›"),
  );
  screen.replaceChildren(usage, list, foot);
  fitScreen();
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
  es.addEventListener("open", () => { pill.textContent = "已连接"; pill.className = "pill ok"; });
  es.addEventListener("error", () => { pill.textContent = "已断开，重连中…"; pill.className = "pill bad"; });
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
