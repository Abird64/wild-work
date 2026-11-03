"use strict";
// ---------- 渲染 ----------
function render() {
  renderTopbar();
  renderSummary();
  renderAccounts();
  applyAcctFilter();
  refreshTabBalances();
  renderTimes();
}

// renderTopbar 顶栏 API 地址渲染：
// - 监听 127.0.0.1/localhost → 原样显示；
// - 监听 0.0.0.0/::/空（所有网卡）→ 显示局域网 IP（lan_ip），客户端可跨机接入；
//   取不到局域网 IP 时兑底 127.0.0.1；
// - 其他自定义地址 → 原样显示。
// 此前 0.0.0.0 被硬换成 127.0.0.1，局域网客户端复制到的是本机环回地址，无法跨机接入。
function renderTopbar() {
  $("ver").textContent = "v" + state.version;
  $("serverLine").textContent = state.running ? "服务运行中" : "服务未启动";
  $("aboutVer").textContent = state.version;

  const wildcard = state.listen_host === "0.0.0.0" || state.listen_host === "" || state.listen_host === "::";
  const host = wildcard ? (state.lan_ip || "127.0.0.1")
    : (state.listen_host === "localhost" ? "127.0.0.1" : state.listen_host);
  const apiURL = `http://${host}:${state.listen_port}/v1`;
  $("apiAddr").querySelector(".val").textContent = apiURL;
  // 监听所有网卡时提示真实含义（避免误导为仅本机可用）
  $("apiAddr").title = wildcard
    ? `监听 0.0.0.0（所有网卡），局域网可用；点击复制`
    : "点击复制地址";

  const key = state.api_key;
  const keyEl = $("apiKeyDisplay");
  const keyVal = keyEl.querySelector(".val");
  if (key === "") {
    keyVal.textContent = "（无鉴权）";
    delete keyEl.dataset.key;
    keyEl.title = "当前未设置 API-Key";
  } else {
    // 脱敏显示：中间省略，完整值存 data-key 供一键复制（防旁人瞟屏）
    keyEl.dataset.key = key;
    keyVal.textContent = shortKey(key);
    keyEl.title = "完整 API-Key：" + key + "；点击复制；进入设置修改";
  }
}

// 渠道显示名与 CSS 短类名（后端 group / 费率 channel 均为 provider.Kind）。
const CH_LABEL = { workbuddy: "WorkBuddyCN", workbuddyai: "WorkBuddyAI", traework: "TraeWork", traecode: "TraeCode", qoder: "Qoder", qodercn: "QoderCN", qodercom: "QoderCOM", qwenwork: "千问办公", glm: "智谱清言", monkeycode: "MonkeyCode", raccoon: "小浣熊", loomy: "Loomy", oczen: "OpenCodeZen", kilo: "Kilo", minimax: "MiniMax", lobsterai: "LobsterAI", zcode: "ZCode", gemini: "Gemini", codearts: "CodeArts", cline: "Cline", officeace: "OfficeAce", doubao: "豆包", doubaowork: "豆包工作" };
const CH_CLASS = { workbuddy: "wb", workbuddyai: "wbai", traework: "trae", traecode: "traecode", qoder: "qoder", qodercn: "qodercn", qodercom: "qodercom", qwenwork: "qwenwork", glm: "glm", monkeycode: "monkeycode", raccoon: "raccoon", loomy: "loomy", oczen: "oczen", kilo: "kilo", minimax: "minimax", lobsterai: "lobsterai", zcode: "zcode", gemini: "gemini", codearts: "codearts", cline: "cline", officeace: "officeace", doubao: "doubao", doubaowork: "doubaowork" };
const chLabel = (k) => CH_LABEL[k] || k;
const chClass = (k) => CH_CLASS[k] || "custom";
// ---------- 渠道配色：单一色（R99 v3，2026-10-29 用户裁决）----------
// 改动前：23 个 --ch-<类名> 品牌色，外加自定义渠道按序号取色相、逐级压暗保证白字的
// 自适应配色函数（合计约 60 行机制代码）。两者均已删除。
// 为什么删：① 渠道身份由 chLabel() 的文字承载，色相是多余的第二编码；② 暗色底上
// 23 个色相必然拥挤（原浅色板搬到 #131118，23 个里 21 个低于 4.5:1）；③ 维护成本——
// 新增渠道曾要求同时补 --ch-<类名> + .badge.x + .btn.primary.alt.x 三处，漏一处就静默回退。
// 现在所有渠道色一律取 --ch（CSS 里的单一变量），自定义渠道也用同一色。
// 渠道短类名（CH_CLASS）**保留**：HTML 上仍拼 `class="badge wb"`，applyAcctFilter 靠
// data-group 筛选，其它样式规则也仍按类名命中。
// rampColor(i) 取明度梯第 i 档（0 起，超出封顶到第 6 档）。
// 单色方案里图表靠「明度排名」而不是「色相」区分系列，故与 --ch 是两套变量。
const RAMP_FALLBACK = ["#d4c8fe", "#bda7fe", "#a688f4", "#8e6fd8", "#7757be", "#613fa3"];
const _rampCache = [];
function rampColor(i) {
  const k = Math.min(Math.max(i, 0), 5);
  if (_rampCache[k]) return _rampCache[k];
  let v = "";
  try { v = getComputedStyle(document.documentElement).getPropertyValue(`--ch-ramp-${k + 1}`).trim(); } catch (e) { /* ignore */ }
  return (_rampCache[k] = v || RAMP_FALLBACK[k]);
}

