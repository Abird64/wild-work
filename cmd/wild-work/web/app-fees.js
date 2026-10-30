"use strict";

// feeChannelAccessHtml 费率页里「自定义渠道」的接入点一行（R119）：
// 面板被当成「模型接入点总表」用，需要一眼看到每个渠道走哪条线、用哪把 key。
// 内建渠道没有 base_url/api_key（endpoint 在代码里写死）⇒ 返回空串，不占位。
// key 只显示脱敏值：明文绝不随费率接口批量下发，只在用户点「复制」那一刻按需取（见 copyCustomKey）。
function feeChannelAccessHtml(ch) {
  if (!ch || !ch.base_url) return "";
  const key = ch.api_key_masked || "（未配置 key）";
  return `<div class="fee-access">
    <span class="fee-access-k">接入点</span><code class="fee-access-v fee-copy-url" role="button" tabindex="0"
      data-copy="${esc(ch.base_url)}" title="点击复制接入点：${esc(ch.base_url)}">${esc(ch.base_url)}</code>
    <span class="fee-access-k">api-key</span><code class="fee-access-v fee-copy-key" role="button" tabindex="0"
      data-ch="${esc(ch.channel)}" title="点击复制完整 api-key（此时才向后端取明文，不批量下发）">${esc(key)}</code>
    <!-- R122：「显示全部模型」开关——默认关（只列已添加的），打开则**立即**拉一次该渠道 /models，
         把上游全部模型（含能力）列在旁边，不写进配置。包在 span 里而不是写进 .switch：
         .switch 本身是 42x24 的定位盒子，把文字塞进去会被clip 掉 -->
    <span class="fee-all-wrap" title="打开后立即拉取该接入点的 /models，列出全部可添加模型与能力（只展示，不写入配置）">
      <label class="switch"><input type="checkbox" data-allmodels="${esc(ch.channel)}"${feeShowAll.has(ch.channel) ? " checked" : ""}><span class="slider"></span></label>
      <span class="fee-access-k">显示全部模型</span>
    </span>
  </div>`;
}

// feeShowAll / feeAllData：费率页「显示全部模型」的开关状态与拉取结果。
// renderFees 每 30s 随 loadFees 重渲染，这两个集合必须活在模块级，否则开关一刷新就丢。
const feeShowAll = new Set();
const feeAllData = {};

// feeAllBoxHtml 渲染拉取结果：已添加的标「已添加」，未添加的标「未添加」（只展示，
// 真要添加回自定义渠道面板点「＋ 模型」）。能力沿用 capSummaryHtml，不编造默认值。
function feeAllBoxHtml(chName) {
  const d = feeAllData[chName];
  if (!d) return `<div class="fee-all-box"><span class="hint">（正在拉取…）</span></div>`;
  if (d.err) return `<div class="fee-all-box"><span class="hint" style="color:var(--danger)">${esc(d.err)}</span></div>`;
  const rows = d.rows.map((r) => `<span class="fee-all-item${r.added ? " added" : ""}" title="${r.added ? "已添加，可直接使用" : "尚未添加：回「自定义模型」面板点「＋ 模型」加入"}">` +
    `<code>${esc(r.id)}</code>${capSummaryHtml(r.cap)}${r.added ? "" : "<span class=\"cm-meta cap-unknown\">未添加</span>"}</span>`).join("");
  return `<div class="fee-all-box"><div class="hint">共 ${d.rows.length} 个（点接入点行的开关可收起）</div>${rows}</div>`;
}

