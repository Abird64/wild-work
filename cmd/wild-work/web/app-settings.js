"use strict";
// ---------- 设置弹层（统一配置：监听/API-Key/签到/自启/模型路由/备用上游通道） ----------
// renderBackupProxy 回显全局备用上游通道（单一地址，空 = 关闭）。
// 定位是**备份链路**而不是全局代理：不参与常规请求，仅渠道命中「上游地区拒绝」
// 时在同一次请求内自动取用（决议 R57），故文案里不说「走代理」而说「重发」。
function renderBackupProxy() {
  const inp = $("backupProxyInput");
  inp.value = state.backup_proxy || "";
  inp.placeholder = "如 socks5://127.0.0.1:1080 或 http://127.0.0.1:7890（留空 = 关闭）";
  // 生效位：后端同时回 backup_proxy（配置）与 backup_proxy_active（运行时已装载）。
  // 两者不等 = 配置写了但进程内未生效（地址不合法/启动失败），必须让用户看见。
  const s = $("backupProxyState");
  s.textContent = !state.backup_proxy ? ""
    : (state.backup_proxy_active ? "● 已生效" : "未生效（地址无效或启动失败）");
  s.style.color = state.backup_proxy && !state.backup_proxy_active ? "var(--danger)" : (state.backup_proxy_active ? "var(--ok)" : "var(--muted)");
  $("proxyErr").textContent = "";
}

// selectedHost 当前下拉框（+自定义输入）选定的监听主机名。
// 与后端 config.Listen 归一化口径保持一致：空/`::` 等通配写法按「全部网卡」看待（即对外暴露）。
function selectedHost() {
  const v = $("selHost").value;
  if (v !== "__custom__") return v;
  return $("inHost").value.trim();
}

// isLoopbackHost 判定是否「仅本机可访问」：环回 IP / localhost。
// 对应后端 config.Listen.IsLoopback：**显式写环回才算安全**，空主机名视为对外暴露。
function isLoopbackHost(h) {
  const s = (h || "").trim().toLowerCase().replace(/^\[|\]$/g, "");
  if (s === "localhost") return true;
  return s === "127.0.0.1" || s === "::1" || /^127\./.test(s);
}

// syncListenRisk 按当前选定的监听地址，动态显示/隐藏黄色警告条与管理密码的必填星号。
// 默认 127.0.0.1（本机）时两者均不显示——本机监听无需密码，提示只会干扰。
function syncListenRisk() {
  const needPass = !isLoopbackHost(selectedHost());
  $("listenRiskTip").classList.toggle("hidden", !needPass);
  $("adminPassReq").classList.toggle("hidden", !needPass);
}

