"use strict";
// ---------- 积分明细浮窗 ----------
// 明细条数可能很多（TraeWork 每个签到奖励都是独立条目，常见 30+），
// 故分页展示：每页 DETAIL_PAGE_SIZE 条，页码状态存在 detailState 里，
// 翻页不重新请求（detailCache 已缓存该账号的完整响应）。
//
// 交互：**点击**积分标签打开，✕ / 点击浮窗之外 / Esc 关闭（三个关闭路径都归一到
// closeCreditDetail）。为什么不用 hover：
//   ① 浮窗开着时后台 loadState() 会重建卡片 DOM，被替换的元素不再派发 mouseleave
//      ⇒ 浮窗变成孤儿、关不掉（用户实际遇到的「经常不能自动关闭」就是这个）；
//   ② 触屏设备不派发 mouseenter/mouseleave，手机上既打不开也关不掉；
//   ③ hover 模型把「翻页」与「移出即关」耦合成矛盾诉求（要翻页就得移进浮窗）。
const DETAIL_PAGE_SIZE = 8;
// EXPIRING_DAYS 面板「临期」标记的展示窗口（自然日）。
// 与后端 provider.ExpiringDays 同值：2026-10-20 起阈值不再是设置项，
// 选号看「最早到期时刻」（R112），这个窗口只影响明细里的红色「临期」章。
const EXPIRING_DAYS = 1;
let detailCache = {};
// detailState 当前浮窗的展示状态：{uid, page}。
let detailState = null;
// detailOpenSeq 打开序号：快速连点两个账号时，只让「最后一次点击」的响应上屏。
// 取代了 hover 版靠 detailTimer 的竞态守卫（点击式没有「等待期间鼠标移开」这回事）。
let detailOpenSeq = 0;

// closeCreditDetail 关闭浮窗（唯一的关闭入口，X / 点外部 / Esc / 切 tab 都走它）。
function closeCreditDetail() {
  const tip = $("creditTip");
  if (tip) tip.style.display = "none";
  detailOpenSeq++; // 让在途的明细请求失效，避免关掉后又被异步回调弹出来
}

// bindCreditDetailDismiss 绑定浮窗的外部关闭路径（绑定一次）。
// 为什么用 document 级委托而不是给每个入口挂 handler：浮窗与卡片都是动态重建的，
// 委托不受重建影响——这正是旧 hover 实现的失效根源（元素换掉后监听器一起消失）。
function bindCreditDetailDismiss() {
  document.addEventListener("click", (e) => {
    const tip = $("creditTip");
    if (!tip || tip.style.display === "none") return;
    // 归属判定必须用事件传播路径（composedPath），不能用 e.target.closest：
    // showCreditDetail → syncCreditsFromDetail 会在本次派发途中重写积分格 innerHTML，
    // 把被点的 SPAN 从 DOM 摘掉；detached 节点的 closest() 恒为 null，
    // 会让「点另一个账号的标签」被误判成「点外部」而把刚打开的浮窗关掉。
    // composedPath 在派发时就已固化，不受中途 DOM 替换影响。
    const path = e.composedPath ? e.composedPath() : [];
    const inPath = (pick) => path.some((n) => n instanceof Element && pick(n));
    if (inPath((n) => n.getAttribute("data-close-credit-tip") === "1")) { closeCreditDetail(); return; }
    if (path.includes(tip) || tip.contains(e.target)) return;          // 浮窗内部（含翻页）不关
    if (inPath((n) => n.classList.contains("acct-credits"))) return;   // 点另一个账号的标签 = 切换，不关
    closeCreditDetail();                                               // 其余任何位置都关
  });
  // Esc 关闭：与其它弹层（设置/帮助/查找模型）的键盘行为一致
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeCreditDetail(); });
}

