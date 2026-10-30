"use strict";
// ---------- 初始化 ----------
(async function init() {
  bind();
  bindMainTabs();
  bindUsage();
  // 会话探针（不返回 401）：未登录状态下只拉这接口，避免控制台报错
  try {
    const st = await api("/api/auth/state");
    if (st.auth_enabled && !st.auth_session) { showLogin(); return; }
    authSession = st.auth_session || "";
  } catch (e) { /* 探针失败走下面的常规加载（如代理拦截） */ }  loadUsage(); // 页面加载即拉取（首次渲染自动刷新，不依赖手动点击）
  await loadState(); // 状态瞬间返回
  await loadFees();  // 费率表用缓存/静态兜底，秒开
  // 运行统计：统计/请求日志 30s、运行日志 15s 常驻轮询
  // （不依赖当前 tab——里程碑 toast 要在任何 tab 下及时弹出；tab 切入时另有即时拉取）
  loadStats();
  setInterval(loadStats, STATS_POLL_MS);
  loadAppLog();
  setInterval(loadAppLog, APPLOG_POLL_MS);
})();

function fmtCredits(n) {
  if (n == null) return "-";
  return Number(n).toLocaleString("zh-CN");
}

function fmtTokensFull(n) {
  if (!n) return "0";
  if (n >= 1e8) return (n / 1e8).toFixed(2) + "亿";
  if (n >= 1e4) return (n / 1e4).toFixed(1) + "万";
  return Number(n).toLocaleString("zh-CN");
}

// fmtAxisTokens 坐标轴专用紧凑刻度：万/亿取整不带小数（刻度无需精度，悬浮明细仍用 fmtTokensFull）。
// 「8000.0万」这类长标签会撑爆 50px 网格边距、被画布裁成 0000.0万。
function fmtAxisTokens(n) {
  if (!n) return "0";
  if (n >= 1e8) return Math.round(n / 1e8) + "亿";
  if (n >= 1e4) return Math.round(n / 1e4) + "万";
  return String(Math.round(n));
}

// cacheCell 渲染「缓存利用率」单元格。**优雅降级的三态**（后端约定）：
//   cr_cov > 0 → 有上报样本，显示百分比（可准确到 0%，那是真没命中）
//   cr_cov = 0 → 上游不提供该维度（glm / raccoon 等）→ 显示「—」，**绝不显示 0%**
// 把「命中率为零」与「不知道」区分开是本列的核心要求：后者显示 0% 会误导用户
// 以为换渠道/改提示词就能改善，而实际上是上游根本不回报这个指标。
function cacheCell(r) {
  const cov = Number(r.cr_cov) || 0;
  if (cov <= 0) return `<span class="muted" title="该模型的上游不提供缓存命中字段（本工具不做估算）">—</span>`;
  const hit = Number(r.cr) || 0;
  const pct = (hit / cov) * 100;
  // 样本量少时刻意标低可信度：单次请求的命中率很可能不代表稳态
  const n = Number(r.cr_reqs) || 0;
  const tip = `命中 ${fmtTokensFull(hit)} / 上报请求 prompt ${fmtTokensFull(cov)}（${n} 次上报请求）`;
  const shown = pct >= 10 ? pct.toFixed(1) : pct.toFixed(2);
  return `<span class="cache-pct" title="${esc(tip)}">${shown}%</span>`;
}

// renderUsageCacheCard 汇总卡：无任何上报样本时显示「—」（同样不伪造 0%）
function renderUsageCacheCard(t) {
  const cov = Number(t.cr_cov) || 0;
  const el = $("ucCache");
  if (cov <= 0) { el.textContent = "—"; el.title = "当前窗口内没有渠道上报缓存字段"; return; }
  const pct = ((Number(t.cr) || 0) / cov) * 100;
  el.textContent = (pct >= 10 ? pct.toFixed(1) : pct.toFixed(2)) + "%";
  el.title = `命中 ${fmtTokensFull(t.cr)} / 上报请求 prompt ${fmtTokensFull(cov)}（${t.cr_reqs || 0} 次上报请求）；未上报的渠道不计入分母`;
}

