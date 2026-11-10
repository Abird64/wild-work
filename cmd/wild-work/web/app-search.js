"use strict";
// ---------- 三场景推荐排序（SR-005a：纯函数，零 DOM / 零全局态 / 零请求） ----------
// 规格：SR-008 §二。核心前提是「积分都是免费领的」⇒ 金钱维不进排序；
// rate 只用于三态识别与同分裁决，不参与主排序（SD-10 已裁）。
//
// 四态（互斥）：FREE 零消耗 / PRICED 速率已知 / UNPRICED 消耗未知 / UNAVAILABLE 已摘出路由。
// ⚠️ UNPRICED（priced=false && free=false）绝不可当免费处理：本刻 242 模型里 139 个属此态，
//    若当 0 费率排前，会得到一批「最便宜」的假首选（SR-002 §5.1.1 已实证 5 个 trae 模型从非零变 0 属此态）。

// 账号池归并：traework 与 traecode 共用同一个 Trae 账号（/api/state accounts 里 traecode 为 0 个）。
// 不归并会让 traecode 判「余额 0」被整渠道排除 —— 而它是 code 场景唯一 1M 上下文的入口（SD-15 裁决）。
const RANK_POOL = { traework: "trae", traecode: "trae" };
// 已摘出全部路由的渠道（MS-007：workbuddy 因 11128 风险摘除）。属可用性硬门槛，不是价格判断。
const RANK_UNROUTED = { workbuddy: true };

// 场景权重（SR-008 §2.4）。W = 额度维：余额越多越该用（0.75）＋ 快到期的额外加分（0.25）。
// R165 收编：可编辑场景只剩 custom。它的权重沿用原 complex（custom 的链就是从 complex 继承的：
// P 性能 / C 能力 / W 余额三分，能力权重最高——"自定义"是一次手写选择，值得为能力让一点速度）。
// ⚠️ 场景名改了就**必须**同步这张表：查不到权重时 rankCandidates 直接返回空数组
// （表现为"推荐列表一片空白"而不是报错，很难看出原因）。
const RANK_WEIGHTS = {
  custom: { P: 0.30, C: 0.50, W: 0.20 },
  // legacy 两键保留：老面板截图/文档里的 code/complex 仍可能被手工调用（推荐列表 tab 已只剩 custom）。
  // 删掉它们不会报错，只会让任何传旧名的调用静默返回空数组——留着的成本是零。
  code:    { P: 0.35, C: 0.45, W: 0.20 },
  complex: { P: 0.30, C: 0.50, W: 0.20 },
};

function rankPoolOf(ch) { return RANK_POOL[ch] || ch; }

function rankNum(v) {
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}

