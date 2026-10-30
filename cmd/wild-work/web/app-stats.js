"use strict";
// ---------- 运行统计面板（第三个主 tab；数据来自 internal/stats 引擎的 /api/stats） ----------
const STATS_POLL_MS = 30000;   // 统计/请求日志轮询周期
const APPLOG_POLL_MS = 15000;  // 运行日志（app.log）轮询周期
const APPLOG_MAX_LINES = 200;  // 渲染上限（上游 /api/logs 最多回 300 行，前端再多不渲染）
const STATS_LOGS_URL = "/api/stats/logs?limit=15";
const TOAST_SEEN_KEY = "stats-toast-seen"; // 里程碑 toast 已读去重（localStorage，防每轮轮询重复弹）

// 小数积分（模型行）：整数走千分位，非整数保留两位
function fmtCreditS(x) {
  x = Number(x) || 0;
  return Number.isInteger(x) ? fmtCredits(x) : x.toFixed(2);
}
// tok/s 一位小数（≥100 取整）；0 值由调用方给「—」不解为 0 速度
function fmtPct(x) {
  if (!x) return "";
  return x >= 100 ? String(Math.round(x)) : x.toFixed(1);
}
// Trae 系渠道判定（TraeWork/TraeCode 共用账号积分池，上游 usage 帧不含单次积分 → 积分为分摊估算）
function isTraeChannel(model) {
  const s = String(model || "");
  const i = s.indexOf("/");
  return i > 0 && (s.slice(0, i) === "traework" || s.slice(0, i) === "traecode");
}

let statsRefreshing = false; // 防重入：上轮未完成时跳过本轮（防慢响应晚到覆盖新数据）
let statsWarned = false;     // 拉取失败只 warn 一次，避免控制台刷屏
let statsLastOk = "";        // 最近一次成功拉取时刻（失败角标要注明数据停止于何时）

// 更新角标：成功 → 记录时刻；失败 → 标红注明数据停止时间（陈旧数据必须可感知）
function stMarkUpdated() {
  const el = $("stUpdated");
  el.classList.remove("st-stale");
  el.textContent = "上次更新 " + (statsLastOk = new Date().toTimeString().slice(0, 8));
}
function stMarkStale() {
  const el = $("stUpdated");
  el.classList.add("st-stale");
  el.textContent = statsLastOk ? `拉取失败 · 数据停止于 ${statsLastOk}` : "拉取失败";
}

async function loadStats() {
  if (statsRefreshing) return;
  statsRefreshing = true;
  try {
    const [s, lg] = await Promise.all([
      api("/api/stats"),
      api(STATS_LOGS_URL).catch(() => null), // 失败保旧：跳过本轮渲染，不清空已渲染的请求日志
    ]);
    lastStats = s; // 供三场景推荐排序取 models/platforms 活值（与面板同源，不另拉一份）
    renderStats(s);
    renderAbnormal(s.abnormal || []);
    renderFallbacks(s.fallbacks || []);
    if (lg) renderLogs(lg.rows || []);
    stMarkUpdated();
  } catch (e) {
    stMarkStale();
    if (!statsWarned) { statsWarned = true; console.warn("运行统计加载失败：", e); }
  } finally {
    statsRefreshing = false;
  }
}

async function loadAppLog() {
  try {
    const d = await api("/api/logs");
    renderAppLog(d.lines || []);
  } catch (e) { /* 上游瞬时不可达：保留旧内容，下轮重试 */ }
}

// 汇总横条条目（标签 + 等宽数值 + 可选小字注解）
function stTkItem(label, val) {
  return `<span class="st-tk-item">${label} <span class="st-tk-val">${val}</span></span>`;
}

// perfLookup 运行统计引擎的「模型 → 速度」索引（key = "channel/model"）。
// 速度数据只存在于 stats 引擎内存（ledger 只扫文件、无速度字段），
// 用量 tab 展示平均速度时按此表匹配；键形与 stats 的 models[].model 一致。
let perfLookup = {};