async function loadUsage() {
  try {
    const st = await api(`/api/usage?days=${usageDays}`);
    if (st.disabled) {
      return;
    }
    renderUsage(st);
  } catch (e) { /* 统计加载失败不阻塞 */ }
}

function renderUsage(st) {
  $("ucTokens").textContent = fmtTokensFull(st.token.total);
  $("ucReqs").textContent = fmtCredits(st.token.requests);
  $("ucSpend").textContent = fmtCredits(st.credit.spend);
  $("ucEarn").textContent = fmtCredits(st.credit.earn);
  renderUsageCacheCard(st.token || {});
  syncUsageRangeActive(); // 高亮始终跟随 usageDays（含异步返回路径）

  renderModelTable(st.token.by_model || [], usageDays);
  renderCreditTab(st.credit || {});
  renderUsageDonut(st);
  renderCreditDonut((st.credit || {}).entries);
}

// syncUsageRangeActive 把「今日/7日」按钮高亮同步为当前 usageDays。
// 单一入口：初始化、点击切换、异步返回都走它，避免三处 toggle 竞态
// （此前点击后 loadUsage 异步返回时高亮被回写错误的 bug）。
function syncUsageRangeActive() {
  $("usageRange").querySelectorAll(".seg-btn").forEach((x) =>
    x.classList.toggle("active", parseInt(x.dataset.days, 10) === usageDays));
}

function renderModelTable(rows, days) {
  const tb = $("tblModels").querySelector("tbody");
  if (!rows.length) { tb.innerHTML = `<tr><td colspan="7" class="empty-cell">暂无数据（流水自本功能上线后开始记录）</td></tr>`; return; }
  const top = rows.slice(0, MODEL_TOP_N); // 前 20 名，已按 token 降序
  tb.innerHTML = top.map((r) => {
    const avg = days > 1 ? Math.round((r.pt + r.ct) / days) : (r.pt + r.ct);
    return `<tr>
      <td>${esc(r.model)}</td><td>${chBadge(r.channel)}</td>
      <td class="num">${fmtCredits(r.requests)}</td><td class="num">${fmtTokensFull(r.pt + r.ct)}</td>
      <td class="num">${fmtTokensFull(avg)}</td>
      <td class="num">${cacheCell(r)}</td><td class="num">${perfCellFor(r.channel, r.model)}</td></tr>`;
  }).join("");
}

function renderCreditTab(credit) {
  const entries = credit.entries || [];
  const names = credit.name_map || {};
  // 分页渲染流水（环形由 renderCreditDonut 基于同一份条目聚合）
  recentAll = entries;
  recentPage = 0;
  lastNames = names;
  renderRecentPage(names);
  renderCreditDonut(entries);
}

function renderRecentPage(names) {
  const kinds = { earn: "↑签到/发放", spend: "↓消耗", expire: "✖过期" };
  const pages = Math.max(1, Math.ceil(recentAll.length / RECENT_PAGE_SIZE));
  if (recentPage >= pages) recentPage = pages - 1;
  // 倒序展示（最新在前）：从尾部往前取当前页
  const end = recentAll.length - recentPage * RECENT_PAGE_SIZE;
  const start = Math.max(0, end - RECENT_PAGE_SIZE);
  const slice = recentAll.slice(start, end).reverse();
  $("recentList").innerHTML = slice.length
    ? slice.map((r) => `<div class="rline ${r.kind}">
        <span class="rtime">${fmtDateTime24(r.ts)}</span>
        <span class="rkind">${kinds[r.kind] || r.kind}</span>
        <span class="ramount">${fmtCredits(r.amount)}</span>
        <span class="racct">${chBadge(r.channel)} ${esc((names || {})[r.uid] || shortUid(r.uid))}</span>
        <span class="rname">${esc(r.note || "")}</span>
        <span class="rbal">余额 ${fmtCredits(r.balance)}</span></div>`).join("")
    : `<div class="muted" style="padding:8px">暂无流水</div>`;
  $("pgInfo").textContent = `${recentPage + 1} / ${pages}`;
  $("pgPrev").disabled = recentPage === 0;
  $("pgNext").disabled = recentPage >= pages - 1;
}