// toggleFeeAllModels 开关切换：打开时**才**发探测请求（费率页每 30s 轮询，不得把它变常开）。
// api_key 传脱敏串，后端按渠道名换成真值（与「列表永不收明文」同一口径）。
async function toggleFeeAllModels(chName, on) {
  if (!on) { delete feeAllData[chName]; feeShowAll.delete(chName); rerenderFees(); return; }
  feeShowAll.add(chName);
  const ch = (renderFees.last || {}).channels ? renderFees.last.channels.find((c) => c.channel === chName) : null;
  rerenderFees();
  if (!ch || !ch.base_url) { feeAllData[chName] = { err: "该渠道没有可探测的接入点" }; return void rerenderFees(); }
  try {
    const res = await api("/api/config/custom_channel_test", { name: chName, base_url: ch.base_url, api_key: ch.api_key_masked || "" });
    if (!res || !res.ok) {
      const tried = ((res && res.tries) || []).map((t) => `${t.url} → ${t.status || t.err || "?"}`).join("； ");
      feeAllData[chName] = { err: `拉取失败：${(res && res.error) || "上游未返回模型列表"}${tried ? `（试过：${tried}）` : ""}` };
    } else {
      // 费率行的 ID 带渠道前缀（`ch/model`，R52），而上游 /models 回的是裸名 ⇒ 比前先剥前缀
      const bare = (id) => (id && id.startsWith(chName + "/") ? id.slice(chName.length + 1) : id);
      const added = new Set((ch.models || []).map((m) => bare(m.id || m.model)));
      const list = res.caps && res.caps.length ? res.caps : (res.models || []).map((id) => ({ id }));
      feeAllData[chName] = { rows: list.map((c) => ({ id: c.id, cap: c.cap_source ? c : null, added: added.has(c.id) })) };
    }
  } catch (e) {
    feeAllData[chName] = { err: `拉取失败：${e.message}` };
  }
  if (feeShowAll.has(chName)) rerenderFees();
}

// rerenderFees 重画费率区（开关状态变了就得重画）：没有缓存过数据时什么都不做。
function rerenderFees() {
  if (renderFees.last) renderFees(renderFees.last);
}

// copyCustomKey 复制某个自定义渠道的**完整** api-key（R119）：面板平时只拿到脱敏串，
// 复制要的是可用凭据 ⇒ 只在用户点下去时才请求 /api/config/custom_channel_key 取一次明文。
// 明文只进剪贴板，不进 DOM、不缓存（与「零 token 落盘」同口径：能不见就不见）。
async function copyCustomKey(name) {
  try {
    const res = await api("/api/config/custom_channel_key", { name });
    if (!res || !res.ok || !res.api_key) { toast("该渠道未配置 api-key"); return; }
    copyText(res.api_key, `渠道 ${name} 的 api-key`);
  } catch (e) {
    toast(`取 api-key 失败：${e.message}`);
  }
}