// perfCellFor 按 "channel/model" 取速度单元格；无样本显示「—」（与运行统计同款三态）。
function perfCellFor(channel, model) {
  const m = perfLookup[channel + "/" + model];
  if (!m) return `<span class="muted" title="该模型暂无成功请求的速度样本">—</span>`;
  const tps = Number(m.avg_tok_per_sec) || 0;
  const ttfb = Number(m.avg_ttfb_ms) || 0;
  const parts = [];
  if (tps > 0) parts.push(fmtPct(tps) + " tok/s");
  if (ttfb > 0) parts.push(fmtCredits(ttfb) + "ms");
  if (!parts.length) return `<span class="muted" title="无速度样本">—</span>`;
  return `<span title="样本 ${fmtCredits(m.perf_samples)} 次成功请求">${parts.join(" · ")}</span>`;
}

// buildPerfLookup 用 stats 快照的 models[] 建索引（30s 轮询时刷新）。
function buildPerfLookup(models) {
  const lk = {};
  for (const m of models || []) {
    if (m && m.model) lk[m.model] = m;
  }
  perfLookup = lk;
}

function renderStats(s) {
  const tu = s.token_usage || {};
  // 汇总横条已删（与合计行重复）：全池汇总数字全部由平台表合计行呈现，
  // 仅 >0 才出现的「今日作废/今日到期」也走合计行（列常年存在，零值置灰）。
  $("stMilestone").textContent = s.milestone_count > 0 ? `里程碑 ${s.milestone_count}×100分` : "";
  renderPlatformTable(s);
  // Token 多天行（今日情况标题右侧）
  $("stToken").innerHTML =
    stTkItem("Token 今日", fmtTokensFull(tu.today)) +
    stTkItem("7日", fmtTokensFull(tu.days7)) +
    stTkItem("30日", fmtTokensFull(tu.days30)) +
    stTkItem("请求 今日", fmtCredits(tu.req_today) + " / " + fmtCredits(tu.req_all)) +
    stTkItem("已记录", fmtCredits(tu.days) + " 天");
  renderRotation(s.rotation || []);
  renderExpiry(s.expiry || []);
  renderModels(s.models || []);
  buildPerfLookup(s.models || []); // 用量 tab 的速度列数据源（同源同频刷新）
  handleToasts(s.toasts || []);
  // R129：首页与统计同源——指标卡要 credit_in/used/total/expiring，三场景摘要要账号池状态。
  // 放在这里而不是各拉一次接口，保证「首页数字」与「平台表」永远同一时刻的一致快照。
  renderHomeCards();
  renderHomeRecent();
  renderHomeScenes();
  // 模型管理页可能先于 state 到达就被打开过；这里补一次（门车型，已有内容不覆盖）
  const mm = $("mtabModels");
  if (mm && !mm.classList.contains("hidden")) ensureSceneEditor();
}