async function openSettings() {
  // 弹层互斥：打开设置时收起「查找模型」（#134）
  $("modelSearchOverlay").classList.add("hidden");
  // 监听
  $("inPort").value = state.listen_port;
  $("selHost").value = state.listen_host === "127.0.0.1" ? "127.0.0.1"
    : (state.listen_host === "0.0.0.0" || state.listen_host === "" || state.listen_host === "::") ? "0.0.0.0"
    : "__custom__";
  if ($("selHost").value === "__custom__") {
    $("inHost").value = state.listen_host;
    $("customHostRow").classList.remove("hidden");
  } else {
    $("customHostRow").classList.add("hidden");
  }
  // API-Key
  $("keyInput").value = state.api_key;
  // 管理密码：后端不回显，只提示是否已设置（留空 = 不改动，与 oczen key 同一「未改动」约定）
  $("adminPassInput").value = "";
  $("adminPassInput").placeholder = state.admin_pass_set
    ? "已设置（留空 = 不改动；填入新值 = 修改）"
    : (state.auth_required ? "必填：监听非 127.0.0.1 时面板必须鉴权" : "留空 = 面板不鉴权（仅本机监听时允许）");
  // 模型路由
  const cc = state.compat || {};
  const channels = cc.channels || [];
  $("selCh").innerHTML = channels.map(c => `<option value="${c}">${c}</option>`).join("");
  $("selCh").value = cc.default_channel || (channels[0] || "");
  $("inMaxTok").value = cc.max_tokens_cap || 0;
  const map = cc.model_map || {};
  const entries = Object.entries(map);
  $("mapSummary").textContent = entries.length === 0 ? "（空）" : entries.map(([k,v]) => `${k} → ${v}`).join("\u00A0 \u00A0");
  // 映射编辑器：随对话框打开而重置为当前值，收起
  const mapTextVal = entries.map(([k,v]) => `${k} = ${v}`).join("\n");
  $("mapText").value = mapTextVal;
  $("mapText").dataset.baseline = mapTextVal; // 供 syncSceneSideViews 判断"用户有没有动过"
  $("mapEditor").classList.add("hidden");
  $("mapErr").textContent = "";
  renderMapPresets(channels);
  // 备用上游通道 + oczen 自定义 key
  renderBackupProxy();
  $("oczenKeyInput").value = state.oczen_api_key || ""; // 回显脱敏值；未改动则原样回传，后端按脱敏值识别为「未变」
  $("oczenKeyInput").dataset.touched = "";
  // 模型降级链
  if (!lastFees) loadFees();
  fbRules = JSON.parse(JSON.stringify(state.model_fallback || []));
  renderFbList();
  $("fbFromInput").value = ""; $("fbToInput").value = "";
  // 思考强度注入（`*` 全局默认单独回显到「全局默认」行）
  const effs = state.reasoning_efforts || {};
  effRules = Object.entries(effs).filter(([k]) => k !== "*").map(([model, effort]) => ({ model, effort }));
  $("effGlobalInput").value = effs["*"] || "";
  renderEffList();
  $("effModelInput").value = ""; $("effEffortInput").value = "";
  // 自动签到 + 开机自启
  $("chkAutostart").checked = !!state.autostart;
  $("chkLoopGuard").checked = state.loop_guard !== false; // 缺省（旧后端）按开启显示
  $("chkBodyLog").checked = !!state.body_log;
  syncListenRisk(); // 按当前监听地址初始化警告条与必填星号
  // R129：设置不再是弹层，而是「其他」Tab 的一页。原先那句「移除 settingsOverlay 的 hidden」
  // 改为切到该 tab —— 表单字段的 id 一个都没变，故上面的回显逻辑完全复用。
  showMainTab("other");
  // 三场景编辑区（SR-005b）：先渲染（立刻可见），再 await 取活值（推荐列表要用新数据，SD-7）。
  // 顺序是「先可见后补齐」而不是反过来——费率接口慢时不该让整页等它。
  await openSceneEditor();
}

function closeSettings() {
  closeTimeEditor(); // 关页时不留半开的时间选择器
  closeModelSearch();
}

// ---------- 渠道编辑弹窗（R82） ----------
// 主列表只做一行展示，新增/修改渠道都走这个弹窗。
// editChIdx = -1 表示新增；>=0 表示修改 editCustomChannels[editChIdx]。
// ceKeyPlain 只活在这个弹窗开关闭期间（R122 眼睛）：明文不回写列表/配置对象，
// 配置里永远只存用户填的或后端下发的脱敏串，关窗即清。
let editChIdx = -1;
let ceKeyPlain = "";

function openChannelEditor(idx) {
  editChIdx = typeof idx === "number" ? idx : -1;
  const ch = editChIdx >= 0 ? editCustomChannels[editChIdx] : null;
  $("channelEditorTitle").textContent = ch ? "编辑渠道" : "新增渠道";
  $("ceName").value = ch ? ch.name : "";
  // R122：放开渠道标识可改（旧版直接 disabled）。改名等于换路由，代价在保存时
  // 弹确认告知，并由后端级联 config 引用 / 隔离旧 state 快照。
  $("ceName").disabled = false;
  $("ceURL").value = ch ? ch.base_url : "";
  $("ceAuth").value = ch && ch.auth === "basic" ? "basic" : "";
  syncCeKeyHint();
  $("ceKey").value = ch ? (ch.api_key || "") : "";
  $("ceKey").type = "password"; // 明文不默认展开，眼睛点一下才取
  ceKeyPlain = "";
  $("ceErr").textContent = "";
  setCeHint("");
  $("channelEditorOverlay").classList.remove("hidden");
  setTimeout(() => (ch ? $("ceURL") : $("ceName")).focus(), 50);
}

// syncCeKeyHint 按当前鉴权方式改写 API Key 输入框的占位提示。
// basic 时必须指用户去填 `app_key:app_secret`：只提示「填密钥」会得到一串被 base64 后
// 上游回 APIG.1009 的无效凭据，而那个错误文案完全看不出是「少了冒号」。
function syncCeKeyHint() {
  const basic = $("ceAuth").value === "basic";
  $("ceKey").placeholder = basic ? "app_key:app_secret" : "可选；留空表示无鉴权";
}

function closeChannelEditor() {
  $("channelEditorOverlay").classList.add("hidden");
  editChIdx = -1;
  // 明文不得留在 DOM 里等下一次开窗被误提交（R122）。
  $("ceKey").value = "";
  $("ceKey").type = "password";
  ceKeyPlain = "";
}