function renderFees(fees) {
  const box = $("feesBox");
  const channels = fees.channels || [];
  renderFees.last = fees; // 开关重拉后要能原地重画

  if (channels.length === 0) {
    box.innerHTML = `<div class="note">${esc(fees.note || "")}</div>
      <div class="note">${esc(fees.disclaimer || "")}</div>`;
    return;
  }
  let html = `<div class="note">${esc(fees.note || "")}</div>`;
  if (fees.cached_at) html += `<div class="note">费率上次更新：${esc(fees.cached_at)}</div>`;
  if (fees.error) html += `<div class="note" style="color:var(--danger)">${esc(fees.error)}</div>`;

  const UNKNOWN_TIP = "上游未返回，请在客户端自行确认";

  // 能力图标：模型 ID 后的小标记，title 属性提供文字描述。
  // 只展示上游明确声明的能力；未声明的（字段缺失或上游返回 false）不显示图标。
  // tool_calls 不展示：几乎所有模型都支持，图标信息量低。
  const capIcons = (m) => {
    if (!m) return "";
    const caps = [];
    if (m.supports_images) {
      caps.push(`<span class="cap-icon cap-img" title="支持图像输入（多模态视觉）：可直接发送图片给该模型">👁</span>`);
    }
    if (m.supports_reasoning) {
      caps.push(`<span class="cap-icon cap-reason" title="支持思考/推理模式：回复前会进行推理（可能含 reasoning_content）">🧠</span>`);
    }
    return caps.length > 0 ? ` <span class="cap-icons">${caps.join("")}</span>` : "";
  };

  // 思考档位标记（issue #96）：模型 ID 后的档位小字标，如 [low·high·max]。
  // 只在上游目录声明时展示（reasoning_efforts 非空），不自行编造。
  const effortTag = (m) => {
    if (!m || !m.reasoning_efforts || !m.reasoning_efforts.length) return "";
    const list = m.reasoning_efforts.join("·") + (m.default_reasoning_effort ? `（默认 ${m.default_reasoning_effort}）` : "");
    return ` <span class="effort-tag" title="思考档位（上游目录声明）：${esc(list)}；客户端传 reasoning_effort 即可调档">[${esc(m.reasoning_efforts.join("·"))}]</span>`;
  };

  // 上下文标记：模型 ID 后的 (1M)/(180K) 小字标。
  // has_context = 上游接口真实返回 / 本工具手工实测 / 用户在面板手填（R119）。
  // tooltip 必须如实标出来源——不把实测/手填冒充成上游官方值，
  // 同时也不埋没它（cline 这类渠道上游根本不返回上下文，只能靠实测）。
  const CAP_SRC_TEXT = { api: "上游接口返回", measured: "本工具实测值（仅供参考，非上游官方声明）", user: "面板手填值（非上游声明）" };
  const ctxTag = (m) => {
    if (!m || !m.has_context || !m.context_window) return "";
    // cap_source 优先（后端统一口径）；老后端无该字段时退回 hand_measured 二选一
    const srcKey = m.cap_source || (m.hand_measured ? "measured" : "api");
    const src = CAP_SRC_TEXT[srcKey] || CAP_SRC_TEXT.api;
    const hand = srcKey === "api" ? "" : "*";
    return ` <span class="ctx-tag${hand ? " hand" : ""}" title="上下文窗口：${fmtTokensCompact(m.context_window)} tokens（${src}）">(${fmtTokensCompact(m.context_window)}${hand})</span>`;
  };

  // 能力文字摘要，拼进模型 tooltip。
  // 措辞说明：能力来源决定「不支持」这三个字能不能说出口（R49/R119）——
  // 上游未声明 ⇒ 只说「未声明」；实测/手填 ⇒ 说「实测不支持」/「手填未勾选」；
  // 压根没探测过 ⇒ 说「未探测」，绝不用默认值冒充结论。
  const capText = (m) => {
    if (!m) return null;
    const srcKey = m.cap_source || (m.hand_measured ? "measured" : "api");
    const noText = { api: "上游未声明", measured: "实测不支持", user: "手填未勾选", unknown: "未探测" }[srcKey] || "上游未声明";
    const yes = [], no = [];
    (m.supports_images ? yes : no).push("图像输入");
    (m.supports_reasoning ? yes : no).push("思考模式");
    const parts = [];
    if (yes.length) parts.push(`支持：${yes.join("、")}`);
    if (no.length) parts.push(`${noText}：${no.join("、")}`);
    if (m.reasoning_efforts && m.reasoning_efforts.length) {
      parts.push(`思考档位：${m.reasoning_efforts.join("/")}${m.default_reasoning_effort ? `（默认 ${m.default_reasoning_effort}）` : ""}`);
    }
    if (srcKey === "measured") parts.push("（能力与上下文为本工具实测值，仅供参考）");
    if (srcKey === "user") parts.push("（能力与上下文为您在面板手填，本工具不核实其真实性）");
    if (srcKey === "unknown") parts.push("（未探测过能力：可在「自定义模型」里点「导入」拉一次上游 /models）");
    return parts.join("；");
  };

  // 模型 id 的 tooltip：能拿到上下文则按来源展示，否则明确说未知
  const modelTip = (m) => {
    const parts = [`模型：${m.model}`];
    const srcKey = m.cap_source || (m.hand_measured ? "measured" : "api");
    if (m.has_context && m.context_window) {
      const src = srcKey === "api" ? "" : `（${CAP_SRC_TEXT[srcKey]}）`;
      parts.push(`上下文窗口：${fmtTokensCompact(m.context_window)}${src}`);
      if (m.max_tokens) parts.push(`最大输出：${fmtTokensCompact(m.max_tokens)}`);
    } else {
      parts.push("上下文窗口：未知");
      parts.push("最大输出：未知");
      parts.push(srcKey === "unknown" ? "（尚未探测上游，本工具不编造默认值）" : "（上游未提供该信息）");
    }
    const ct = capText(m);
    if (ct) parts.push(ct);
    return parts.join("\n");
  };

  // 单行倍率单元格
  const rateCell = (m) => {
    if (!m) return "";
    if (!m.priced) {
      return `<span class="rate-unknown" title="${esc(UNKNOWN_TIP)}">unknown</span>`;
    }
    if (m.free) {
      return `<span class="rate-free" title="上游标注为免费（x0.00）">✦ Free</span>`;
    }
    return `<span class="rate-paid">x${m.rate.toFixed(2)}</span>`;
  };

  // 促销标签：使用上游给的颜色（原本被拼在文案里没解析）
  const noteCell = (m) => {
    if (!m || !m.note) return "";
    const style = m.color ? ` style="color:${esc(m.color)}"` : "";
    return ` <span class="rate-note"${style}>${esc(m.note)}</span>`;
  };

  // 模型单元格（二开，升级勿丢）：上方为模型名 + 能力/上下文标记，下方为完整模型 ID
  // （含渠道前缀，即客户端添加模型时实际要填的模型名），点击一键复制到剪贴板。
  // 模型名本身也可点击复制完整 ID（官方 v2.6.1 交互），两处 data-mid 语义一致。
  // m.model 来自 /api/fees 是裸名（不带渠道前缀），完整 ID 需拼上 chPrefix（ch.channel）。
  const modelCellHtml = (m, chPrefix) => {
    if (!m) return "";
    const raw = m.model;
    // 短名：去掉渠道前缀（`xxx/name` -> `name`），展示在上行的模型名位置
    const short = raw.includes("/") ? raw.slice(raw.indexOf("/") + 1) : raw;
    // 完整 ID 取服务端给的 m.id（同 /v1/models 口径）：普通渠道是 `<渠道>/<模型>`；
    // 上游 ID 自带命名空间的渠道（cline-free/*）直接透传上游 ID（R59）。
    // 缺字段时退回旧拼接（兼容旧后端）：仅当裸名已以渠道前缀开头时原样保留（防重复）。
    const full = m.id || (chPrefix && !raw.startsWith(`${chPrefix}/`) ? `${chPrefix}/${raw}` : raw);
    const copyTip = `点击复制：${full}`;
    const line = `<div class="mline"><code class="fee-model-copy" data-mid="${esc(full)}" role="button" tabindex="0" title="${esc(modelTip(m))}\n${copyTip}">${esc(short)}</code>${ctxTag(m)}${effortTag(m)}${capIcons(m)}${noteCell(m)}</div>`;
    // 仅当完整ID != 短名时才显示可复制的完整 ID 行，避免裸名模型重复
    const idRow = full !== short
      ? `<span class="full-id" data-mid="${esc(full)}" title="点击复制完整模型 ID：${esc(full)}">${esc(full)}</span>`
      : "";
    return `<div class="mcell">${line}${idRow}</div>`;
  };

  // 渠道多标签：每个渠道一个 tab，panel 内双列模型布局不变（issue：费率表太长）。
  // 记住上次选中的渠道，重渲染后自动恢复（后台刷新不打断用户浏览）。
  if (!renderFees.lastCh) renderFees.lastCh = channels[0].channel;
  // 选中的渠道若已不在列表里（渠道被移除），回退到第一个
  if (!channels.some((c) => c.channel === renderFees.lastCh)) {
    renderFees.lastCh = channels[0].channel;
  }
  const active = renderFees.lastCh;

  html += `<div class="fees-tabs">`;
  for (const ch of channels) {
    const n = (ch.models || []).length;
    const on = ch.channel === active ? " active" : "";
    // 渠道余额角标：取自 state.accounts（与积分汇总条同源），让用户在选择渠道时即可看到
    // 「这个渠道还有多少可用」；单位随渠道计量单位切换（token / 积分，见 tabBalText）。
    const bal = channelCredits(ch.channel);
    const balHtml = bal === null ? ""
      : `<span class="fees-tab-bal" title="${esc(tabBalTitle(bal))}">${esc(tabBalText(bal))}</span>`;
    html += `<button class="fees-tab${on}" data-feech="${esc(ch.channel)}"
      title="${esc(chLabel(ch.channel))}">${esc(chLabel(ch.channel))}<span class="fees-tab-n">${n}</span>${balHtml}</button>`;
  }
  html += `</div>`;

  for (const ch of channels) {
    const models = ch.models || [];
    const hidden = ch.channel !== active ? ' class="hidden"' : '';
    html += `<div class="fees-panel${hidden ? ' hidden' : ''}" data-feepanel="${esc(ch.channel)}">`;
    html += feeChannelAccessHtml(ch); // 自定义渠道才非空：接入点 + Key 一览（R119）
    html += `<table><thead><tr><th>模型</th><th>倍率</th><th>模型</th><th>倍率</th></tr></thead><tbody>`;
    for (let i = 0; i < models.length; i += 2) {
      const m1 = models[i];
      const m2 = models[i + 1];
      const id1 = modelCellHtml(m1, ch.channel);
      const id2 = modelCellHtml(m2, ch.channel);
      html += `<tr><td>${id1}</td><td>${rateCell(m1)}</td><td>${id2}</td><td>${rateCell(m2)}</td></tr>`;
    }
    html += `</tbody></table>`;
    // 「显示全部模型」开着就把拉取结果列在表格下面（R122）
    if (feeShowAll.has(ch.channel)) html += feeAllBoxHtml(ch.channel);
    html += `</div>`;
  }

  html += `<div class="note" style="margin-top:8px">${esc(fees.disclaimer || "")}</div>`;
  box.innerHTML = html;
}