// 平台表（今日情况主视图）：列 = #/平台/账号/当前积分/今日收入/今日消耗(净值)/今日作废/
// 今日消耗Token/今日到期/明日到期/7日内到期，末行合计。「今日消耗」= 差值花费 − 积分包到期作废
// （净值才是真消耗）；「今日作废」= ledger 当日 expire 流水（往回看：已经没了多少），
// 与「今日到期」（往前看：今晚余额里还有多少会没）是相反方向的两个口径，不能混为一列。
//
// 到期三列口径（2026-10-19 用户定义，后端 provider.ExpireBucketOf 同一实现）：
// 按**精确时刻**判定，桶的边界是**自然日**，且**次日 00:00:00 失效算今日**——
// 上游每日类额度（qwenwork valid_to、traework 签到 expire_time）就是次日零点失效，
// 归「明日」会让人以为还有一整天。列头 title 全部收敛到「今日情况」旁那一个 ? 角标，
// 避免同一套说明在四个地方各写一遍、改口径时漏改。
function renderPlatformTable(s) {
  const rows = s.platforms || [];
  if (!rows.length) { $("stPlat").innerHTML = ""; return; }
  let html = `<div class="st-plat-row st-plat-head">` +
    `<span class="st-plat-no">#</span>` +
    `<span class="st-plat-slot">平台</span>` +
    `<span class="st-plat-accts">账号</span>` +
    `<span class="st-plat-credits">当前积分</span>` +
    `<span class="st-plat-in">今日收入</span>` +
    `<span class="st-plat-out" title="花费差值扣除积分包到期作废后的净消耗">今日消耗</span>` +
    `<span class="st-plat-expd" title="今日已到期的积分包额度（已从余额消失，不是消耗）">今日作废</span>` +
    `<span class="st-plat-tokens">今日消耗Token</span>` +
    `<span class="st-plat-exp">今日到期</span>` +
    `<span class="st-plat-exp">明日到期</span>` +
    `<span class="st-plat-exp">7日内到期</span>` +
    `</div>`;
  rows.forEach((r, i) => {
    html += `<div class="st-plat-row">` +
      `<span class="st-plat-no">${i + 1}</span>` +
      `<span class="st-plat-slot">${chBadge(r.group)}</span>` +
      `<span class="st-plat-accts">${fmtCredits(r.accounts)}</span>` +
      stPlatNum("st-plat-credits", r.credits, "分") +
      stPlatNum("st-plat-in", r.credit_in) +
      stPlatNum("st-plat-out", r.credit_used) +
      stPlatNum("st-plat-expd", r.credit_expired) +
      stPlatTok(r.tokens) +
      stPlatNum("st-plat-exp", r.expire_today, "分") +
      stPlatNum("st-plat-exp", r.expire_tomorrow, "分") +
      stPlatNum("st-plat-exp", r.expire_7d, "分") +
      `</div>`;
  });
  // 合计行：与旧汇总横条同信息量——消耗列带「精确」小注（逐请求 usage 帧口径），
  // title 悬浮说明净消耗 = 差值花费 - 作废。
  const sumExact = Number(s.credit_out_exact) > 0 ? ` title="净消耗 ${fmtCredits(s.credit_used)}：差值扣除作废 ${fmtCredits(s.credit_expired)}；逐请求精确 ${fmtCreditS(s.credit_out_exact)}"` : "";
  html += `<div class="st-plat-row st-plat-sum-row">` +
    `<span class="st-plat-no"></span>` +
    `<span class="st-plat-slot">合计</span>` +
    `<span class="st-plat-accts">${fmtCredits(s.total_accounts)}</span>` +
    stPlatNum("st-plat-credits", s.total_credits, "分") +
    stPlatNum("st-plat-in", s.credit_in) +
    `<span class="st-plat-out"${sumExact}>${fmtCredits(s.credit_used)}${Number(s.credit_out_exact) > 0 ? ` <span class="st-plat-sub">(精 ${fmtCreditS(s.credit_out_exact)})</span>` : ""}</span>` +
    stPlatNum("st-plat-expd", s.credit_expired) +
    stPlatTok(s.tokens) +
    stPlatNum("st-plat-exp", s.expire_today, "分") +
    stPlatNum("st-plat-exp", s.expire_tomorrow, "分") +
    stPlatNum("st-plat-exp", s.expiring_7d, "分") +
    `</div>`;
  $("stPlat").innerHTML = html;
  fitPlatCols();
}