async function showCreditDetail(e, uid) {
  // e（内联 onclick 传入的 event）刻意只作占位：浮窗位置由 CSS 固定（水平居中、距顶 12vh），
  // 不需要锚定元素。**不要**在这里取 `e.currentTarget` —— 它在下面第一个 `await` 之后恒为
  // null（事件派发早已结束），留着只会误导后来者（2026-10-17 清理掉一处这样的死代码）。
  const seq = ++detailOpenSeq; // 本次打开的身份；被更晚的打开或关闭动作作废时不再上屏

  // 切换到另一个账号时重置页码；同一账号重复点击保留原页码。
  const page = (detailState && detailState.uid === uid) ? detailState.page : 0;

  let d = detailCache[uid];
  if (!d) {
    try {
      d = await api("/api/account/resource_detail", { uid });
      // 竞态守卫：等待接口期间用户又点了别的账号 / 关闭了浮窗 → 本次结果作废。
      if (seq !== detailOpenSeq) return;
      detailCache[uid] = d;
      syncCreditsFromDetail(uid, d);
    } catch (err) {
      // 不再静默 return：接口失败以前什么都不显示，用户完全不知道发生了什么事。
      // 后端对 oczen / kilo / 自定义渠道直接 400（「无积分，不适用」），其余是网络/
      // 上游错误——两类都必须把原因摆到用户面前（浮窗 + toast 双通道：
      // 浮窗贴在被点的位置、常驻；toast 兜住浮窗可能被下一次点击关掉的情形）。
      if (seq !== detailOpenSeq) return;
      const msg = (err && err.message) ? err.message : "积分明细获取失败";
      renderCreditTipNotice(msg);
      toast(`积分明细获取失败：${msg}`);
      return;
    }
  }
  // 同理：请求成功但一件明细都没有（渠道没下发条目）也必须给出可见解释，不再静默返回。
  if (!d || !d.items || !d.items.length) {
    renderCreditTipNotice("该账号暂无可展示的积分明细（上游未下发条目）。");
    return;
  }

  let tip = $("creditTip");
  if (!tip) {
    tip = document.createElement("div");
    tip.id = "creditTip";
    tip.className = "credit-tip";
    document.body.appendChild(tip);
  }

  // 位置由 CSS 固定（水平居中、距顶 12vh）：不再跟着被点卡片算 left/top，
  // 这样后台重渲染、滚动都不会让浮窗跑偏，也避免窄屏越界。
  detailState = { uid, page };
  renderCreditDetail();
  tip.style.display = "block";
}

// syncCreditsFromDetail 把卡片与汇总条的积分数字回写成明细响应的同源代码。
// 顶层原本是 pool 快照（仅刷新事件/30min 自动循环更新），明细是 hover 实时值，
// 区间内消耗会让「同屏两个数字对不上」；后端 ResourceDetail 现已把同一响应回写
// pool 快照，前端这里再把卡片与汇总条改为同源——弹层展开那一刻全屏数字一致。
// 只改展示状态与对应 DOM，不重请求、不全量重绘（全量重绘会替换正在 hover 的
// 卡片节点，tooltip 锚点与鼠标事件会丢）。
function syncCreditsFromDetail(uid, d) {
  const a = (state.accounts || []).find((x) => x.uid === uid);
  if (!a || a.credits_na) return;
  a.credits = d.usable_remain || 0;
  a.unusable_credits = d.unusable_remain || 0;
  a.expiring_credits = d.expiring_remain || 0;
  a.credits_stale = false;
  const cell = document.querySelector(`.acct-credits[data-uid="${uid}"]`);
  if (cell) cell.innerHTML = creditsText(a);
  renderSummary();
}

// renderCreditTipNotice 在浮窗里显示一条可见提示（替代「静默什么都不发生」）。
// 为什么必须用浮窗而不用 toast：toast 3 秒后消失且位置在页面底部，用户点的是卡片上的
// 某个数字，反馈必须出现在「他刚点的那个地方」，否则容易被当成没反应。
// 提示条沿用浮窗壳子（关闭按钮/点外部/Esc 全部照常），只是正文换成一行说明。
function renderCreditTipNotice(msg) {
  let tip = $("creditTip");
  if (!tip) {
    tip = document.createElement("div");
    tip.id = "creditTip";
    tip.className = "credit-tip";
    document.body.appendChild(tip);
  }
  tip.innerHTML = `<div class="detail-head">`
    + `<span class="detail-count"></span>`
    + `<span class="detail-pager"></span>`
    + `<span class="detail-title">积分明细</span>`
    + `<button type="button" class="detail-close" data-close-credit-tip="1" title="关闭" aria-label="关闭积分明细">✕</button>`
    + `</div><div class="detail-empty">${esc(msg)}</div>`;
  tip.style.display = "block";
}

