"use strict";
// ---------- 首页仪表盘（R129）----------
// 首页 = 指标卡 + 三场景摘要 + 用量环形 + 运行统计（原有 mtabStats 全部保留）。
// 设计依据（TP-002 v3）：第一屏必须回答「现在落到谁、还剩多少、今天要做什么」。
// 指标卡的数字全部来自既有 `/api/stats` 与 `/api/state` 字段，**不新增任何后端字段**；
// 唯一缺的是「模型最近使用时刻」，那是 R130 的事（此处不显示相对时间，避免编造）。

// renderHomeCards 顶部 6 张指标卡。全部实字段，缺数据时显示「—」而不是 0。
function renderHomeCards() {
  const box = $("homeCards");
  if (!box || !state) return;
  const accs = state.accounts || [];
  const st = lastStats || {};
  const tu = st.token_usage || {};
  const signed = accs.filter((a) => a.last_checkin_ok).length;
  const na = accs.filter((a) => a.credits_na).length;
  const canSign = accs.length - na;
  const cards = [
    { l: "账号总数", v: String(accs.length),
      s: `已签到 ${signed}/${canSign}${na ? ` · 不适用 ${na}` : ""}`,
      cls: signed >= canSign ? "ok" : "warn" },
    { l: "今日获得积分", v: st.credit_in != null ? `+${fmtCredits(st.credit_in)}` : "—", s: "签到与每日发放", cls: "ok" },
    { l: "今日消耗积分", v: st.credit_used != null ? fmtCredits(st.credit_used) : "—",
      s: st.reqs != null ? `${fmtCredits(tu.req_today || 0)} 次请求 · 作废 ${fmtCredits(st.credit_expired || 0)}` : "—", cls: "warn" },
    { l: "总积分", v: st.total_credits != null ? fmtCredits(st.total_credits) : "—",
      s: `${new Set(accs.map((a) => a.group)).size} 个渠道`, cls: "" },
    { l: "即将过期", v: st.expiring_7d != null ? fmtCredits(st.expiring_7d) : "—",
      s: `今日 ${fmtCredits(st.expire_today || 0)} · 明日 ${fmtCredits(st.expire_tomorrow || 0)}`,
      cls: (st.expire_today || 0) > 0 ? "bad" : "" },
    { l: "Token 消耗", v: tu.days7 != null ? fmtTokensFull(tu.days7) : "—",
      s: tu.all != null ? `本周 · 累计 ${fmtTokensFull(tu.all)} / ${fmtCredits(tu.req_all || 0)} 次` : "—", cls: "" },
  ];
  box.innerHTML = cards.map((c) =>
    `<div class="kcard"><div class="k-l">${esc(c.l)}</div>` +
    `<div class="k-v ${c.cls}">${esc(c.v)}</div><div class="k-s">${esc(c.s)}</div></div>`).join("");
}

// renderHomeRecent 「最近在用」列表（R130，数据来自 /api/stats 的 recent_models）。
//
// 为什么需要它：用户的原话是「查看最近在用的模型」——旧面板只有「今日按模型消耗」
// 排行，回答不了「我昨天那个活跑的是哪个模型」。该字段后端跨天保留，故今早还没
// 跑任何请求时也能看到昨天最后在用的是什么。
//
// 口径注意：reqs/tokens 是**今日**的（跨天清零），LastTS 是**绝对时刻**（跨天保留）。
// 两者口径不同，故今日没跑的模型必须显示「今日未用」而不是显示 0——那是两回事。
function renderHomeRecent() {
  const box = $("homeRecent");
  const sec = $("homeRecentSec");
  if (!box) return;
  const rows = (lastStats && lastStats.recent_models) || [];
  if (!rows.length) {
    // 一条都没有 = 这个实例还没成功记账过任何请求。显示空态而不是隐藏整块，
    // 否则用户会以为是面板坏了。
    if (sec) sec.classList.remove("hidden");
    box.innerHTML = '<div class="hint" style="padding:4px 2px">还没有成功记账的请求</div>';
    return;
  }
  if (sec) sec.classList.remove("hidden");
  box.innerHTML = rows.map((r) => {
    const ch = r.model.split("/")[0];
    const today = r.reqs ? `<span class="rm-tok">${fmtTokensFull(r.tokens || 0)}</span>
        <span class="rm-req">${r.reqs} 次</span>`
      : '<span class="rm-idle">今日未用</span>';
    return `<div class="rm-row">
      <span class="ch" style="--c:var(--ch)">${esc(chLabel(ch))}</span>
      <code title="${esc(r.model)}">${esc(r.model.split("/").slice(1).join("/") || r.model)}</code>
      ${today}
      <span class="rm-ago" title="${esc(fmtClock(r.last_ts))}">${esc(relTime(r.last_ts))}</span>
    </div>`;
  }).join("");
}