// fitPlatCols 平台表动态列宽：每行是独立 grid，固定 px 遇到大额数字会截断
// （合计行「(精 xxx.xx)」/积分上千/万）；也不能改 auto 列——各行独立解析会跨行错位。
// 渲染后实测每列所有行（含表头/合计）最宽 cell 的 scrollWidth，经容器 CSS 变量
// --plat-cols 统一应用。不可见时（tab 隐藏）scrollWidth 全为 0，跳过等切回时重测。
function fitPlatCols() {
  const el = $("stPlat");
  const rows = [...el.querySelectorAll(".st-plat-row")];
  if (!rows.length || !rows[0].offsetParent) return;
  // 测量「内容需要的宽度」：cell 是 overflow:hidden 的 flex 容器，scrollWidth 被
  // 当前列宽钳住（列宽 96px 永远量出 96px，死循环自洽）。改成临时把所有 cell
  // position:absolute + width:max-content：脱离 grid 流按内容自适应撑开，
  // offsetWidth 即真实需求；一次性进入测量态、统一读、再统一还原（避免逐 cell 反复重排）。
  const cells = [];
  for (const r of rows) {
    for (const c of r.children) {
      cells.push([c, c.style.position, c.style.width]);
      c.style.position = "absolute";
      c.style.width = "max-content";
    }
  }
  const max = [];
  rows.forEach((r) => {
    [...r.children].forEach((c, i) => {
      max[i] = Math.max(max[i] || 0, c.offsetWidth);
    });
  });
  for (const [c, p, w] of cells) { c.style.position = p; c.style.width = w; }
  // 首列(#)固定 28px；平台列 minmax(实测, 1fr)——吸收表格剩余空白（徽章左对齐，
  // 空白自然落在徽章右侧）；其余列固定为实测宽 +2px 余量。
  // 末列不用 1fr：总宽超出时 1fr 会被压到内容之下把数字剪掉；
  // 也不用 max-content：它不吸收空白，右侧会裸露一块「无主宽度」。
  const cols = max.map((w, i) => {
    if (i === 0) return "28px";
    if (i === 1) return `minmax(${w + 2}px, 1fr)`;
    return (w + 2) + "px";
  });
  el.style.setProperty("--plat-cols", cols.join(" "));
}

// 平台表数值单元格：0 值置灰「—」（避免满屏 0 淹没真正有数的列），颜色由列类自带
function stPlatNum(cls, amount, suffix) {
  amount = Number(amount) || 0;
  if (amount <= 0) return `<span class="${cls} st-plat-zero">—</span>`;
  return `<span class="${cls}">${fmtCredits(amount)}${suffix || ""}</span>`;
}
// Token 单元格：单位走 万/亿，0 同样置灰
function stPlatTok(amount) {
  amount = Number(amount) || 0;
  if (amount <= 0) return `<span class="st-plat-tokens st-plat-zero">—</span>`;
  return `<span class="st-plat-tokens">${fmtTokensFull(amount)}</span>`;
}

const ST_ROT_MARKS = ["①", "②", "③"];

// groupUnit 渠道的计量单位（""=积分 / "token"=Token 计数）：从 state.accounts 同渠道账号取，
// 保证与卡片、汇总条、费率页角标**同源**（单一来源仍是后端的 credits_unit）。
function groupUnit(group) {
  const a = (state.accounts || []).find((x) => (x.group || "workbuddy") === group);
  return a && a.credits_unit === "token" ? "token" : "";
}

// 使用中 / 接棒顺序：当前=粘性账号；接棒=最早到期优先推算的前 3
function renderRotation(rot) {
  if (!rot.length) { $("stRotation").innerHTML = ""; $("stRotation").className = "st-rotation"; return; }
  let html = `<div class="st-block-title">使用中 / 接棒顺序<span class="st-hint">当前=粘性账号 · 接棒=最早到期优先推算</span></div>`;
  for (const rv of rot) {
    // 单位随渠道切换：token 计量渠道（如 ZCode）显示 tokens，不再一律写「分」。
    const isTok = groupUnit(rv.group) === "token";
    const bal = (v) => (isTok ? `${fmtTokens(v)} tokens` : `${fmtCredits(v)}分`);
    const cur = rv.current
      ? `使用中 <b>${esc(rv.current.nickname)}</b>（粘性 ${esc(rv.current.sticky || "0")}，余额 ${bal(rv.current.credits)}）`
      : `<span class="st-rot-none">暂无使用记录</span>`;
    let nextHtml = "";
    (rv.next || []).forEach((n, j) => {
      nextHtml += `<span class="st-rot-next">${ST_ROT_MARKS[j]} ${esc(n.nickname)}` +
        `（${bal(n.credits)}${n.expiring > 0 ? "·临期" + bal(n.expiring) : ""}）</span>`;
    });
    html += `<div class="st-rot-row">${chBadge(rv.group)}` +
      `<span class="st-rot-cur">${cur}</span>` +
      (nextHtml ? `<span class="st-rot-nexts">${nextHtml}</span>` : "") + `</div>`;
  }
  $("stRotation").innerHTML = html;
  $("stRotation").className = "st-rotation st-block";
}