// toggleCeKeyEye API Key 旁的眼睛（R122：编辑态也能看明文，与「点复制取原文」同一口径）。
// 新增态框里就是用户刚打的字，直接切 type、**不发请求**；
// 编辑态框里是后端下发的脱敏串，只能那一下按需取回明文。
async function toggleCeKeyEye() {
  const inp = $("ceKey");
  if (inp.type === "text") { inp.type = "password"; setCeHint(""); return; }
  const name = editChIdx >= 0 ? (editCustomChannels[editChIdx] || {}).name : "";
  if (!isMaskedKey(inp.value)) {           // 没配 key / 刚填的明文：就地显示
    inp.type = "text";
    setCeHint("");
    return;
  }
  if (!name) return setCeHint("请先填渠道标识并保存后再取明文", true);
  if (ceKeyPlain) { inp.value = ceKeyPlain; inp.type = "text"; return; }
  setCeHint("正在取回明文…", false);
  try {
    const res = await api("/api/config/custom_channel_key", { name });
    if (!res.ok || !res.api_key) { setCeHint("该渠道未配置 api-key", true); return; }
    ceKeyPlain = res.api_key;
    inp.value = ceKeyPlain;
    inp.type = "text";
    setCeHint("已显示明文（只有这一条，不随列表下发）", false);
  } catch (e) {
    setCeHint("取明文失败：" + e.message, true);
  }
}

// stripQuotes 去掉首尾成对引号/反引号：从 markdown 代码块或 curl 示例里粘地址常见。
function stripQuotes(s) {
  s = (s || "").trim();
  const q = `'"\``;
  if (s.length > 1 && q.includes(s[0]) && s[s.length - 1] === s[0]) return s.slice(1, -1).trim();
  return s;
}

// isMaskedKey 认后端下发的脱敏形态（**** / … / ••）。它不是用户真填的密钥，
// 提交时要原样送回，后端按渠道名找回真实值（与 Go 侧 isMaskedKey 同一口径，两边得同步改）。
const isMaskedKey = (k) => !!k && (k.includes("****") || k.includes("…") || k.includes("••"));