// renderCreditDetail 按 detailState 重绘 tooltip（含分页控件与可用/不可用小计）。
function renderCreditDetail() {
  const tip = $("creditTip");
  if (!tip || !detailState) return;
  const d = detailCache[detailState.uid];
  // 空明细不能静默 return：此前这里直接返回，用户点了积分数字后浮窗不出现也没有任何解释
  // （典型：渠道有积分但没有下发条目、或该账号额度桶为空）。改为在浮窗里说明。
  if (!d || !d.items || !d.items.length) {
    renderCreditTipNotice("该账号暂无可展示的积分明细（上游未下发条目）。");
    return;
  }

  const items = d.items.slice();
  // 临期判定与后端 provider.ExpireWithinDays 同口径：**自然日**（窗口内到期）。
  // 判据 = 失效时刻 - 零点宽限 所属自然日 ≤ 今天+days（次日 00:00 失效算今日）。
  // 窗口固定 1 天（= 后端 provider.ExpiringDays）：2026-10-20 起阈值不再可配，
  // 选号改看「最早到期时刻」，这个窗口只决定明细里打不打「临期」红章。
  const expDays = EXPIRING_DAYS;
  // expireInstantUtc 失效时刻的真实 UTC 毫秒，与后端 provider.ExpireInstant 同口径：
  //   日期粒度（YYYY-MM-DD）= 该日全天可用 ⇒ **次日 00:00（UTC+8）**才失效；
  //   RFC3339 = 精确时刻（如 gemini 的配额窗口重置、workbuddy 的 CycleEndTime）。
  // 解析不出返回 null（该条目无到期信息）。旧实现用 split("-") + Number()，遇到
  // RFC3339 会得到 NaN 静默退化，故统一收口到这里。
  const EXPIRE_MIDNIGHT_GRACE_MS = 5 * 60 * 1000; // 与后端 expireMidnightGrace 同值
  const expireInstantUtc = (s) => {
    if (!s) return null;
    const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s);
    // 纯日期：该日全天可用 ⇒ 次日零点失效（后端 ExpireInstant 的 t.AddDate(0,0,1)）
    if (m) return Date.UTC(+m[1], +m[2] - 1, +m[3] + 1) - 8 * 3600 * 1000;
    const t = Date.parse(s);
    return Number.isNaN(t) ? null : t;
  };
  // 失效时刻所属「自然日」的 UTC 毫秒（已扣零点宽限）：次日 00:00:03 归前一天。
  const expireDayUtc = (s) => {
    const ms = expireInstantUtc(s);
    if (ms === null) return null;
    const d = new Date(ms - EXPIRE_MIDNIGHT_GRACE_MS + 8 * 3600 * 1000);
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate()) - 8 * 3600 * 1000;
  };
  const nowCst = new Date(Date.now() + 8 * 3600 * 1000);
  const todayUtc = Date.UTC(nowCst.getUTCFullYear(), nowCst.getUTCMonth(), nowCst.getUTCDate()) - 8 * 3600 * 1000;
  // 临期：失效自然日 ≤ 今天 + expDays 天（含），且尚未失效
  const isExpiring = (s) => {
    const day = expireDayUtc(s), inst = expireInstantUtc(s);
    if (day === null || inst === null) return false;
    if (inst <= Date.now()) return false;
    return day <= todayUtc + expDays * 24 * 3600 * 1000;
  };
  // 排序：临期（可消耗、有剩余、窗口内到期）→ 可用 → 已用完/不可用/仅展示垫底；组内按到期日升序。
  const expUtcOf = (it) => {
    const ms = expireInstantUtc(it.expire_at);
    return ms === null ? Infinity : ms;
  };
  const groupOf = (it) => (it.usable && it.remain > 0) ? (isExpiring(it.expire_at) ? 0 : 1) : 2;
  items.sort((a, b) => groupOf(a) - groupOf(b) || expUtcOf(a) - expUtcOf(b));
  const pages = Math.max(1, Math.ceil(items.length / DETAIL_PAGE_SIZE));
  const page = Math.min(Math.max(0, detailState.page), pages - 1);
  detailState.page = page;
  const slice = items.slice(page * DETAIL_PAGE_SIZE, (page + 1) * DETAIL_PAGE_SIZE);

  // 有效期列仅当上游确实下发了到期时间时才出现——渠道未返回则不显示该列，
  // 避免一列全空或把「无到期信息」误读成「永不过期」。
  const hasExpiry = items.some((it) => it.expire_at);

  // 头部：翻页器在顶部居中，标题与条数分列两端（用户方案：翻页不跨出浮窗）。
  let html = `<div class="detail-head">`;
  html += `<span class="detail-count">共 ${items.length} 条</span>`;
  if (pages > 1) {
    html += `<span class="detail-pager">`;
    html += `<span class="detail-pg${page === 0 ? " off" : ""}" data-pg="${page - 1}">‹</span>`;
    html += `<span class="detail-pg-info">${page + 1} / ${pages}</span>`;
    html += `<span class="detail-pg${page >= pages - 1 ? " off" : ""}" data-pg="${page + 1}">›</span>`;
    html += `</span>`;
  }
  // 标题随计量单位切换：token 渠道不叫「积分明细」（避免把 token 数当积分读）。
  // ⚠️ 账号必须由 detailState.uid 取（本函数的单一数据源）：`uid` 是 showCreditDetail 的
  // **形参**，不在本函数作用域内。此处曾写成 `x.uid === uid` ⇒ ReferenceError: uid is not
  // defined，抛在下面的 `tip.innerHTML = html` 之前 ⇒ 浮窗 display:block 但内容为空，
  // 用户看到的就是「点积分数字什么都不出现」（发布级回归，由 scripts/ui-static-check.mjs 锁死）。
  const detailAcct = (state.accounts || []).find((x) => x.uid === detailState.uid);
  const detailIsToken = !!(detailAcct && detailAcct.credits_unit === "token");
  html += `<span class="detail-title">${detailIsToken ? "Token 额度明细" : "积分明细"}</span>`;
  // 关闭按钮：桌面与触屏共用同一入口（触屏没有 hover，只能靠显式按钮）
  html += `<button type="button" class="detail-close" data-close-credit-tip="1" title="关闭" aria-label="关闭积分明细">✕</button>`;
  html += `</div>`;
  html += `<table class="detail-table"><thead><tr><th>套餐</th><th>总额</th><th>已用</th><th>剩余</th>`;
  if (hasExpiry) html += `<th>有效期</th>`;
  html += `</tr></thead><tbody>`;
  for (const it of slice) {
    // 不可用额度整行淡显 + 角标，与可用额度区分开（如 TraeWork 的官方客户端专用池）。
    let cls = it.usable ? "" : ' class="detail-unusable"';
    let tag = "";
    if (!it.usable) {
      tag = '<span class="detail-tag" title="该额度仅供官方客户端使用，本工具无法消耗">不可用</span>';
    } else if (it.unit === "token") {
      // Token 计量（ZCode 的额度桶、MonkeyCode 的每日 Token 额度）：明示单位，不计入积分合计。
      tag = '<span class="detail-tag" title="以 token 计量，不计入积分合计">tokens</span>';
    } else if (it.info_only) {
      // 其它「单位与积分不同」的展示型条目：展示但不计入合计。
      tag = '<span class="detail-tag" title="单位与积分不同，仅作展示，不计入合计">仅展示</span>';
    }
    // 临期条目（可消耗且有剩余、窗口内到期，与后端 ExpiringWithin 余额口径一致）：
    // 整行淡红底 + 到期日红字 + 临期红章；已用完（remain=0）不标临期，避免与汇总口径打架。
    let expCell = "-", expCellCls = "";
    if (it.usable && it.remain > 0 && it.expire_at) {
      const expUtc = expireInstantUtc(it.expire_at);
      if (expUtc !== null) {
        if (isExpiring(it.expire_at)) {
          expCell = `${esc(it.expire_at)}<span class="detail-exp-tag" title="今日到期（${expDays} 个自然日，次日 00:00 失效算今日），选号时优先消耗">临期</span>`;
          expCellCls = ' class="detail-exp"';
          cls = ' class="detail-exp-row"';
        } else {
          expCell = esc(it.expire_at);
        }
      } else {
        expCell = esc(it.expire_at);
      }
    }
    // Token 条目用紧凑单位显示（上亿数字直接铺开会撑破表格）。
    const cellNum = (v) => (it.unit === "token" ? fmtTokens(v) : v);
    html += `<tr${cls}><td>${esc(it.name)}${tag}</td><td>${cellNum(it.total)}</td><td>${cellNum(it.used)}</td><td>${cellNum(it.remain)}</td>`;
    if (hasExpiry) html += `<td${expCellCls}>${expCell}</td>`;
    html += `</tr>`;
  }
  html += `</tbody></table>`;

  // 小计行：只在确实存在不可用额度时才拆开展示，否则保持单数字（不制造无意义的 0）。
  const usable = d.usable_remain || 0;
  const unusable = d.unusable_remain || 0;
  html += `<div class="detail-sum">`;
  html += `<span>可用 <b>${usable}</b></span>`;
  if (unusable > 0) html += `<span class="detail-sum-unusable">不可用 <b>${unusable}</b></span>`;
  html += `</div>`;
  tip.innerHTML = html;
  tip.style.display = "block";

  // 翻页：只改状态重绘，不重新请求接口。关闭路径由 bindCreditDetailDismiss 统一接管。
  tip.querySelectorAll(".detail-pg").forEach((btn) => {
    if (btn.classList.contains("off")) return;
    btn.onclick = (ev) => {
      ev.stopPropagation();
      const target = Number(btn.dataset.pg);
      if (Number.isFinite(target)) { detailState.page = target; renderCreditDetail(); }
    };
  });
}
