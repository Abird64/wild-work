"use strict";
// ---------- 自定义渠道（OpenAI 兼容，R66） ----------
// RESERVED_CHANNEL_NAMES 与 provider.BuiltinKinds() 同集（+ ModelNamespaces 的上游命名空间）：
// 与内建渠道同名时 server/app 两层会指向不同 Runtime，故前端先给即时反馈（后端同样会拒）。
// ⚠️ 新增内建渠道时这里要同步：漏一个就变成「前端不拦、保存后才报错」
//   （ui-static-check 会对比 provider.go 的 Kind 常量）。
const RESERVED_CHANNEL_NAMES = new Set([
  "workbuddy", "workbuddyai", "traework", "traecode", "qoder", "qodercn", "qodercom",
  "qwenwork", "monkeycode", "raccoon", "loomy", "oczen", "kilo", "minimax", "lobsterai",
  "zcode", "gemini", "codearts", "glm", "cline", "officeace", "doubao", "doubaowork",
  "cline-free", // ModelNamespaces：上游 ID 自带命名空间，自定义渠道不得占用
]);
let editCustomChannels = [];

// parseTokens 解析带 k/M 单位的 token 数（如 1M→1000000、64k→64000）；纯数字原样返回。
function parseTokens(str, fallback) {
  if (str === undefined || str === null || str === "") return fallback || 0;
  if (typeof str === "number") return str;
  const s = String(str).trim();
  if (!s) return fallback || 0;
  const upper = s.toUpperCase();
  let mult = 1;
  let numStr = upper;
  if (upper.endsWith("M")) {
    mult = 1000000;
    numStr = upper.slice(0, -1).trim();
  } else if (upper.endsWith("K")) {
    mult = 1000;
    numStr = upper.slice(0, -1).trim();
  }
  const n = parseFloat(numStr);
  if (isNaN(n) || n < 0) return fallback || 0;
  return Math.round(n * mult);
}

// formatTokenCount 反向格式化：优先整数 k/M（1M、64k），非整除时保留一位小数。
function formatTokenCount(num) {
  if (!num) return "—";
  if (num >= 1000000 && num % 100000 === 0) return `${num / 1000000}M`;
  if (num >= 1000 && num % 1000 === 0) return `${num / 1000}k`;
  if (num >= 1000000) return `${(num / 1000000).toFixed(1).replace(/\.0$/, "")}M`;
  if (num >= 1000) return `${(num / 1000).toFixed(1).replace(/\.0$/, "")}k`;
  return String(num);
}

// capSourceTag 能力来源小章（R119）：「上游」= /models 声明过，「手填」= 用户在面板填的，
// 「未探测」= 从来没拉过 / 拉了但上游什么能力字段都没给。
// 为什么要显示：旧版导入把「1M/64k/全能力」当上游声明写死，面板与 /v1/models
// 把「不知道」冒充成「上游说的」（R49 同款纪律）；来源标出来后用户才知道该去补哪些。
// 措辞与费率页 capText() 共用同一套语义，不在两处另维护名单。
function capSourceTag(src) {
  const s = src || "unknown";
  const map = { api: ["上游", "cap-api", "能力/上下文来自上游 /models 声明"], user: ["手填", "cap-user", "能力/上下文由您在面板手填（本工具不核实）"], unknown: ["未探测", "cap-unknown", "未拿到能力信息：点「＋ 模型」拉一次上游 /models；不编造默认值"] };
  const [txt, cls, tip] = map[s] || map.unknown;
  return `<span class="cm-meta ${cls}" title="${esc(tip)}">${txt}</span>`;
}