const _chColorCache = {};
function chColor() {
  if (_chColorCache.single) return _chColorCache.single;
  let v = "";
  try { v = getComputedStyle(document.documentElement).getPropertyValue("--ch").trim(); } catch (e) { /* ignore */ }
  return (_chColorCache.single = v || "#a098be");
}

// 渠道徽章的唯一渲染入口（R99）：全仓只此一处拼 badge。
// 底色/圆点色一律来自 CSS 的 --ch（不再内联 background，因为所有渠道同色）；
// 只保留渠道类名，供筛选与后续样式命中。
function chBadge(k) {
  return `<span class="badge ${chClass(k)}">${esc(chLabel(k))}</span>`;
}
// 不支持显式签到（手动按钮）的渠道：
// WorkBuddy 国际版不提供手动签到，而是自动对话保活领日活奖励；
// 千问办公无签到活动；OpenCodeZen 与 Kilo 是免密渠道，
// 无账号概念（也无积分）。
// 小浣熊已实现签到（setting_info 触发每日积分落账，见 internal/raccoon/checkin.go），支持手动按钮。
// MonkeyCode 已实现签到（POST {console}/api/v1/users/wallet/checkin，见 internal/monkeycode/checkin.go），
// 故**不在**本名单内：它要有手动按钮，也要参与批量签到。
// Loomy 同样已实现签到（宠物每日打工奖励，见 internal/loomy/checkin.go），故也在名单外。
// 注意：Loomy 的奖励判据是「当日有效对话轮次」（上游算），未在官方客户端用过该账号时
// 领取结果会是「未达档位」——那是如实状态，不是按钮坏了。
const NO_EXPLICIT_CHECKIN = new Set(["workbuddyai", "qwenwork", "oczen", "kilo", "zcode", "gemini", "cline"]);
const noExplicitCheckin = (g) => NO_EXPLICIT_CHECKIN.has(g) || isCustomGroup(g); // 自定义渠道无签到活动（R66）
// isCustomGroup 该分组是否为用户自定义 OpenAI 兼容渠道（R66）。
// 以 state.custom_channels（后端 /api/state 下发，保存即热更新）为准，勿在前端另维护一份。
const isCustomGroup = (g) => !!(state && (state.custom_channels || []).some((c) => c && c.name === g));
// hasNoCredits 该分组是否无积分概念：内建 NA 名单 ∪ 动态自定义渠道。与后端 creditsNAKind 同口径。
const hasNoCredits = (g) => NO_CREDITS_BUILTIN.has(g) || isCustomGroup(g);
// 导入型渠道：凭据由本机已登录的官方客户端提供，没有浏览器登录流程（见 internal/app/import_local.go）。
const IMPORT_LOCAL_CHANNELS = new Set(["monkeycode", "raccoon", "loomy", "officeace"]);
const isImportLocal = (ch) => IMPORT_LOCAL_CHANNELS.has(ch);
// 支持「网页版自动登录」的渠道：后端拉起独立 profile 的浏览器，用户在网页里登录，
// 后端经 CDP 捕获凭据（小浣熊：Cookie 里的 xhx_token）。
// 小浣熊两个集合都命中 —— 弹窗里同时给「网页版登录」与「从客户端导入」两个动作。
// Loomy 也两个集合都命中，但机制不同：**不**拉浏览器、**不**用 CDP 捕获，而是起一个
// 127.0.0.1 的本机登录引导页（手机号 + 短信验证码），由前端 window.open 打开
// （见 internal/login_loomy）。故文案需按渠道区分，不能套用小浣熊那段。
const WEB_LOGIN_CHANNELS = new Set(["raccoon", "loomy"]);
const hasWebLogin = (ch) => WEB_LOGIN_CHANNELS.has(ch);
// 粘贴凭据型渠道：没有浏览器登录编排，凭据由用户从官方站点自行创建后粘贴进来。
// Cline：app.cline.bot → 注册账号 → Account / API Keys 建 sk_ Key（见 internal/cline）。
const PASTE_KEY_CHANNELS = new Set(["cline"]);
const isPasteKey = (ch) => PASTE_KEY_CHANNELS.has(ch);
// 无手动签到渠道的状态文案：每个在 NO_EXPLICIT_CHECKIN 里的渠道都要有一条
// （漏写就落兜底的「无签到」，把「每日积分自动发放」说成「不支持」）。
const NO_CHECKIN_TAG = {
  workbuddyai: "自动领日活奖励",
  qwenwork: "每日自动发放", // 积分由服务端 00:00 被动发放，无领取动作
  oczen: "不支持",
  kilo: "不支持",
  zcode: "不支持",
  gemini: "不支持",
  cline: "无签到",
};
const noCheckinText = (g) => NO_CHECKIN_TAG[g] || "无签到";
// 无积分概念的渠道（匿名通道）：积分区域显示「不适用」，并隐藏刷新积分/明细入口。
// 内建名单只有 oczen/kilo/cline；自定义渠道（R66）同样无积分，但渠道名是用户配置的、
// 运行期可增删，故运行时以后端下发的 custom_channels 动态判定（isCustomGroup）。
const NO_CREDITS_BUILTIN = new Set(["oczen", "kilo", "cline"]);
// 支持跳转登录编排的渠道：登录态失效时卡片显示 ⟳ 重新登录入口（#134）。
// officeace 一并纳入（2026-10-17）：它除了「登录态失效」，还有「缺 refresh_token」
// 这种**不能自愈**的形态（客户端导入只拿得到不过期的网关 Basic），此时同样只能重新登录
// 才能恢复积分/签到 —— 徽标与 ⟳ 入口由同一处逻辑驱动（见 renderAccounts 的 needRelogin）。
const RELOGIN_CHANNELS = new Set(["workbuddy", "workbuddyai", "traework", "qoder", "qodercn", "qodercom", "qwenwork", "raccoon", "officeace"]);

