"use strict";
// ---------- 用量模块：环形 + 占比清单（R129，2026-10-29）----------
// 为什么不再用折线图（当日真实数据量化，不是口味问题）：
//   ① 今日 24 个桶只有 8 个非空，其中 7 个是"尚未到来的时刻" ⇒ 横向 2/3 空白；
//   ② 今日主力渠道 99.4%，另两条 0.6%/0.0% 贴 X 轴，肉眼不可见；
//   ③ 7 日 16 条模型线，前 3 名占 98.3% ⇒ 读图不如读表。
// 根因：折线表达"趋势"，而用户要问的是"谁在吃额度"（占比）。故改环形 + 占比清单。
// 同时第三方图表库整体下线（本面板仅此两处图表），不再依赖外网 CDN。
const TOP_SLICES = 5; // 前 5 名 + 「其他」= 6 档，正好用满明度梯

// donutSVG 画环形。items 已按值降序；每段留 gap 度间隙便于分辨边界。
function donutSVG(items, size, thick) {
  const total = items.reduce((a, b) => a + b.v, 0) || 1;
  const r = (size - thick) / 2, cx = size / 2, cy = size / 2, gap = 2.2;
  let ang = -90;
  const segs = items.map((it, i) => {
    const sweep = (it.v / total) * 360;
    const sw = Math.max(0, sweep - gap);
    const a0 = ang, a1 = ang + sweep;
    ang = a1;
    const p = (a) => [cx + r * Math.cos((a * Math.PI) / 180), cy + r * Math.sin((a * Math.PI) / 180)];
    const [x0, y0] = p(a0), [x1, y1] = p(a1);
    const large = sw > 180 ? 1 : 0;
    return `<path d="M ${x0.toFixed(2)} ${y0.toFixed(2)} A ${r} ${r} 0 ${large} 1 ${x1.toFixed(2)} ${y1.toFixed(2)}"
      fill="none" stroke="${rampColor(i)}" stroke-width="${thick}"/>`;
  }).join("");
  return `<svg class="donut" viewBox="0 0 ${size} ${size}" width="${size}" height="${size}" aria-hidden="true">
    <circle cx="${cx}" cy="${cy}" r="${r}" fill="none" stroke="var(--raised)" stroke-width="${thick}"/>${segs}</svg>`;
}

// aggSlices 把已排序的 [{name, v, meta}] 收敛成「前 TOP_SLICES + 其他」。
// 「其他」的 v 是**剩余项之和**（不是逐个画），meta=null 表示聚合项。
function aggSlices(sorted) {
  const head = sorted.slice(0, TOP_SLICES);
  if (sorted.length <= TOP_SLICES) return head;
  const rest = sorted.slice(TOP_SLICES);
  return head.concat([{
    name: `其他 ${rest.length} 个`, v: rest.reduce((a, b) => a + b.v, 0), meta: null,
  }]);
}

// renderUsageDonut 用量环形（token 维度）。数据源 /api/usage 的 by_model。
function renderUsageDonut(st) {
  const box = $("usageDonut"), list = $("usageShare"), foot = $("usageFoot");
  if (!box || !list) return;
  const rows = (st.token.by_model || []).slice().sort((a, b) => (b.pt + b.ct) - (a.pt + a.ct));
  const total = rows.reduce((a, r) => a + r.pt + r.ct, 0);
  const slices = aggSlices(rows.map((r) => ({
    name: `${r.channel}/${r.model}`, v: r.pt + r.ct, meta: r,
  })));
  box.innerHTML = donutSVG(slices, 196, 28);
  const tv = $("udTotal"), tl = $("udLabel");
  if (tv) tv.innerHTML = fmtTokensFull(total);
  if (tl) tl.textContent = `${usageDays === 1 ? "今日" : usageDays + " 日"} Token 消耗`;
  list.innerHTML = sliceRows(slices, total, "token");
  if (foot) foot.textContent = "饼图与清单同源（/api/usage by_model）。「未上报」= 上游不提供该维度，不是 0%。";
}

// renderCreditDonut 积分维度复用同一结构，只换数据源（按渠道聚合 spend）。
function renderCreditDonut(entries) {
  const box = $("creditDonut"), list = $("creditShare"), foot = $("creditFoot");
  if (!box || !list) return;
  const byCh = {};
  for (const e of entries || []) {
    if (e.kind !== "spend") continue;
    byCh[e.channel] = (byCh[e.channel] || 0) + -e.amount;
  }
  const rows = Object.keys(byCh).map((ch) => ({ name: chLabel(ch), v: byCh[ch], meta: { ch } }))
    .filter((r) => r.v > 0).sort((a, b) => b.v - a.v);
  const total = rows.reduce((a, r) => a + r.v, 0);
  const slices = aggSlices(rows);
  box.innerHTML = donutSVG(slices, 196, 28);
  const tv = $("cdTotal"), tl = $("cdLabel");
  if (tv) tv.textContent = fmtCredits(total);
  if (tl) tl.textContent = `${usageDays === 1 ? "今日" : usageDays + " 日"}积分消耗`;
  list.innerHTML = sliceRows(slices, total, "credit");
  if (foot) foot.textContent = "积分口径来自 ledger 流水差分，与 /api/stats 的 token 口径并列（R43，不合并不互校）。";
}

// sliceRows 占比清单。token 维度带缓存命中（本模块最有行动价值的一列：
// 实测「吃 64% token 的模型复用率只有 12%」直接解释额度掉得快）；积分维度带条目数。
function sliceRows(slices, total, mode) {
  const head = mode === "token"
    ? `<div class="mv-row mv-head"><span class="mv-sw"></span><span class="mv-name">模型</span>
        <span class="mv-bar"></span><span class="mv-pct">占比</span>
        <span class="mv-a">缓存命中</span><span class="mv-b">Token</span><span class="mv-c">请求</span></div>`
    : `<div class="mv-row mv-head"><span class="mv-sw"></span><span class="mv-name">渠道</span>
        <span class="mv-bar"></span><span class="mv-pct">占比</span>
        <span class="mv-a"></span><span class="mv-b">积分</span><span class="mv-c"></span></div>`;
  const body = slices.map((s, i) => {
    const pct = total ? (s.v / total) * 100 : 0;
    let a = "", b = "", c = "", nm = s.name;
    if (mode === "token") {
      const r = s.meta;
      if (r && r.cr_cov) a = `<b class="hit">${Math.round((r.cr / r.cr_cov) * 100)}%</b>`;
      else a = `<span class="dim">${r ? "未上报" : "—"}</span>`;
      b = fmtTokensFull(s.v);
      c = r ? String(r.requests) : "—";
    } else {
      b = fmtCredits(s.v);
      c = String((s.meta && s.meta.ch) || "");
    }
    return `<div class="mv-row"><span class="mv-sw" style="background:${rampColor(i)}"></span>
      <span class="mv-name" title="${esc(nm)}">${esc(nm)}</span>
      <span class="mv-bar"><i style="width:${Math.max(pct, 0.4).toFixed(1)}%;background:${rampColor(i)}"></i></span>
      <span class="mv-pct">${pct.toFixed(1)}%</span>
      <span class="mv-a">${a}</span><span class="mv-b">${b}</span><span class="mv-c">${c}</span></div>`;
  }).join("");
  return head + body;
}