// fmtExpireAt 把上游 ExpireAt 渲染成**完整标准时间**「YYYY-MM-DD HH:MM:SS」。
//
// 为什么需要：后端下发的是 RFC3339（`2026-10-20T00:00:00+08:00`），直接显示
// 又长又被列宽截断（用户实测看不到秒）；纯日期粒度（`2026-10-19`，loomy 之外的
// 少数渠道仍可能给）则补成「该日 23:59:59」——它的语义是「该日全天可用」，
// 补 00:00:00 会误导成「当天零点就没了」。
// 时区固定按 UTC+8 展示（与后端 expireLoc 一致），不依赖浏览器本地时区。
function fmtExpireAt(s) {
  if (!s) return "—";
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s);
  if (m) return `${s} 23:59:59`; // 纯日期：该日全天可用
  const t = Date.parse(s);
  if (Number.isNaN(t)) return s; // 解析不了就原样显示，不吞信息
  const d = new Date(t + 8 * 3600 * 1000); // 转成 UTC+8 墙钟后按 UTC 读数
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getUTCFullYear()}-${p(d.getUTCMonth() + 1)}-${p(d.getUTCDate())} ` +
    `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}`;
}

// 最近临期：到期日升序；risk=按今日速度花不完 → 给预计作废量（额度−外推消耗）
function renderExpiry(ex) {
  if (!ex.length) { $("stExpiry").innerHTML = ""; $("stExpiry").className = "st-expiry"; return; }
  let html = `<div class="st-block-title">最近临期<span class="st-hint">按剩余时长升序 · ${ex.length} 个账号</span></div>` +
    `<div class="st-scroll st-ex-scroll">` +
    `<div class="st-ex-row st-ex-head">` +
    `<span class="st-ex-tag">#</span>` +
    `<span class="st-ex-plat-slot">平台</span>` +
    `<span class="st-ex-name">账号</span>` +
    `<span class="st-ex-date">到期时刻</span>` +
    `<span class="st-ex-days">剩余</span>` +
    `<span class="st-ex-amt">额度 · 今日已耗</span>` +
    `</div>`;
  ex.forEach((r, i) => {
    // 剩余时长优先用后端下发的精确小时数（hours_left），退回自然日文案兜底。
    // 精确到 0.1h 才能表达「今晚 5.7 小时后就没」，这是本轮改动的核心诉求。
    const h = Number(r.hours_left);
    const dayTxt = Number.isFinite(h) && h > 0
      ? (h < 1 ? `${Math.round(h * 60)} 分钟后` : `${h.toFixed(1)} 小时后`)
      : (r.days_left <= 0 ? "今日到期" : (r.days_left === 1 ? "明日到期" : `${r.days_left} 天后到期`));
    // 预计作废：burn_forecast<0 表示观测不足、无法外推，此时按「全部额度都会作废」
    // 展示（与后端 Risk 判定同口径），不能显示「作废 -1 分」这种反数。
    const burn = r.risk
      ? ` · 预计作废 ${fmtCredits(r.burn_forecast < 0 ? r.amount : r.amount - r.burn_forecast)}分`
      : "";
    // 配色按**紧迫度**（后端 urgency），不按 risk：
    //   今天到期 → 红（st-risk）；明天到期 → 黄（st-warn）；更远 → 不上色。
    // 此前用 risk 上色，7 天内的行全飘红（135 小时后到期的也红），一片警示等于没有警示。
    // 「预计作废」文字仍只跟 risk 走：那是数据结论，与颜色分档独立。
    const urg = r.urgency === "today" ? " st-risk"
      : (r.urgency === "tomorrow" ? " st-warn" : "");
    html += `<div class="st-ex-row${urg}">` +
      `<span class="st-ex-tag">${i + 1}</span>` +
      `<span class="st-ex-plat-slot">${chBadge(r.group)}</span>` +
      `<span class="st-ex-name" title="${esc(r.uid)}">${esc(r.nickname || r.uid)}</span>` +
      `<span class="st-ex-date">${esc(fmtExpireAt(r.expire_at))}</span>` +
      `<span class="st-ex-days">${esc(dayTxt)}</span>` +
      `<span class="st-ex-amt">${fmtCredits(r.amount)}分 · 已耗 ${fmtCredits(r.today_out)}${burn}</span>` +
      `</div>`;
  });
  html += `</div>`;
  $("stExpiry").innerHTML = html;
  $("stExpiry").className = "st-expiry st-block";
}

