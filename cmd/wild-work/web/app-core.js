// wild-work 管理面板前端（原生 JS，无构建步骤，直接 fetch 管理 API）
"use strict";

const $ = (id) => document.getElementById(id);

// ---------- API 封装 ----------
// 管理 API 会话：cookie 为 HttpOnly 不读（fetch 需 same-origin 自动携带），
// 前端只持有「口令指纹」用于判断登录态（session=state.auth_session）。
let authSession = "";
async function api(path, body) {
  const opts = { method: "GET", headers: { "Content-Type": "application/json" }, credentials: "same-origin" };
  if (body !== undefined) {
    opts.method = "POST";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    if (resp.status === 401 && data.need_login) showLogin(); // 会话失效：即时弹登录层
    throw new Error(data.error || ("请求失败 " + resp.status));
  }
  return data;
}

// ---------- 管理面板登录 ----------
// 注意：id 一律带 admin 前缀，与「添加账号」弹层（loginOverlay/loginMsg）语义不同，切勿混用。
// 已渲染过面板数据时不再只盖一层遮罩，而是整页重载：彻底清掉 DOM/内存里的账号与密钥
// （登出或会话失效后残留展示不安全）。首次加载（state 为 null）不重载，避免死循环。
function showLogin(msg) {
  const hadData = state !== null;
  state = null;
  if (hadData) { location.reload(); return; }
  $("adminLoginOverlay").classList.remove("hidden");
  $("adminLoginErr").textContent = msg || "";
  $("adminLoginPass").focus();
}

async function submitLogin() {
  const pass = $("adminLoginPass").value;
  if (!pass) { $("adminLoginErr").textContent = "请输入管理员密码"; return; }
  $("btnAdminLogin").disabled = true;
  try {
    const r = await api("/api/auth/login", { password: pass });
    authSession = r.session || "";
    $("adminLoginPass").value = "";
    $("adminLoginOverlay").classList.add("hidden");
    await loadState();
    await loadFees();
  } catch (e) {
    $("adminLoginErr").textContent = e.message;
  } finally {
    $("btnAdminLogin").disabled = false;
  }
}

async function logout(msg) {
  try { await api("/api/auth/logout", {}); } catch (e) { /* 会话已失效也继续收敛到登录层 */ }
  authSession = "";
  showLogin(msg || "已退出登录");
}

// ensureSession 重新探测登录态：未登录弹登录层，否则加载面板数据。
// 用于密码变更/清空后重新同步（改密码会作废旧会话，清空密码则鉴权消失）。
async function ensureSession(msg) {
  try {
    const st = await api("/api/auth/state");
    if (st.auth_enabled && !st.auth_session) { showLogin(msg); return; }
    authSession = st.auth_session || "";
  } catch (e) { showLogin(msg); return; }
  await loadState();
}

// ---------- 工具 ----------
function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

let toastTimer = null;
function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), 3000);
}

function shortUid(uid) {
  if (!uid) return "";
  return uid.length <= 12 ? uid : uid.slice(0, 6) + "…" + uid.slice(-4);
}

// fmtDateTime24 时间戳 → "MM-DD HH:MM"（强制 24 小时制，两位补零）。
// 不用 toLocaleString：它随浏览器 locale/ICU 走，某些环境会渲染成
// 12 小时制（7:07 PM）或「上午/下午」前缀，与全站 24h 制口径不一致。
function fmtDateTime24(ts) {
  const d = new Date(ts * 1000);
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// fmtLeft 解冻时刻（unix 秒）→ 「剩 X 时 X 分」；已过期返回「即将解冻」。
// 冷却跨零点时 24h 制绝对时刻（如 10-18 07:07 = 次日清晨）极易被误读成
// 「19 点被 12 小时制截成 7 点」，并列相对时长后不再歧义。
function fmtLeft(untilTs) {
  const sec = untilTs - Date.now() / 1000;
  if (sec <= 0) return "即将解冻";
  const m = Math.floor(sec / 60);
  if (m < 60) return `剩 ${m} 分`;
  return `剩 ${Math.floor(m / 60)} 时 ${String(m % 60).padStart(2, "0")} 分`;
}

// shortKey 顶栏 API-Key 中间省略（同 shortUid 风格），完整值存 data-key 供复制。
function shortKey(k) {
  return k.length <= 24 ? k : k.slice(0, 6) + "…" + k.slice(-4);
}