// bindFeesTabs 费率渠道标签点击切换（事件委托，绑定一次）。
// 渲染只改 active 类与面板可见性，不重建 DOM，避免滚动位置跳动。
function bindFeesTabs() {
  const box = $("feesBox");
  box.addEventListener("click", (e) => {
    // 模型名点击复制（官方 v2.6.1）：code.fee-model-copy 优先判定，
    // 与下方的 .full-id（二开完整 ID 行）互不冲突、语义一致。
    const code = e.target.closest("code.fee-model-copy");
    if (code && box.contains(code)) {
      copyText(code.dataset.mid, "模型名");
      return;
    }
    // 完整模型 ID 一键复制（二开，升级勿丢）：点费率表每行模型下方的完整ID即复制
    const cpid = e.target.closest && e.target.closest(".full-id");
    if (cpid) {
      copyText(cpid.dataset.mid || cpid.textContent, "模型 ID");
      return;
    }
    // 自定义渠道的接入点 / api-key 一键复制（R119）：key 走按需取明文，不得复制脱敏串
    const urlc = e.target.closest && e.target.closest("code.fee-copy-url");
    if (urlc) {
      copyText(urlc.dataset.copy, "接入点");
      return;
    }
    const keyc = e.target.closest && e.target.closest("code.fee-copy-key");
    if (keyc) {
      copyCustomKey(keyc.dataset.ch);
      return;
    }
    const btn = e.target.closest(".fees-tab");
    if (!btn) return;
    const ch = btn.dataset.feech;
    renderFees.lastCh = ch;
    box.querySelectorAll(".fees-tab").forEach((x) => x.classList.toggle("active", x === btn));
          box.querySelectorAll(".fees-panel").forEach(function (p) {
        if (p.dataset.feepanel === ch) { p.classList.remove("hidden"); }
        else { p.classList.add("hidden"); }
      });
  });
  // 「显示全部模型」开关（R122）：用 change 委托，开关状态存模块级，重渲染不丢。
  // 不得放 click：label 包住 input 时 click 会被 label 转发再冒泡一次，双重切换。
  box.addEventListener("change", (e) => {
    const sw = e.target.closest && e.target.closest("input[data-allmodels]");
    if (sw) toggleFeeAllModels(sw.dataset.allmodels, sw.checked);
  });
  // 键盘可达：模型名支持 Enter/Space 复制（与 role="button" 对应）。
  box.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" && e.key !== " ") return;
    const code = e.target.closest("code.fee-model-copy");
    if (code && box.contains(code)) { e.preventDefault(); copyText(code.dataset.mid, "模型名"); return; }
    const urlc = e.target.closest("code.fee-copy-url");
    if (urlc) { e.preventDefault(); copyText(urlc.dataset.copy, "接入点"); return; }
    const keyc = e.target.closest("code.fee-copy-key");
    if (keyc) { e.preventDefault(); copyCustomKey(keyc.dataset.ch); }
  });
}

// fmtTokensCompact 把 token 数格式化为 1M / 192k 紧凑形式。
//
// ⚠️ 用途边界（2026-10-17 修「同名函数互相覆盖」后确立）：
//   - 本函数（k/M）**只**用于「模型元数据」——上下文窗口、最大输出这类上游以
//     k/M 口径描述的能力值，保持与官方文档一致（131072 -> 131k）。
//   - **额度 / 用量数字**（账号卡片的 Token 计数、明细单元格、平台表）必须用
//     `fmtTokens`（万/亿中文单位）—— 那些数字动辄上亿，`94539k` 用户读不出来。
// 历史：本函数原名 `fmtTokens`，与后面新增的万/亿版**同名**，后者在前、本函数在后，
// 于是本函数静默覆盖了它 ⇒ Token 卡片显示成 `94539k`。改名以消除覆盖。
function fmtTokensCompact(n) {
  if (!n) return "-";
  if (n >= 1000000 && n % 1000000 === 0) return `${n / 1000000}M`;
  if (n >= 1000) return `${Math.round(n / 1000)}k`;
  return String(n);
}