// rankCandidates 纯函数：输入 fees/stats（GET 响应）、scene、candidatesData（SR-002 JSON），
// 输出有序数组。不读 DOM、不读 state、不发请求、不改入参（内部只读）。
function rankCandidates(fees, stats, scene, candidatesData) {
  const w = RANK_WEIGHTS[scene];
  if (!w) return [];

  // --- 索引活值 ---
  const feeById = {};
  for (const ch of (fees && fees.channels) || []) {
    for (const m of ch.models || []) {
      const id = m.id || (ch.channel + "/" + m.model);
      feeById[id] = m;
    }
  }
  const statByModel = {};
  for (const sm of (stats && stats.models) || []) if (sm.model) statByModel[sm.model] = sm;

  // --- 账号池余额（先归并，再归一） ---
  const poolBal = {}, poolExp = {};
  for (const p of (stats && stats.platforms) || []) {
    const pool = rankPoolOf(p.group);
    poolBal[pool] = (poolBal[pool] || 0) + (rankNum(p.credits) || 0);
    poolExp[pool] = (poolExp[pool] || 0) + (rankNum(p.expire_7d) || 0);
  }
  const maxBal = Math.max(1, ...Object.values(poolBal));

  // --- 候选池（来自 SR-002 数据文件，顺序不作推荐承诺） ---
  const sc = ((candidatesData && candidatesData.scenes) || []).find((x) => x.alias === scene);
  const poolIds = ((sc && sc.candidates) || []).map((c) => c.id).filter(Boolean);

  const out = [];
  for (const id of poolIds) {
    const ch = id.split("/")[0];
    const fm = feeById[id] || null;
    const missing = [];
    if (!fm) missing.push("fees");
    const sm = statByModel[id] || null;
    if (!sm) missing.push("stats");

    // --- 四态判定（互斥，顺序即优先级：摘出路由 > 免费 > 已定价 > 未知） ---
    let state;
    if (RANK_UNROUTED[ch]) state = "UNAVAILABLE";
    else if (fm && fm.free === true) state = "FREE";
    else if (fm && fm.priced === true) state = "PRICED";
    else state = "UNPRICED"; // 含「目录里查不到」——同样按未知处理，绝不当免费

    if (state === "UNAVAILABLE") continue; // 可用性是硬门槛，直接排除

    // --- P 性能：无足够实测 ⇒ null（不得当 0 分，也不得当满分） ---
    let pScore = null, perf = null;
    const samples = sm ? rankNum(sm.perf_samples) : null;
    const ttfb = sm ? rankNum(sm.avg_ttfb_ms) : null;
    const tps = sm ? rankNum(sm.avg_tok_per_sec) : null;
    if (samples !== null && samples >= 10 && (ttfb !== null || tps !== null)) {
      const pTtfb = ttfb !== null && ttfb > 0 ? Math.max(0, Math.min(1, (12000 - ttfb) / 9000)) : null;
      const pTps = tps !== null && tps > 0 ? Math.max(0, Math.min(1, tps / 200)) : null;
      if (pTtfb !== null && pTps !== null) pScore = pTtfb * 0.6 + pTps * 0.4;
      else pScore = pTtfb !== null ? pTtfb : (pTps !== null ? pTps : null);
      perf = { ttfb, tps, samples };
    }
    if (samples !== null && samples < 10) missing.push("perf_samples");

    // --- C 能力（按场景） ---
    const ctx = fm ? rankNum(fm.context_window) : null;
    const tools = fm ? fm.supports_tools : null;
    const rsn = fm ? fm.supports_reasoning : null;

    let cScore = null;
    if (scene === "code") {
      // tools 是硬条件：未采集/未声明一律淘汰（SR-001 §2 I4 家族；MS-001 §7.2）
      if (tools !== true) continue;
      cScore = 0.5 + (ctx !== null && ctx >= 128000 ? 0.5 : 0);
    } else if (scene === "complex" || scene === "custom") {
      let acc = 0, wsum = 0;
      if (ctx !== null) { acc += 0.6 * (ctx >= 500000 ? 1 : ctx >= 128000 ? 0.6 : 0.3); wsum += 0.6; }
      if (rsn !== null) { acc += 0.4 * (rsn ? 1 : 0); wsum += 0.4; }
      cScore = wsum > 0 ? acc / (0.6 + 0.4) : null;
      if (ctx === null) missing.push("context_window");
    } else {
      // 其它（历史 fast 名）：能力只作门槛（ctx >= 128k），不进评分（否则 1M 上下文白送分）
      cScore = ctx !== null ? (ctx >= 128000 ? 1 : 0.4) : 0.5;
      if (ctx === null) missing.push("context_window");
    }

    // --- W 额度：余额越多越优先（用户原则）+ 到期压力 ---
    const pool = rankPoolOf(ch);
    const bal = poolBal[pool] || 0;
    const exp = poolExp[pool] || 0;
    const wScore = bal <= 0 ? 0 : 0.75 * (bal / maxBal) + 0.25 * Math.min(1, exp / bal);

    // --- 覆盖惩罚（沿用 MS-001 §7.1）：只对已观测维度加权，再按观测比例折减 ---
    const parts = [];
    if (pScore !== null && w.P) parts.push([pScore, w.P]);
    if (cScore !== null && w.C) parts.push([cScore, w.C]);
    if (w.W) parts.push([wScore, w.W]);
    if (!parts.length) continue;
    const wAvail = parts.reduce((a, x) => a + x[1], 0);
    const wTotal = w.P + w.C + w.W;
    const score = (parts.reduce((a, x) => a + x[0] * x[1], 0) / wAvail) * (wAvail / wTotal);

    // --- T 分级（防「渠道余额多但没测过」靠 W 维挤掉实测模型） ---
    // T1 有实测且能力门槛满足 / T2 有实测但能力维缺失 / T3 无实测（排全部 T1/T2 之后）
    let tier;
    if (pScore === null) tier = 3;
    else if (cScore === null) tier = 2;
    else tier = 1;

    out.push({
      id, channel: ch, state, tier, score: Math.round(score * 10000) / 10000,
      rate: fm ? fm.rate : null, free: !!(fm && fm.free), priced: !!(fm && fm.priced),
      ctx, tools, reasoning: rsn,
      w: Math.round(wScore * 10000) / 10000, // 额度分量（供 UI 展示与回归断言；不参与 evidence 权重）
      evidence: null, // 由 UI 从 candidatesData 取；此处只作占位，严禁参与排序（SD-2）
      missing,
    });
  }

  // 排序：状态分层 → T 升序 → 分数降序 → id 升序。
  // ⚠️ 第一键「状态分层」不是可选的：UNPRICED 的额度维可能极高（大池子），
  //    若只按 T+分数排，它会靠 W 维挤到 PRICED 前面 —— 而它「花多少分未知」，
  //    把它排在 PRICED 之前等于向用户承诺一个无法核实的成本（实测已抓到过）。
  const STATE_RANK = { PRICED: 0, FREE: 1, UNPRICED: 2 };
  out.sort((a, b) =>
    ((STATE_RANK[a.state] ?? 3) - (STATE_RANK[b.state] ?? 3)) ||
    (a.tier - b.tier) || (b.score - a.score) ||
    (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  // 同时暴露已排序快照的顺序，供 UI 直接消费（避免二次排序口径漂移）
  return out;
}

// collectAllModels 展平全渠道模型，附带裸名/完整 ID（完整 ID = 渠道/模型，即客户端该填的名字）。
function collectAllModels() {
  const out = [];
  for (const ch of (lastFees && lastFees.channels) || []) {
    for (const m of ch.models || []) {
      const raw = m.model;
      const short = raw.includes("/") ? raw.slice(raw.indexOf("/") + 1) : raw;
      // 完整 ID 一律取服务端给的 m.id（与 /v1/models、provider.PublicModelID 同源）；
      // 缺字段时退回旧拼接规则（兼容旧后端缓存）：上游 ID 自带命名空间（cline-free/*）
      // 的渠道由服务端直接透传上游 ID，不再补 `<渠道>/` 前缀（R59）。
      const full = m.id || (ch.channel && !raw.startsWith(`${ch.channel}/`) ? `${ch.channel}/${raw}` : raw);
      out.push({ ch: ch.channel, m, short, full });
    }
  }
  return out;
}

function openModelSearch() {
  $("modelSearchOverlay").classList.remove("hidden");
  const inp = $("modelSearchInput");
  inp.value = "";
  renderModelSearch("");
  inp.focus();
}
function closeModelSearch() { $("modelSearchOverlay").classList.add("hidden"); }

// renderModelSearch 按关键字过滤全渠道模型并渲染（对话框固定高度，结果在 .ms-list 内滚动）。
// 关键字匹配完整 ID 或裸名（大小写不敏感）——两处都包含渠道名，故搜渠道亦可。
function renderModelSearch(q) {
  const kw = String(q || "").trim().toLowerCase();
  const all = collectAllModels();
  const hit = kw ? all.filter((x) => x.full.toLowerCase().includes(kw) || x.short.toLowerCase().includes(kw)) : all;
  const box = $("modelSearchList");
  if (!hit.length) {
    box.innerHTML = `<div class="ms-empty">${all.length ? "无匹配模型" : "费率数据尚未加载，请点「刷新」后再试"}</div>`;
  } else {
    box.innerHTML = hit.map(({ ch, m, short, full }) => {
      const caps = [m.supports_images ? "👁" : "", m.supports_reasoning ? "🧠" : ""].join("");
      // 档位小字（issue #96）：上游声明才显示
      const eff = (m.reasoning_efforts && m.reasoning_efforts.length)
        ? `<span class="ms-eff" title="思考档位：${esc(m.reasoning_efforts.join("·"))}${m.default_reasoning_effort ? `（默认 ${m.default_reasoning_effort}）` : ""}">${esc(m.reasoning_efforts.join("·"))}</span>` : "";
      const ctx = (m.has_context && m.context_window)
        ? `<span title="上下文窗口：${esc(fmtTokensCompact(m.context_window))} tokens（${m.hand_measured ? "本工具实测，仅供参考" : "上游接口返回"}）">(${esc(fmtTokensCompact(m.context_window))}${m.hand_measured ? "*" : ""})</span>` : "";
      const rate = m.free ? "Free" : (m.priced ? `x${m.rate.toFixed(2)}` : "unknown");
      return `<div class="ms-row" data-mid="${esc(full)}" role="button" tabindex="0" title="点击复制完整模型 ID：${esc(full)}">`
        + chBadge(ch)
        + `<span class="ms-name">${esc(short)}</span>`
        + `<span class="ms-tags">${ctx}${caps}${eff}</span>`
        + `<span class="ms-rate">${esc(rate)}</span>`
        + `</div>`;
    }).join("");
  }
  // 底部提示：命中数（结果全量渲染，列表本身可滚动，故不再提示「未显示」）。
  $("modelSearchFoot").textContent = !all.length ? ""
    : (hit.length === 0 ? "无匹配" : `共 ${hit.length} 个匹配`);
}

// ---------- 配置区模糊搜索下拉（combobox）：模型ID / 思考档位输入框复用「查找模型」数据 ----------
// attachModelPicker 把 <input> 升级为可搜索下拉：focus/input 渲染候选（数据源与「查找模型」
// 弹层同源——lastFees 缓存展平，字段与文案对齐 renderModelSearch），↑↓ 高亮、Enter 选中、
// Esc / 失焦 / 点击外部关闭；仍可自由手输（无匹配不强制选择，输入框即 combobox）。
// opts.onPick(id)：选中回调（缺省=仅回填并关菜单）；
// opts.enterAction(e)：菜单关闭时按回车的原行为（如「跳到下一个输入框 / 提交」）；
// opts.stayOpen：多选模式——点选后不关菜单、清空输入、刷新候选（配合 excludeSet 连续多选）；
// opts.multiAccum：累积多选——逗号分隔逐个追加，过滤关键字取末段、已累积段从候选隐藏；
// opts.excludeSet()：返回需在候选中完全隐藏的模型集合（完整 ID 精确等值 -> true）。
let pkOpenMenu = null;
function pkClose() {
  document.querySelectorAll(".pk-menu.open").forEach((m) => m.classList.remove("open"));
  pkOpenMenu = null;
}
// 点击任意输入框/菜单之外的区域关闭当前下拉（capture 保证先于其他 mousedown 处理器）
document.addEventListener("mousedown", (e) => {
  if (!pkOpenMenu) return;
  if (e.target.closest(".pk-wrap")) return;
  pkClose();
}, true);
// pkHl 命中段高亮（先分段转义再拼 <mark>，与 .ms-list 的 mark 样式共用）
function pkHl(text, kw) {
  const k = String(kw || "").trim().toLowerCase();
  if (!k) return esc(text);
  const i = String(text).toLowerCase().indexOf(k);
  if (i < 0) return esc(text);
  return esc(text.slice(0, i)) + "<mark>" + esc(text.slice(i, i + k.length)) + "</mark>" + esc(text.slice(i + k.length));
}
// pkItemHTML 候选项两行富信息：行1 渠道徽标 + 完整 ID（命中段高亮）+ 价格；
// 行2 上下文窗口（实测带 *）/ 能力图标 / 思考档位（含默认）——与 renderModelSearch 逐字对齐。
function pkItemHTML(x, kw) {
  const m = x.m;
  const caps = (m.supports_images ? "👁" : "") + (m.supports_reasoning ? "🧠" : "");
  const ctx = (m.has_context && m.context_window) ? `(${esc(fmtTokens(m.context_window))}${m.hand_measured ? "*" : ""})` : "";
  const eff = (m.reasoning_efforts && m.reasoning_efforts.length)
    ? esc(m.reasoning_efforts.join("·")) + (m.default_reasoning_effort ? `（默认 ${esc(m.default_reasoning_effort)}）` : "") : "";
  const rate = m.free ? "Free" : (m.priced ? `x${(+m.rate).toFixed(2)}` : "unknown");
  return `<div class="pk-item pk-rich" data-id="${esc(x.full)}" title="点击选用 ${esc(x.full)}">`
    + `<div class="pk-line1">${chBadge(x.ch)}<span class="id">${pkHl(x.full, kw)}</span><span class="rate">${esc(rate)}</span></div>`
    + `<div class="pk-line2">${ctx}${caps}${eff ? `<span class="pk-eff">${eff}</span>` : ""}</div></div>`;
}
function attachModelPicker(input, opts) {
  opts = opts || {};
  const wrap = input.closest(".pk-wrap");
  if (!wrap) return;
  let menu = wrap.querySelector(".pk-menu");
  if (!menu) { menu = document.createElement("div"); menu.className = "pk-menu"; wrap.appendChild(menu); }
  let hlIdx = -1;
  function render() {
    let kw, exclude;
    const baseExcl = opts.excludeSet ? opts.excludeSet() : null;
    if (opts.multiAccum) {
      // 累积多选：过滤关键字取末段（最后一个逗号后），已累积段从候选隐藏（精确等值）；
      // 同时合并 excludeSet（如主力模型ID全局排除），每次 render 实时取——主力列表变更即同步可选范围。
      const parts = String(input.value || "").split(/[,，]/);
      kw = (parts[parts.length - 1] || "").trim();
      exclude = baseExcl ? { ...baseExcl } : {};
      for (let k = 0; k < parts.length - 1; k++) { const seg = parts[k].trim(); if (seg) exclude[seg] = true; }
    } else {
      kw = input.value.trim();
      exclude = baseExcl;
    }
    const kwl = kw.toLowerCase();
    const all = collectAllModels();
    const items = all.filter((x) => {
      if (exclude && exclude[x.full]) return false;
      if (!kwl) return true;
      return x.full.toLowerCase().includes(kwl) || x.short.toLowerCase().includes(kwl);
    });
    hlIdx = -1;
    menu.innerHTML = items.length
      ? items.slice(0, 40).map((x) => pkItemHTML(x, kw)).join("")
      : `<div class="pk-empty">${all.length ? "无匹配模型（可继续手动输入）" : "费率数据尚未加载，请点「刷新」后再试"}</div>`;
    menu.classList.add("open");
    pkOpenMenu = menu;
  }
  function pick(id) {
    if (opts.stayOpen && !opts.multiAccum) {
      // 多选（点选即追加）：回调追加后清空输入继续搜；onPick 可能重建 DOM（重渲染），先判连接
      if (opts.onPick) opts.onPick(id);
      if (!input.isConnected) return;
      input.value = "";
      render();
      return;
    }
    if (opts.multiAccum) {
      // 累积多选：选中项替换末段并补逗号，光标留在新末段继续搜索/输入
      const parts = String(input.value || "").split(/[,，]/);
      parts[parts.length - 1] = id;
      input.value = parts.join(",") + ",";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      render();
      return;
    }
    input.value = id;
    input.dispatchEvent(new Event("input", { bubbles: true }));
    pkClose();
    if (opts.onPick) opts.onPick(id); else input.focus();
  }
  function moveHl(d) {
    const nodes = menu.querySelectorAll(".pk-item");
    if (!nodes.length) return;
    hlIdx = (hlIdx + d + nodes.length) % nodes.length;
    nodes.forEach((n, k) => n.classList.toggle("pk-hl", k === hlIdx));
    nodes[hlIdx].scrollIntoView({ block: "nearest" });
  }
  input.addEventListener("focus", render);
  input.addEventListener("click", render);
  input.addEventListener("input", render);
  input.addEventListener("keydown", (e) => {
    if (!menu.classList.contains("open")) { if (e.key === "Enter" && opts.enterAction) opts.enterAction(e); return; }
    // 菜单开着时按键由下拉消化，stopPropagation 防触发 #fbList 上的 Enter 委托（如「回车追加备胎」）
    if (e.key === "ArrowDown") { e.preventDefault(); e.stopPropagation(); moveHl(1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); e.stopPropagation(); moveHl(-1); }
    else if (e.key === "Escape") { pkClose(); }
    else if (e.key === "Enter") {
      e.preventDefault(); e.stopPropagation();
      const nodes = menu.querySelectorAll(".pk-item");
      if (hlIdx >= 0 && nodes[hlIdx]) pick(nodes[hlIdx].dataset.id);
      else if (nodes.length === 1) pick(nodes[0].dataset.id);
      else { pkClose(); if (opts.enterAction) opts.enterAction(e); }
    }
  });
  // 失焦即关（菜单项 mousedown 已 preventDefault，点菜单项不会先触发 blur）
  input.addEventListener("blur", () => setTimeout(pkClose, 120));
  menu.addEventListener("mousedown", (e) => e.preventDefault());
  menu.addEventListener("click", (e) => {
    const it = e.target.closest(".pk-item");
    if (it) pick(it.dataset.id);
  });
}

// 思考档位下拉（单选模糊搜索，交互与模型下拉一致；候选为常见档位，仍可自由输入）。
// 渠道支持什么档位由渠道模型自己决定（后端哑管道不校验），故候选只是输入建议。
const PK_EFFORTS = ["none", "low", "medium", "high", "xhigh", "max"];
function attachEffortPicker(input, opts) {
  opts = opts || {};
  const wrap = input.closest(".pk-wrap");
  if (!wrap) return;
  let menu = wrap.querySelector(".pk-menu");
  if (!menu) { menu = document.createElement("div"); menu.className = "pk-menu"; wrap.appendChild(menu); }
  let hlIdx = -1;
  function render() {
    const kw = input.value.trim().toLowerCase();
    const list = PK_EFFORTS.filter((t) => !kw || t.includes(kw));
    hlIdx = -1;
    menu.innerHTML = list.length
      ? list.map((t) => `<div class="pk-item" data-id="${t}" title="点击选用 ${t}"><span class="id">${pkHl(t, kw)}</span><span class="pk-ch-tag">档位</span></div>`).join("")
      : `<div class="pk-empty">无匹配档位（可自由输入）</div>`;
    menu.classList.add("open");
    pkOpenMenu = menu;
  }
  function pick(id) {
    input.value = id;
    input.dispatchEvent(new Event("input", { bubbles: true }));
    pkClose();
    if (opts.onPick) opts.onPick(id); else input.focus();
  }
  function moveHl(d) {
    const nodes = menu.querySelectorAll(".pk-item");
    if (!nodes.length) return;
    hlIdx = (hlIdx + d + nodes.length) % nodes.length;
    nodes.forEach((n, k) => n.classList.toggle("pk-hl", k === hlIdx));
  }
  input.addEventListener("focus", render);
  input.addEventListener("click", render);
  input.addEventListener("input", render);
  input.addEventListener("keydown", (e) => {
    if (!menu.classList.contains("open")) { if (e.key === "Enter" && opts.enterAction) opts.enterAction(e); return; }
    if (e.key === "ArrowDown") { e.preventDefault(); e.stopPropagation(); moveHl(1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); e.stopPropagation(); moveHl(-1); }
    else if (e.key === "Escape") { pkClose(); }
    else if (e.key === "Enter") {
      e.preventDefault(); e.stopPropagation();
      const nodes = menu.querySelectorAll(".pk-item");
      if (hlIdx >= 0 && nodes[hlIdx]) pick(nodes[hlIdx].dataset.id);
      else if (nodes.length === 1) pick(nodes[0].dataset.id);
      else { pkClose(); if (opts.enterAction) opts.enterAction(e); }
    }
  });
  input.addEventListener("blur", () => setTimeout(pkClose, 120));
  menu.addEventListener("mousedown", (e) => e.preventDefault());
  menu.addEventListener("click", (e) => {
    const it = e.target.closest(".pk-item");
    if (it) pick(it.dataset.id);
  });
}