// relTime unix 秒 → 相对时间。只用到「分钟级」，且过了 24h 就退回绝对日期——
// 「37 小时前」不如「10-28」有用（人读相对时间只在当天有效）。
function relTime(ts) {
  if (!ts) return "—";
  const sec = Math.floor(Date.now() / 1000) - ts;
  if (sec < 0) return "刚刚";        // 时钟漂移不显示「-3 分钟前」
  if (sec < 60) return "刚刚";
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`;
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时前`;
  return fmtClock(ts).split(" ")[0] || "—";
}

// fmtClock unix 秒 → "MM-DD HH:MM"（本地时区，用于悬浮提示与超过 24h 的回落显示）。
function fmtClock(ts) {
  if (!ts) return "—";
  const d = new Date(ts * 1000);
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// renderHomeScenes 三场景摘要（只读）。编辑在「模型管理」页，这里只回答「现在落到谁」。
// 备胎链只列前 3 个（更多没有决策价值，看全链去模型管理页）。
function renderHomeScenes() {
  const box = $("homeScenes");
  if (!box || !state) return;
  // 用 data-goto + 容器上的 onclick 委托，而不是给按钮起 id：动态生成的节点若用 id，
  // 「$() 悬空引用」扫描会把它当成指向不存在 DOM。onclick 赋值天然幂等（重复赋值只替换），
  // 不必再维护「绑定过没」的标记（那还得碰 dataset，虚假 DOM 里不一定有）。
  box.onclick = (e) => {
    const b = e.target && e.target.closest && e.target.closest("[data-goto]");
    if (b) showMainTab(b.dataset.goto);
  };
  const rules = sceneNormalize(state.compat, state.model_fallback);
  const acc = state.accounts || [];
  const byGroup = {};
  for (const a of acc) (byGroup[a.group] = byGroup[a.group] || []).push(a);
  const rows = SCENE_ALIASES.map(({ alias, label, desc }) => {
    const r = rules[alias] || { from: "", to: [] };
    const ch = (r.from || "").split("/")[0];
    const pool = byGroup[ch] || [];
    const usable = pool.filter((a) => !a.disabled && !a.cooling).length;
    const tos = (r.to || []).slice(0, 3)
      .map((t, i) => `<span class="hs-pill"><b>${i + 1}</b>${esc(t)}</span>`).join("");
    const more = (r.to || []).length > 3 ? `<span class="hs-more">+${(r.to || []).length - 3}</span>` : "";
    return `<div class="hs-card">
      <div class="hs-head"><span class="hs-tag">${esc(alias)}</span><b>${esc(label)}</b>
        <span class="hs-desc">${esc(desc)}</span></div>
      <div class="hs-main"><code>${esc(r.from || "（未配置）")}</code>
        <span class="hs-pool ${usable ? "ok" : "bad"}">可用 ${usable}/${pool.length}</span></div>
      <div class="hs-chain"><span class="hs-lbl">备胎</span>${tos || '<span class="hint">（无）</span>'}${more}</div>
    </div>`;
  }).join("");
  box.innerHTML = `<div class="sec-head"><h2>三场景落点</h2>
    <span class="hint">面板配置别名背后的主模型与备胎顺序</span>
    <button class="btn small" data-goto="models">去调整</button></div>
    <div class="hs-grid">${rows}</div>`;
}

// 主面板 tab 切换（首页 / 账号管理 / 模型管理 / 其他）—— R129 四 Tab 重构
// 结构变化要点：
//   · 首页 = 指标卡 + 三场景摘要 + 用量（环形）+ 运行统计（含临期/接棒/日志）
//   · 账号管理 = 原首页内容（不再有「全部签到」，签到入口统一收在首页）
//   · 模型管理 = 三场景与降级链编辑器 + 模型路由/思考强度 + 费率清单
//   · 其他 = 原设置弹层的 7 组（设置不再是独立页面/弹层，改成本页分组）
// 为什么 settingsOverlay 被删：设置是常驻配置，用弹层会让「改一项要开一次弹窗再关」；
// 弹层仍保留给「中断式任务」（添加账号、自定义渠道、模型导入、确认框）。
const MAIN_TABS = ["home", "accounts", "models", "other"];
function showMainTab(name) {
  const t = MAIN_TABS.includes(name) ? name : "home";
  document.querySelectorAll(".main-tabs .mtab").forEach((x) => {
    x.classList.toggle("active", x.dataset.mtab === t);
  });
  const map = { home: "mtabHome", accounts: "mtabAccounts", models: "mtabModels", other: "mtabOther" };
  for (const k of MAIN_TABS) {
    const el = $(map[k]);
    if (el) el.classList.toggle("hidden", k !== t);
  }
  closeCreditDetail(); // 切 tab 强制收起积分明细浮窗，避免残留浮在其他页签上（R129）
  if (t === "home") {
    loadUsage();  // 用量/环形与指标卡同屏，进首页即刷新
    loadStats();  // 运行统计（含临期、接棒、日志）
    fitPlatCols();
  }
  if (t === "models") {
    // 三场景/降级链编辑器现在挂在「模型管理」页（R129：从设置弹层搬出）。
    // 推荐候选要用费率与额度，缺才拉；编辑器本身先渲染，不阻塞在接口上。
    if (!lastFees) { loadFees().catch(() => {}); }
    if (!lastStats) { loadStats().catch(() => {}); }
    ensureSceneEditor();
  }
  if (t === "other") syncOtherNav();
}

// ensureSceneEditor 门车型渲染：只有「还没渲染过」才灌数据。
// 为什么不能无条件调 openSceneEditor()：它会把 edit 态重置成服务端值——用户正在改
// 三场景时，任何一次统计刷新都会把编辑冲掉（与 SR-003 的 G1 同族坑）。
// 但「首帧 state 还没到」时又必须补渲染，否则模型管理页永远空白。
function ensureSceneEditor() {
  if (!state) return;
  const box = $("sceneCards");
  if (!box) return;
  if (box.innerHTML.trim()) return;   // 已有内容 = 渲染过（或正在编辑），不覆盖
  openSceneEditor().catch(() => {});
}

function bindMainTabs() {
  document.querySelectorAll(".main-tabs .mtab").forEach((b) => {
    b.onclick = () => showMainTab(b.dataset.mtab);
  });

  bindFeesTabs(); // 费率面板渠道标签切换（事件委托一次绑定）
  bindSummaryFilter(); // 积分汇总条 chip 点击筛选账号（事件委托一次绑定）
}

// syncOtherNav 「其他」页侧导航：点条目滚到对应 cfg-section 并高亮。
// 分组标题即原设置的 cfg-heading，滚动目标就是各 section 自身（id 未变）。
function syncOtherNav() {
  const nav = $("otherNav");
  if (!nav) return;
  const links = [...nav.querySelectorAll("a")];
  links.forEach((a) => {
    a.onclick = () => {
      const el = $(a.dataset.sec);
      if (el) el.scrollIntoView({ block: "start", behavior: "smooth" });
      links.forEach((x) => x.classList.toggle("on", x === a));
    };
  });
  if (!links.some((x) => x.classList.contains("on")) && links[0]) links[0].classList.add("on");
}

// 面板 tab / 范围切换事件（bind 末尾调用）
function bindUsage() {
  $("btnRefreshUsage").onclick = loadUsage;
  $("usageRange").querySelectorAll(".seg-btn").forEach((b) => {
    b.onclick = () => {
      usageDays = parseInt(b.dataset.days, 10);
      persistUsageDays(); // 记住选择，刷新不重置
      syncUsageRangeActive(); // 单一入口同步高亮
      loadUsage();
    };
  });
  syncUsageRangeActive(); // 初始高亮按恢复的 usageDays（localStorage 命中「今日」不显示 7日 高亮）
  document.querySelectorAll(".tabs .tab").forEach((b) => {
    b.onclick = () => {
      document.querySelectorAll(".tabs .tab").forEach((x) => x.classList.toggle("active", x === b));
      $("tabToken").classList.toggle("hidden", b.dataset.tab !== "token");
      $("tabCredit").classList.toggle("hidden", b.dataset.tab !== "credit");
    };
  });
  $("pgPrev").onclick = () => { if (recentPage > 0) { recentPage--; renderRecentPage(lastNames); } };
  $("pgNext").onclick = () => { recentPage++; renderRecentPage(lastNames); };
}