// creditsText 账号卡片的积分文案。
// 拆成「可用 / 不可用 / 临期」三个数字：渠道（如 TraeWork）会下发官方客户端专用的
// 额度池，对本工具是看得见用不了的，混进一个数字会让人误判可用余额；
// 临期是可消耗余额中 24h 内（到期日≤明天）到期的部分，提示优先消耗。
// 不可用仅在 >0 时显示；临期同理，凭空多个灰/红 0 很吵。
// 旧版本 state 文件（v2.2.0 及之前，无 unusable 字段）读入后 credits_stale=true，
// 此时不把旧值当真值，改显示「待刷新」；自动刷新首刷成功后即变回真实拆分。
// fmtTokens Token 数格式化：这类数字动辄上亿，直接铺开会撑破卡片与汇总条，
// 故用中文紧凑单位（万/亿）显示；完整语义靠 title 说明。
function fmtTokens(n) {
  n = Number(n) || 0;
  const trim = (s) => s.replace(/\.?0+$/, "");
  if (Math.abs(n) >= 1e8) return trim((n / 1e8).toFixed(2)) + "亿";
  if (Math.abs(n) >= 1e4) return trim((n / 1e4).toFixed(2)) + "万";
  return n.toLocaleString("zh-CN");
}

// shortWindowName 把上游窗口名压成面板能放下的短标签。
//
// 上游原名带「（%）」后缀（如「5 小时窗口（%）」），那是计量单位说明，
// 放进卡片既长又重复（旁边已经有 % 号）。这里只去掉后缀，不做其它改写 ——
// 窗口叫什么由上游定，前端不该另起一套名字。
function shortWindowName(name) {
  // 除了「（%）」后缀，豆包那侧还会带「 (type=1)」这类协议内部标注 ——
  // 那是上游窗口编号，对用户没有意义，一律剥掉（窗口叫什么由上游定，前端只做收纳）。
  return String(name || "")
    .replace(/（%）/g, "")
    .replace(/\s*\(type=\d+\)\s*$/i, "")
    .replace(/#/g, "")
    .trim();
}

function creditsText(a) {
  if (a.credits_na) {
    return `<span class="credit-na" title="该渠道无积分概念">不适用</span>`;
  }
  if (a.credits_stale) {
    return `<span class="credit-stale" title="余额口径已过期（旧版本状态文件），正在自动刷新…">待刷新</span>`;
  }
  // Token 计量渠道（credits_unit="token"，如 ZCode）：同一位置改显示 **Token 计数**。
  // 与积分分开统计——它**不进**积分汇总合计（见 renderSummary），
  // 但保留临期角标（token 桶同样有到期时间，来自内部 ResourceItem.ExpireAt）。
  // 百分比配额渠道（credits_unit="percent"，如 Gemini）：额度是「窗口剩余百分比」，
  // 不存在"可用积分"。旧实现落到默认分支显示「0 可用积分」——额度明明满格却像已耗尽
  // （2026-10-30 用户实测）。这里改成逐窗口展示，窗口名与重置时刻都由后端下发。
  if (a.credits_unit === "percent") {
    const ws = a.quota_windows || [];
    if (!ws.length) {
      // 还没刷过（或刷新失败）：如实说"待刷新"，不编造 0%。
      return `<span class="credit-stale" title="百分比配额尚未刷新，下次自动刷新后显示">待刷新</span>`;
    }
    return ws.map((w) => {
      const pct = Number(w.percent) || 0;
      const cls = pct <= 10 ? "credit-expiring" : "credit-pct";
      const tip = w.reset_at ? `${w.name}剩余 ${pct}%，${w.reset_at.replace("T", " ").slice(0, 16)} 重置`
        : `${w.name}剩余 ${pct}%`;
      return `<span class="${cls}" title="${esc(tip)}">${esc(shortWindowName(w.name))} ${pct}%</span>`;
    }).join(`<span class="credit-wsep"> · </span>`);
  }
  if (a.credits_unit === "token") {
    let html = `<span class="credit-num">${fmtTokens(a.credits)}</span>`
      + `<span title="本渠道额度以 token 计量，与积分分开统计">tokens</span>`;
    if ((a.expiring_credits || 0) > 0) {
      html += `<span class="credit-expiring"> (临期${fmtTokens(a.expiring_credits)})</span>`;
    }
    return html;
  }
  let html = `<span class="credit-num">${a.credits}</span><span class="credit-unit">可用积分</span>`;
  if ((a.expiring_credits || 0) > 0) {
    html += `<span class="credit-expiring"> (临期${a.expiring_credits})</span>`;
  }
  if ((a.unusable_credits || 0) > 0) {
    html += `<span class="credit-unusable"> (不可用${a.unusable_credits})</span>`;
  }
  return html;
}

// 渠道汇总固定顺序：与后端 feeChannelOrder（internal/app/app.go）同序，
// 保持「账号卡片 → 汇总条 → 费率 tab」三处看到同一个渠道顺序。
// ⚠️ 新增渠道必须同步此表：漏项会被 renderSummary 按字母序垫到最末
//   （ui-static-check 有覆盖度断言）。qoder 已下线，仅为存量账号兜底留位。
const CH_SUMMARY_ORDER = ["oczen", "kilo", "workbuddy", "workbuddyai", "qodercn", "qodercom", "traework", "traecode", "qwenwork", "glm", "cline", "monkeycode", "raccoon", "loomy", "minimax", "lobsterai", "zcode", "gemini", "codearts", "officeace", "doubao", "doubaowork", "qoder"];

// renderSummary 渠道积分汇总条：每渠道一行「可用总积分 / 临期」，末尾合计。
// 数据全部来自 state.accounts（pool 实时值），与账号卡片同源；
// 口径与 R17 一致：不可用额度只作角标、不进合计；credits_na（匿名）不参与合计。
function renderSummary() {
  const el = $("creditSummary");
  if (!el) return;
  if (!state.accounts.length) { el.classList.add("hidden"); return; }
  el.classList.remove("hidden");

  const byCh = {};
  for (const a of state.accounts) {
    const g = a.group || "workbuddy";
    let c = byCh[g];
    if (!c) { c = byCh[g] = { n: 0, credits: 0, expiring: 0, unusable: 0, stale: 0, na: false, isToken: false, tokens: 0 }; }
    c.n++;
    if (a.credits_na) { c.na = true; continue; }
    // Token 计量渠道单列：只累加 token 数，**不并入**积分合计（与积分分开统计）。
    if (a.credits_unit === "token") { c.isToken = true; c.tokens += a.credits || 0; c.expiring += a.expiring_credits || 0; continue; }
    c.credits += a.credits || 0;
    c.expiring += a.expiring_credits || 0;
    c.unusable += a.unusable_credits || 0;
    if (a.credits_stale) c.stale++;
  }

  const fmt = (n) => n.toLocaleString("zh-CN");
  // 已知渠道按固定顺序，未知渠道（新增时）按字母序垫后，保证不丢行
  let chs = CH_SUMMARY_ORDER.filter((k) => byCh[k])
    .concat(Object.keys(byCh).filter((k) => !CH_SUMMARY_ORDER.includes(k)).sort());
  // 「不适用」渠道（无积分概念：oczen/kilo/cline/自定义渠道）默认不进汇总条：
  // 它们的 chip 只有一句「不适用」，既不反映余额也不参与合计，纯噪声。
  // 收起时改由末尾一枚开关 chip 汇总提示（点它原地展开），账号卡片同步收起。
  const naChs = chs.filter((k) => byCh[k].na);
  const naAccts = naChs.reduce((n, k) => n + byCh[k].n, 0);
  if (!showNoCredit) chs = chs.filter((k) => !byCh[k].na);

  let total = 0, totalExp = 0, staleChs = 0;
  let html = `<span class="cs-title">积分汇总</span>`;
  for (const k of chs) {
    const c = byCh[k];
    const allStale = c.stale === c.n;
    // R17 口径：口径过期的数字不当真值——全渠道待刷新时不进合计
    // （否则"待刷新"的旧值会悄悄抬高合计，用户看到的数字无从解释）。
    // Token 计量渠道不进积分合计（这就是「和积分分开统计」的落点）。
    if (!c.na && !c.isToken && !allStale) { total += c.credits; totalExp += c.expiring; }
    if (!c.na && !c.isToken && allStale) staleChs++;
    let body;
    if (c.na) {
      body = `<span class="credit-na">不适用</span>`;
    } else if (c.isToken) {
      // Token 计数：以 tokens 为单位单列，明示它不是积分。
      body = `<span class="cs-num">${fmtTokens(c.tokens)}</span>`
        + `<span class="cs-tok" title="本渠道额度以 token 计量，不计入「合计」">tokens</span>`
        + (c.expiring > 0 ? `<span class="cs-exp" title="今日到期（次日 00:00 失效算今日），选号时优先消耗">临期${fmtTokens(c.expiring)}</span>` : "");
    } else if (allStale) {
      body = `<span class="credit-stale">待刷新</span>`;
    } else {
      body = `<span class="cs-num">${fmt(c.credits)}</span>`
        + (c.expiring > 0 ? `<span class="cs-exp" title="今日到期（次日 00:00 失效算今日），选号时优先消耗">临期${fmt(c.expiring)}</span>` : "")
        + (c.unusable > 0 ? `<span class="cs-unu" title="账号名下、本工具不可消耗的额度（不计入合计）">不可用${fmt(c.unusable)}</span>` : "")
        + (c.stale > 0 ? `<span class="cs-stale" title="部分账号余额待刷新">${c.stale} 待刷新</span>` : "");
    }
    const on = acctFilter === k ? " on" : "";
    html += `<span class="cs-chip clickable${on}" data-wg="${esc(k)}" role="button" tabindex="0"`
      + ` title="点击筛选该渠道账号，再次点击取消">${chBadge(k)}`
      + `<span class="cs-n">${c.n}号</span>${body}</span>`;
  }
  // 「全部」重置项：仅在有筛选时出现，点击清除筛选。整条不参与合计，故放在合计 chip 之前。
  if (acctFilter) {
    html += `<span class="cs-chip clickable cs-all" data-wg="" role="button" tabindex="0" title="显示全部账号">`
      + `<span class="cs-n">全部</span></span>`;
  }
  // 「不适用」开关：默认收起（只留这一枚），展开后上面会重新出现这些渠道的 chip。
  // 入口刻意放在被隐藏 chip 的原位，不额外占工具栏一格（R103）。
  if (naChs.length) {
    const names = naChs.map(chLabel).join("、");
    const naTitle = showNoCredit
      ? `收起「不适用」渠道（${names}）及其账号卡片`
      : `展开「不适用」渠道（${names}）：这些渠道无积分概念，余额恒为「不适用」`;
    html += `<span class="cs-chip clickable cs-na${showNoCredit ? " on" : ""}" data-na-toggle="1"`
      + ` role="button" tabindex="0" title="${esc(naTitle)}">`
      + `<span class="cs-n">不适用</span><span class="cs-num">${naAccts}号</span>`
      + `<span class="cs-n">${showNoCredit ? "收起" : "展开"}</span></span>`;
  }
  html += `<span class="cs-chip cs-total" title="各渠道可用积分合计（不含「不可用」、匿名通道与 Token 计量渠道）">`
    + `<span class="cs-title">合计</span><span class="cs-num">${fmt(total)}</span>`
    + (totalExp > 0 ? `<span class="cs-exp">临期${fmt(totalExp)}</span>` : "")
    + (staleChs > 0 ? `<span class="cs-stale" title="有渠道整体待刷新，未计入合计">${staleChs} 渠道待刷新</span>` : "")
    + `</span>`;
  el.innerHTML = html;
}

// acctFilter 账号筛选状态：null = 全部；否则为渠道 key（provider.Kind）。
// 汇总条 chip 与账号卡片同源于 state.accounts，点某个渠道 chip 即筛选其账号卡片。
let acctFilter = null;

// showNoCredit 「不适用」账号（无积分概念的渠道：oczen/kilo/cline/自定义渠道）是否展开。
// **默认收起**：这些卡片永远只有一句「不适用」，却各占一整张卡片位置、汇总条上还各占一枚
// chip，页面上真正有余额的渠道反被稀释（2026-10-19 用户要求「不适用的默认隐藏掉，
// 但有时候又得看一下」）。开关放在汇总条末尾的「不适用 N号」chip 上——被隐藏的正是那几枚
// chip，展开入口就在原地，不额外占工具栏。选择持久化到 localStorage，刷新不弹回默认。
const SHOW_NA_KEY = "acct-show-na";
let showNoCredit = false;
try { showNoCredit = localStorage.getItem(SHOW_NA_KEY) === "1"; } catch (e) { /* 存储不可用时保持默认收起 */ }
function persistShowNoCredit() {
  try { localStorage.setItem(SHOW_NA_KEY, showNoCredit ? "1" : "0"); } catch (e) { /* 忽略 */ }
}
// isNoCreditAcct 该账号是否「无积分概念」：以后端下发的 credits_na 为准（与汇总条同口径，
// 含 R66 的自定义渠道动态判定），不在前端另维护一份渠道名单。
const isNoCreditAcct = (a) => Boolean(a && a.credits_na);

// toggleNoCredit 展开/收起「不适用」渠道与账号卡片。
function toggleNoCredit() {
  showNoCredit = !showNoCredit;
  persistShowNoCredit();
  // 收起时若正筛选着某个「不适用」渠道，必须先清筛选：否则筛选态指向已隐藏的渠道，
  // 网格一片空白，而用来取消筛选的「全部」chip 也随汇总条一起消失了。
  if (!showNoCredit && acctFilter
      && (state.accounts || []).some((a) => isNoCreditAcct(a) && (a.group || "workbuddy") === acctFilter)) {
    acctFilter = null;
  }
  // 重渲染账号卡片而不只改 class：hidden 的直接来源是 renderAccounts 里的 naHiddenClass，
  // 只靠 applyAcctFilter 改 class 时，任何一次 render() 都会把收起态“覆盖回去”（两处各写一份
  // 可见性就是漂移源）。重渲染卡片的代价很小，换来单一真相。
  renderSummary();
  renderAccounts();
  applyAcctFilter();
}

// applyAcctFilter 按 acctFilter 显示/隐藏账号卡片（与汇总条 chip 联动）。
// 逐张卡片读 data-group（由 renderAccounts 写入的**渠道 key**，不是 CSS 短类名）：
//   自定义渠道的短类名一律是 custom，拿类名反查会把它们全归到同一个 key 上，
//   而“点某渠道 chip 只留该渠道”本来就不能靠类名区分（R99）。
function applyAcctFilter() {
  const grid = $("acctList");
  if (!grid) return;
  let shown = 0;
  grid.querySelectorAll(".acct-card").forEach((card) => {
    const key = card.dataset.group || "";
    // 两道独立过滤，取「与」：① 渠道 chip 筛选；② 收起「不适用」账号。
    // 必须同时判——筛选到某渠道时也不该把它被收起的「不适用」账号漏出来。
    const naHidden = !showNoCredit && card.dataset.na === "1";
    const show = !naHidden && (!acctFilter || key === acctFilter);
    card.classList.toggle("hidden", !show);
    if (show) shown++;
  });
  // 全部卡片都被收起时给出可执行的提示：留一片空白会被当成「账号丢了」。
  const tip = $("acctHiddenTip");
  if (tip) {
    const total = grid.querySelectorAll(".acct-card").length;
    const naTotal = grid.querySelectorAll('.acct-card[data-na="1"]').length;
    const hide = total > 0 && shown === 0 && naTotal > 0;
    tip.classList.toggle("hidden", !hide);
    if (hide) {
      tip.textContent = `已收起 ${naTotal} 个「不适用」账号（无积分概念的渠道）· 点汇总条末尾的「不适用」展开`;
    }
  }
}

// setAcctFilter 设置筛选并同步汇总条高亮与账号卡片可见性。
function setAcctFilter(kind) {
  acctFilter = (acctFilter === kind) ? null : kind; // 再次点击同一渠道 = 取消
  renderSummary();
  applyAcctFilter();
}

// bindSummaryFilter 汇总条 chip 点击/键盘筛选（事件委托，绑定一次）。
function bindSummaryFilter() {
  const el = $("creditSummary");
  if (!el) return;
  const pick = (target) => {
    // 「不适用」开关走独立分支：它不是渠道筛选（没有 data-wg），点了要展开/收起整组。
    if (target.closest("[data-na-toggle]")) { toggleNoCredit(); return; }
    const chip = target.closest(".cs-chip.clickable");
    if (!chip) return;
    setAcctFilter(chip.dataset.wg || null);
  };
  el.addEventListener("click", (e) => pick(e.target));
  el.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") { e.preventDefault(); pick(e.target); }
  });
}