function renderCustomChList() {
  // R85：每个 Provider 一个区块，Provider 自己一行（信息 + 操作），
  // 它下面的每个模型也各占一行（模型信息 + 操作）。**任何一行都不换行**，
  // 靠横向滚动兜底而不是折行。
  const blocks = editCustomChannels.map((c, i) => {
    const keyMask = c.api_key || "无密钥"; // 后端已脱敏（maskKey），前端再切一遍会把脱敏串切坏（R121）
    const models = c.models || [];

    const modelRows = models.map((m, j) => {
      const mid = typeof m === "string" ? m : m.id;
      const ctxNum = typeof m === "object" ? parseTokens(m.context_window, 0) : 0;
      const outNum = typeof m === "object" ? parseTokens(m.max_tokens, 0) : 0;
      const ctx = ctxNum ? formatTokenCount(ctxNum) : "—";
      const out = outNum ? formatTokenCount(outNum) : "—";
      const efforts = typeof m === "object" && m.reasoning_efforts ? m.reasoning_efforts : [];
      const caps = [];
      if (typeof m === "object" && m.supports_images) caps.push("识图");
      if (typeof m === "object" && (m.supports_reasoning || efforts.length)) caps.push("思考");
      if (typeof m !== "object" || m.supports_tools !== false) caps.push("工具");
      // 未探测过能力的行不得把「工具」当成已声明显示（上面那个兼容分支在旧数据上永远为真）
      const srcTag = capSourceTag(typeof m === "object" ? m.cap_source : "");
      return `<div class="cm-model-row">
        <div class="cm-info">
          <code class="cm-model-id" title="${esc(mid)}">${esc(mid)}</code>
          ${srcTag}
          <span class="cm-meta">上下文 ${esc(ctx)}</span>
          <span class="cm-meta">输出 ${esc(out)}</span>
          <span class="cm-meta ${efforts.length ? "cm-effort" : ""}">${efforts.length ? "档位 " + esc(efforts.join("/")) : "无思考"}</span>
          <span class="cm-meta">${esc(caps.join("·") || "—")}</span>
        </div>
        <button class="btn small cm-btn-edit" data-edit-model-in="${i}:${j}">编辑</button>
        <button class="btn small danger cm-btn-del" data-del-model-in="${i}:${j}">删除</button>
      </div>`;
    }).join("");

    return `<div class="cm-provider">
      <div class="cm-provider-row">
        <div class="cm-info">
          ${chBadge(c.name, editCustomChannels)}
          <span class="cm-url" title="${esc(c.base_url)}">${esc(c.base_url)}</span>
          <span class="cm-key hint" data-ch-name="${esc(c.name)}" style="cursor:pointer"
            title="点击复制完整 api-key（此时才向后端取明文）">[${esc(keyMask)}]</span>
          ${c.auth === "basic" ? '<span class="cm-meta" title="鉴权方式：Authorization: Basic base64(api_key)">Basic</span>' : ""}
          <span class="cm-meta">${models.length} 个模型</span>
        </div>
        <button class="btn small cm-btn-edit" data-edit-custom="${i}">编辑</button>
        <button class="btn small cm-btn-add" data-add-model-in="${i}" title="打开模型面板：默认是上游 /models 清单（点 ＋ 加入），第二个页签手填/编辑">＋ 模型</button>
        <button class="btn small danger cm-btn-del" data-del-custom="${i}">删除</button>
      </div>
      <div class="cm-models">
        ${modelRows || '<div class="cm-model-row cm-empty-row"><span class="hint" style="margin:0">（该 Provider 还没有模型，点「＋ 模型」）</span></div>'}
      </div>
    </div>`;
  }).join("");

  $("customChList").innerHTML = blocks || `<div class="hint">（暂无自定义 Provider，点下方「＋ 新增 Provider」开始配置）</div>`;
  $("customChErr").textContent = "";
  $("customChOk").textContent = "";
}



// ---------- 思考强度注入（设置弹层：客户端未传 reasoning_effort 时按模型注入档位） ----------
// effRules 编辑态副本（openSettings 时从 state.reasoning_efforts 拷贝，`*` 全局默认
// 单独走「全局默认」输入行；保存成功后由 loadState 刷新）。
let effRules = [];

// renderEffList 渲染模型档位编辑区：每条映射一个卡片区块（降级链规则卡片同款设计语言）——
// 「映射 N」徽标 + 模型 ID（超长省略，title 悬浮看全）→ 档位 + 🗑；左对齐不撑满整行。
function renderEffList() {
  const rows = effRules.map((r, i) =>
    `<div class="fb-rule-card fb-map-card">
      <div class="fb-rule-head">
        <span class="fb-rule-badge">映射 ${i + 1}</span>
        <span class="fb-map-model" title="${esc(r.model)}">${esc(r.model)}</span>
        <span class="fb-map-arrow">→</span>
        <span class="fb-map-eff">${esc(r.effort)}</span>
        <button class="icon-btn" data-del-eff="${i}" title="删除该映射">🗑</button>
      </div>
    </div>`
  ).join("");
  $("effList").innerHTML = rows || `<div class="fb-empty">暂无模型档位——未配置的请求不注入</div>`;
  $("effErr").textContent = "";
  updateEffDirty();
}

async function saveReasoningEfforts() {
  try {
    const efforts = {};
    const g = $("effGlobalInput").value.trim();
    if (g) efforts["*"] = g;
    for (const r of effRules) efforts[r.model] = r.effort;
    await api("/api/config/reasoning_efforts", { efforts });
    toast("思考强度已保存（热生效）");
    $("effErr").textContent = "";
    state.reasoning_efforts = efforts; // 即时同步服务端态基准，提示随之熄灭
    updateEffDirty();
    loadState();
  } catch (e) { $("effErr").textContent = e.message; }
}