// 模型消耗（今日，按积分降序）：Trae 系积分加「≈」（分摊估算），平均速度=成功请求实测均值
function renderModels(models) {
  let mh = `<tr><th>#</th><th>模型</th><th>次数</th><th>Token</th><th>缓存</th><th>积分</th>` +
    `<th title="成功请求实测均值：tok/s = 生成 token ÷ 生成耗时（按时长加权）；TTFB = 首字节耗时算术平均">平均速度</th></tr>`;
  models.forEach((m, j) => {
    const isTrae = isTraeChannel(m.model);
    let creditCell;
    if (m.credit > 0) {
      creditCell = isTrae
        ? `<span title="TraeWork 上游不下发单次积分：按该渠道今日真实消耗（余额差值）与 token 占比分摊，为估算值">≈${fmtCreditS(m.credit)}</span>`
        : fmtCreditS(m.credit);
    } else {
      creditCell = isTrae
        ? `<span title="TraeWork 无单次精确积分，按渠道真实消耗分摊；暂无余额差值消耗">—</span>`
        : fmtCreditS(m.credit);
    }
    const tps = Number(m.avg_tok_per_sec) || 0;
    const ttfb = Number(m.avg_ttfb_ms) || 0;
    const parts = [];
    if (tps > 0) parts.push(fmtPct(tps) + " tok/s");
    if (ttfb > 0) parts.push(fmtCredits(ttfb) + "ms");
    const perfCell = parts.length
      ? `<span title="样本 ${fmtCredits(m.perf_samples)} 次成功请求">${parts.join(" · ")}</span>`
      : "—";
    mh += `<tr><td class="st-idx">${j + 1}</td>` +
      `<td class="st-mono" title="${esc(m.model)}">${esc(m.model)}</td>` +
      `<td>${fmtCredits(m.reqs)}</td><td>${fmtTokensFull(m.tokens)}</td>` +
      `<td class="st-mono">${cacheCell(m)}</td>` +
      `<td>${creditCell}</td>` +
      `<td class="st-mono">${perfCell}</td></tr>`;
  });
  $("stModels").innerHTML = models.length ? mh : `<tr><td class="st-empty" colspan="7">今日暂无消耗</td></tr>`;
}

// 请求日志：降序渲染（最新在最上）；tok/s 仅流式有值，0/空给「—」
function renderLogs(rows) {
  let h = `<tr><th>#</th><th>时间</th><th>模型</th><th>模式</th><th>状态</th><th>TTFB</th><th>tok</th><th>tok/s</th><th>total</th><th>积分</th></tr>`;
  for (let i = rows.length - 1; i >= 0; i--) {
    const r = rows[i];
    const status = r.status < 400 ? String(r.status) : `<span class="st-bad">${r.status}</span>`;
    const tps = r.tok_per_sec > 0 ? fmtPct(r.tok_per_sec) : "—";
    h += `<tr><td>${fmtCredits(r.seq)}</td><td class="st-mono">${esc(r.time)}</td><td class="st-mono">${esc(r.model)}</td>` +
      `<td>${esc(r.mode)}</td><td>${status}</td><td>${fmtCredits(r.ttfb_ms)}ms</td><td>${fmtCredits(r.tok)}</td><td>${tps}</td>` +
      `<td>${r.total_sec ? r.total_sec.toFixed(1) + "s" : "—"}</td><td>${r.credit > 0 ? fmtCreditS(r.credit) : "—"}</td></tr>`;
  }
  $("stLogs").innerHTML = rows.length ? h : `<tr><td class="st-empty">暂无请求</td></tr>`;
}