// looksLikeEndpoint 放宽录入：允许漏协议头（后端按内/外网补 http/https）、允许带 /chat/completions
// 尾巴（后端会剥）。只拦真正的误粘：含空白、或连 host 形状都不是。
function looksLikeEndpoint(u) {
  if (/\s/.test(u)) return false;
  if (/^https?:\/\//i.test(u)) return true;
  return /^[a-z0-9.\-]+(:\d+)?(\/|$)/i.test(u);
}

function setCeHint(msg, bad) {
  const el = $("ceHint");
  if (!el) return;
  el.textContent = msg || "";
  el.style.color = bad ? "var(--danger)" : "var(--muted)";
}

// probeCustomChannel 发一次探测：后端做规范化 + 候选根嗅探 + 能力解析（R121）。
// 脱敏态的 api_key 原样发出，由后端 resolveCustomKey 按渠道名换成真值。
function probeCustomChannel(ch) {
  return api("/api/config/custom_channel_test", { name: ch.name, base_url: ch.base_url, api_key: ch.api_key });
}

// applyProbeResult 把探测结论落回 target.base_url，并写一行可行动的结论。
// 失败时必须把试过的地址逐条列出来：只说「连接失败」用户无从知道是 key 错还是路径错。
function applyProbeResult(res, target, setHint) {
  if (!res) return;
  if (res.base_url) target.base_url = res.base_url;
  const bits = [res.models_ok ? `模型目录 ${(res.models || []).length} 个` : "模型目录不可用",
    res.chat_ok ? "对话端点存在" : "对话端点未确认"];
  const tail = res.note ? `；${res.note}` : "";
  if (res.ok) {
    setHint(`✓ ${target.base_url} ｜ ${bits.join(" ｜ ")}${tail}`, false);
  } else {
    const tried = (res.tries || []).map((t) => `${t.url} → ${t.status || t.err || "?"}`).join("； ");
    setHint(`✗ ${res.error || "没能确认端点"}（${bits.join(" ｜ ")}）${tail}${tried ? ` ｜ 试过：${tried}` : ""}`, true);
  }
}

// sniffInto 探测并把确认后的根写回 target（调用方负责把 target.base_url 同步回表单/列表）。
async function sniffInto(target, setHint) {
  if (!target.base_url) {
    setHint("请先填 Base URL", true);
    return null;
  }
  setHint("正在规范化并向上游确认端点…", false);
  try {
    const res = await probeCustomChannel(target);
    applyProbeResult(res, target, setHint);
    return res;
  } catch (e) {
    setHint("识别失败：" + e.message, true);
    return null;
  }
}

// sniffFromEditor 渠道编辑器里的「🔍 识别端点」：把确认后的根回填进输入框（所见即所存）。
async function sniffFromEditor() {
  const t = { name: $("ceName").value.trim().toLowerCase(), base_url: stripQuotes($("ceURL").value), api_key: $("ceKey").value.trim(), auth: $("ceAuth").value };
  const res = await sniffInto(t, setCeHint);
  if (res && t.base_url) $("ceURL").value = t.base_url;
  return res;
}

function saveChannelEditor() {
  const name = $("ceName").value.trim().toLowerCase();
  const url = stripQuotes($("ceURL").value);
  const key = ceKeyPlain || $("ceKey").value.trim(); // 眼睛取回的明文以它为准（R122）
  const auth = $("ceAuth").value === "basic" ? "basic" : "";
  const fail = (m) => { $("ceErr").textContent = m; };
  if (!name) return fail("请填入渠道标识");
  if (!/^[a-zA-Z0-9_\-]+$/.test(name)) return fail("渠道标识仅允许字母、数字、下划线、短横线");
  if (RESERVED_CHANNEL_NAMES.has(name)) return fail(`渠道标识 ${name} 与内建渠道同名，请另取一个`);
  if (!url) return fail("请填入 Base URL");
  // 不再硬性要求 https:// 开头（R121）：本地 one-api 常常是 http://127.0.0.1:11434/v1，
  // 用户从文档里复制的又多是带尾巴的完整地址，交给后端规范化比在这里拦人更合适。
  if (!looksLikeEndpoint(url)) return fail("Base URL 不像合法地址（形如 api.example.com/v1 或 https://…）");
  // basic 形态的 key 是 app_key:app_secret：前端的即时校验与后端 Validate 同一口径，
  // 免得用户点「保存」才被后端回一句错误（后端仍会再校验一次，两边不能只靠一边）。
  if (auth === "basic" && key && !key.includes(":")) return fail("鉴权方式为 Basic 时 API Key 需填 app_key:app_secret（缺少冒号）");
  if (editChIdx >= 0) {
    const ch = editCustomChannels[editChIdx];
    if (name !== ch.name && editCustomChannels.some((c) => c.name.toLowerCase() === name)) return fail(`已存在同名渠道 ${name}`);
    const changed = ch.base_url !== url || (ch.api_key || "") !== key || (ch.auth || "") !== auth;
    const renamed = name !== ch.name;
    // 改名要用户自己知情：历史流水留在旧名、旧前缀请求立即 400（本工具不做 alias）。
    if (renamed && !confirm(`把渠道标识 ${ch.name} 改为 ${name}？\n\n· 以 ${ch.name}/ 开头的模型 ID 需改成 ${name}/…（旧前缀会报 unknown channel）\n· 历史用量/积分流水仍记在 ${ch.name} 下，不会跟着改`)) return;
    if (renamed) pendingRenames.push({ new: name, old: ch.name });
    Object.assign(ch, { name, base_url: url, api_key: key });
    if (auth) ch.auth = auth; else delete ch.auth;
    closeChannelEditor();
    renderCustomChList();
    $("customChOk").textContent = renamed ? `已改名为 ${name}，点「保存全部配置」生效` : "已更新，点击「保存全部配置」生效";
    $("customChErr").textContent = "";
    // 改动过接入点就顺手确认一次（异步，不拦保存）：确认到的根会就地回写。
    if (changed || renamed) sniffInto(ch, (m, bad) => { $("customChErr").textContent = bad ? m : ""; if (!bad) { $("customChOk").textContent = m; renderCustomChList(); } });
    return;
  }
  if (editCustomChannels.some((c) => c.name.toLowerCase() === name)) return fail(`已存在同名渠道 ${name}`);
  // enabled 字段后端已废弃（R83 移除「停用」概念），不再写这个死字段。
  const idx = editCustomChannels.length;
  const fresh = { name, base_url: url, api_key: key, models: [] };
  if (auth) fresh.auth = auth; // 只在 basic 时落字段：bearer 是默认，不往配置里塞无效噪音
  editCustomChannels.push(fresh);
  closeChannelEditor();
  renderCustomChList();
  $("customChErr").textContent = "";
  // 需求②：Provider 一行一key 填完就自动拉一次 /models 并弹勾选列表，
  // 不让用户再「先保存→回列表→点导入」三跳。拉失败也不拦保存（只提一句）。
  $("customChOk").textContent = "已添加，正在规范化并确认端点、拉取模型列表…";
  openModelPick(idx, "api");
}
