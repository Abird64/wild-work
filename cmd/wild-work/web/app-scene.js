"use strict";
// ---------- 三场景路由编辑器（SR-005b） ----------
// 一段独立的编辑区，取代「在 textarea 里手写 键 = 渠道/模型 + 另找降级链卡片对齐」的旧形态。
// 设计约束（全部来自裁决，改代码前先读）：
//   SD-1「点了就存」⇒ 无 diff 预览、无二次确认；但保存前**必须**静默复制旧值（唯一回滚抓手，R-3）。
//   SD-6 ⇒ 唯一写入口 `POST /api/config/scene_routing`（一次 config.Save，无中间态）。
//   SD-7 ⇒ 每次打开重算重排；这里只管编辑，推荐列表在 SR-005c。
//   SD-9 ⇒ 别名固定三个，不接受第四个。
//   SD-4 ⇒ 同主模型/自环/别名等非法形态**当场拦下 + 大白话**（服务端也拦，这里是先手）。
// 编辑态 sceneRules 与 state.compat.model_map / state.model_fallback 的关系：
//   **唯一真相源是服务端**。openSettings 时从 state 深拷贝生成编辑态；保存成功后用服务端
//   返回值覆盖（不信前端算的），保证「面板显示 A、运行时用 B」不可能发生（A-3）。

// SCENE_ALIASES 场景别名与中文名的单一来源（SD-9：只做三个）。
// 顺序即 UI 展示顺序，也是提交时的固定顺序（服务端按 sceneAliases 固定顺序生成，幂等）。
const SCENE_ALIASES = [
  { alias: "fast",    label: "快速", desc: "低延迟优先：日常问答、轻量改写" },
  { alias: "code",    label: "代码", desc: "工具调用是硬条件：需要 tools 能力 + 足够上下文" },
  { alias: "complex", label: "复杂", desc: "深度推理与长上下文优先：方案、长文分析" },
];

// SCENE_CANDIDATES 推荐候选池（来源 = SR-002 的 scene-candidates.json）。
// ⚠️ 这里只搬 id 与证据等级：**活值（倍率/余额/实测速度）一律现取 lastFees/stats**，
//    绝不把快照数字当现值用（否则「推荐」会拿着过期数据排序）。
// 复制而非 fetch 静态资源：本项目无构建链（TP 非目标 4），加一次 fetch 就多一个部署面。
const SCENE_CANDIDATES = {
 "_meta": {
  "source": "SR-002 scene-candidates.json",
  "sampled_at": "2026-10-27"
 },
 "scenes": [
  {
   "alias": "fast",
   "candidates": [
    {
     "id": "traework/deepseek-v4.1-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "local-measured",
      "capability": "local-measured",
      "context": "upstream-declared"
     },
     "routed_out": false
    },
    {
     "id": "workbuddy/deepseek-v4.1-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "local-measured",
      "capability": "upstream-declared",
      "context": "upstream-declared"
     },
     "routed_out": true
    },
    {
     "id": "qodercn/glm-5.3-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "upstream-declared",
      "context": "upstream-declared"
     },
     "routed_out": false
    },
    {
     "id": "workbuddy/glm-5.3-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "upstream-declared",
      "context": "upstream-declared"
     },
     "routed_out": true
    },
    {
     "id": "kilo/cohere/north-mini-code:free",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "upstream-declared",
      "context": "unverified"
     },
     "routed_out": false
    },
    {
     "id": "traecode/deepseek-v4.1-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "local-measured",
      "context": "upstream-declared"
     },
     "routed_out": false
    },
    {
     "id": "traecode/glm-5.3-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "local-measured",
      "context": "upstream-declared"
     },
     "routed_out": false
    },
    {
     "id": "traecode/mimo-v2.6-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "local-measured",
      "context": "upstream-declared"
     },
     "routed_out": false
    }
   ]
  },
  {
   "alias": "code",
   "candidates": [
    {
     "id": "workbuddy/deepseek-v4.1-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "local-measured",
      "capability": "upstream-declared",
      "context": "upstream-declared"
     },
     "routed_out": true
    },
    {
     "id": "raccoon/sn-deepseek-v4-1-flash-1017",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "local-measured",
      "capability": "upstream-declared",
      "context": "upstream-declared"
     },
     "routed_out": false
    },
    {
     "id": "workbuddy/glm-5.3-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "upstream-declared",
      "context": "upstream-declared"
     },
     "routed_out": true
    },
    {
     "id": "qwenwork/flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "local-measured",
      "capability": "upstream-declared",
      "context": "upstream-declared"
     },
     "routed_out": false
    },
    {
     "id": "kilo/nvidia/nemotron-3-ultra-550b-a55b:free",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "upstream-declared",
      "context": "unverified"
     },
     "routed_out": false
    },
    {
     "id": "traecode/deepseek-v4.1-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "local-measured",
      "context": "upstream-declared"
     },
     "routed_out": false
    },
    {
     "id": "traecode/glm-5.3-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "local-measured",
      "context": "upstream-declared"
     },
     "routed_out": false
    },
    {
     "id": "traecode/mimo-v2.6-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "local-measured",
      "context": "upstream-declared"
     },
     "routed_out": false
    }
   ]
  },
  {
   "alias": "complex",
   "candidates": [
    {
     "id": "raccoon/sn-deepseek-v4-1-flash-1017",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "local-measured",
      "capability": "upstream-declared",
      "context": "upstream-declared"
     },
     "routed_out": false
    },
    {
     "id": "workbuddy/deepseek-v4.1-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "local-measured",
      "capability": "upstream-declared",
      "context": "upstream-declared"
     },
     "routed_out": true
    },
    {
     "id": "workbuddy/glm-5.3-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "upstream-declared",
      "context": "upstream-declared"
     },
     "routed_out": true
    },
    {
     "id": "qodercn/glm-5.3-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "upstream-declared",
      "context": "upstream-declared"
     },
     "routed_out": false
    },
    {
     "id": "kilo/nvidia/nemotron-3-ultra-550b-a55b:free",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "upstream-declared",
      "context": "unverified"
     },
     "routed_out": false
    },
    {
     "id": "traecode/deepseek-v4.1-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "local-measured",
      "context": "upstream-declared"
     },
     "routed_out": false
    },
    {
     "id": "traecode/glm-5.3-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "local-measured",
      "context": "upstream-declared"
     },
     "routed_out": false
    },
    {
     "id": "traecode/mimo-v2.6-flash",
     "report_evidence": {
      "rate": "upstream-declared",
      "speed": "unverified",
      "capability": "local-measured",
      "context": "upstream-declared"
     },
     "routed_out": false
    }
   ]
  }
 ]
};

// 编辑态：{ [alias]: { from, to: [] } }。不进 state，避免污染服务端数据。
let sceneRules = {};

// sceneNormalize 把服务端两份数据合成编辑态：model_map 提供 from，model_fallback 按 From 对齐提供 to。
// 找不到对应规则 = 该别名指向一个没有降级链的模型（真实可能的坏状态），to 记空数组让用户看见并补。
function sceneNormalize(compat, fallback) {
  const map = (compat && compat.model_map) || {};
  const rules = fallback || [];
  const out = {};
  for (const { alias } of SCENE_ALIASES) {
    const from = String(map[alias] || "").trim();
    const hit = rules.find((r) => String(r.from || "").trim() === from);
    out[alias] = { from, to: hit ? (hit.to || []).map((t) => String(t).trim()).filter(Boolean) : [] };
  }
  return out;
}

// sceneProblems 本地预检：返回 [{ alias, level: "error"|"warn", msg }]。
// 只做**结构**判断（服务端才是判据权威）；能本地拦的先拦，省一次往返、也让错误留在眼前。
// 唯一硬错误：自环（备胎 = 本链主模型）、主模型为空/形状不对、别名混进 from/to、主模型跨场景重复。
function sceneProblems(rules) {
  const out = [];
  const seenFrom = {};
  for (const { alias, label } of SCENE_ALIASES) {
    const r = rules[alias] || { from: "", to: [] };
    const from = String(r.from || "").trim();
    if (!from) {
      out.push({ alias, level: "error", msg: "主模型不能为空" });
    } else if (SCENE_ALIASES.some((x) => x.alias === from)) {
      // 别名判定**必须**排在形状判定之前：别名天然不含 `/`，先判形状会把
      // 「你写了别名」误报成「格式不对」，用户照着格式提示改还是错。
      out.push({ alias, level: "error", msg: `主模型不能写成别名 ${from}——要写 渠道/模型 全路径（写别名会让整条链永不触发且不报错）` });
    } else if (!/^[^/]+\/.+/.test(from)) {
      out.push({ alias, level: "error", msg: `主模型 ${from} 需为 渠道/模型 形式（如 traework/deepseek-v4.1-flash）` });
    } else {
      if (seenFrom[from]) {
        out.push({ alias, level: "error", msg: `主模型 ${from} 与「${seenFrom[from]}」重复——两个场景共用一个主模型时，降级链只会有一条生效` });
      } else {
        seenFrom[from] = label;
      }
    }
    for (const t of r.to || []) {
      const tv = String(t || "").trim();
      if (!tv) continue;
      if (tv === from) {
        out.push({ alias, level: "error", msg: `备胎不能是它自己的主模型（自环）：${tv}` });
      } else if (SCENE_ALIASES.some((x) => x.alias === tv)) {
        out.push({ alias, level: "error", msg: `备胎不能写成别名 ${tv}——要写 渠道/模型 全路径` });
      } else if (!/^[^/]+\/.+/.test(tv)) {
        out.push({ alias, level: "error", msg: `备胎 ${tv} 需为 渠道/模型 形式` });
      }
    }
    if ((r.to || []).length === 0) {
      out.push({ alias, level: "warn", msg: "备胎链为空——主模型无容量时这条链不会降级（请求会直接失败）" });
    }
  }
  // 互备是合法的（SD-3），这里只做**信息性**说明，不是错误。
  for (const { alias, label } of SCENE_ALIASES) {
    const r = rules[alias] || { to: [] };
    for (const other of SCENE_ALIASES) {
      if (other.alias === alias) continue;
      const otherFrom = String((rules[other.alias] || {}).from || "").trim();
      if (otherFrom && (r.to || []).includes(otherFrom)) {
        out.push({ alias, level: "info", msg: `备胎里含「${other.label}」的主模型 ${otherFrom}——互为备胎是合法的，无需处理` });
      }
    }
  }
  return out;
}

// scenePayload 生成提交体：三场景按固定顺序 + 回填 default_channel / max_tokens_cap。
// **必须回填**这两项：端点内部会整体写 compat 的这三项，漏传等于把默认渠道和封顶清零（R-4）。
function scenePayload(extra) {
  const cc = (state && state.compat) || {};
  const body = {
    scenes: SCENE_ALIASES.map(({ alias }) => {
      const r = sceneRules[alias] || { from: "", to: [] };
      return { alias, from: String(r.from || "").trim(), to: (r.to || []).map((t) => String(t).trim()).filter(Boolean) };
    }),
    default_channel: cc.default_channel || "",
    max_tokens_cap: cc.max_tokens_cap || 0,
  };
  return Object.assign(body, extra || {});
}

// syncSceneSideViews 把「三场景」之外的**同源派生视图**同步到最新 state。
//
// 为什么必须有（2026-10-29 独立审核抓到的加重情节）：
//   ① `#mapText`（设置→模型路由→映射表）只在 `openSettings` 时从 model_map 灌一次，`render()` 从不刷新；
//      于是「打开过设置 → 在三场景卡片里保存 → 回设置点通用『保存』」会按**旧文本**重写 model_map，
//      把刚保存的别名值静默回滚，且绕过全部场景校验。
//   ② `fbRules` 同样只灌一次 ⇒ 三场景保存后它仍是旧副本，`updateFbDirty` 会因**程序自身**造成的差异
//      点亮「⚠ 有未保存的调整」——与 SR-003 已修掉的 G1（假脏标记催用户落盘）同形，只是换了来源。
//
// **不覆盖用户编辑**（这点很关键，否则修好一个坏、引入另一个更坏的）：
//   - mapText：仅当它当前内容仍等于「打开时灌进去的那份」才刷新；用户改过就不动。
//   - fbRules：仅当它与服务端当前态**逐字节相等**（= 无未保存编辑）时才刷新。
// 判据都基于「有没有用户改动」，不是「是不是开着」。
function syncSceneSideViews() {
  if (!state) return;
  const cc = state.compat || {};
  const map = cc.model_map || {};
  const mapText = $("mapText");
  if (mapText && mapText.dataset.baseline !== undefined) {
    const cur = mapText.value;
    const fresh = Object.entries(map).map(([k, v]) => `${k} = ${v}`).join("\n");
    // 只有「用户没动过」才覆盖（baseline = 上次灌进去的内容）
    if (cur === mapText.dataset.baseline) {
      mapText.value = fresh;
      mapText.dataset.baseline = fresh;
    }
  }
  // 降级链编辑态：只在没有未保存差异时同步
  if (typeof fbRules !== "undefined" && $("fbList")) {
    const serverNow = JSON.stringify(state.model_fallback || []);
    if (JSON.stringify(fbRules) === JSON.stringify(serverNow) || fbRules.length === 0) {
      fbRules = JSON.parse(JSON.stringify(state.model_fallback || []));
      renderFbList();
    } else {
      // 有用户编辑：只更新脏标记基准，不覆盖数据（否则会吞掉用户正在改的东西）
      updateFbDirty();
    }
  }
}

// sceneConfigText 生成「复制当前配置」与「保存前自动备份」共用的文本（同一份，避免两处漂移）。
// 格式刻意用可直接回粘的结构（JSON）+ 一行人类可读摘要：纯 JSON 手改容易，纯文本手改更容易但改不回 JSON。
function sceneConfigText() {
  const cc = (state && state.compat) || {};
  const map = cc.model_map || {};
  const rules = (state.model_fallback || []).filter((r) => SCENE_ALIASES.some((x) => map[x.alias] === r.from));
  return JSON.stringify({ model_map: map, model_fallback: rules }, null, 2);
}

// sceneBackup 保存前静默把旧值写进剪贴板（SD-1 的唯一缓解措施，R-3：不得省）。
// 返回 true/false 供 toast 文案区分——剪贴板在非 https 下可能被拒，那就明说，不假装成功。
async function sceneBackup() {
  const text = sceneConfigText();
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (e) {
    try {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand("copy");
      document.body.removeChild(ta);
      return ok;
    } catch (err) { return false; }
  }
}

// sceneSetFrom 改某场景的主模型（附「主模型跨场景唯一」的即时反馈）。
function sceneSetFrom(alias, id) {
  if (!sceneRules[alias]) return;
  const v = String(id || "").trim();
  if (!v) return;
  // 改主模型不会顺手删数据：若 v 已在本链备胎里，改完即自环 ⇒ 拒绝并说明（与 SR-003 同口径）。
  if ((sceneRules[alias].to || []).includes(v)) {
    $("sceneErr").textContent = `主模型不能改成 ${v}：它已在「${SCENE_ALIASES.find((x) => x.alias === alias).label}」的备胎链上（会变成自环），请先移除该备胎`;
    return;
  }
  sceneRules[alias].from = v;
  renderSceneCards();
}

// sceneAddTo 追加一个备胎（只拦自环与重复——跨场景互备是合法的，SD-3）。
function sceneAddTo(alias, id) {
  const r = sceneRules[alias];
  if (!r) return;
  const v = String(id || "").trim();
  if (!v) return;
  if (v === r.from) { $("sceneErr").textContent = "备胎不能是它自己的主模型（自环）"; return; }
  if ((r.to || []).includes(v)) { $("sceneErr").textContent = `备胎 ${v} 已在该链上（无需重复添加）`; return; }
  r.to.push(v);
  renderSceneCards();
}

// sceneMoveTo 调整备胎顺序（序号 = 降级优先级）。up=true 上移一位。
function sceneMoveTo(alias, idx, delta) {
  const r = sceneRules[alias];
  if (!r) return;
  const j = idx + delta;
  if (j < 0 || j >= r.to.length) return;
  const [moved] = r.to.splice(idx, 1);
  r.to.splice(j, 0, moved);
  renderSceneCards();
}

// sceneDelTo 移除一个备胎。
function sceneDelTo(alias, idx) {
  const r = sceneRules[alias];
  if (!r || idx < 0 || idx >= r.to.length) return;
  r.to.splice(idx, 1);
  renderSceneCards();
}

// sceneSummary 一个场景的「当前指向」单行摘要（供卡片头部与原始视图对照）。
function sceneSummary(r) {
  const to = (r && r.to) || [];
  return `${(r && r.from) || "（未设置）"} → ${to.length ? to.join(" → ") : "（无备胎）"}`;
}

// ---------- 三场景推荐候选列表（SR-005c） ----------
// 定位：**帮用户挑，不替用户选**（文案必须写明「仅供参考」，R-4）。
// 排序真相源是 rankCandidates（SR-005a 纯函数），这里只做呈现，绝不另算一遍口径。
//
// 三条硬约束（改代码前先读）：
//   ① 证据等级（report_evidence）只做**徽标**，严禁进排序（SD-2）。故本区块**只读**
//      SCENE_CANDIDATES 的 evidence，不改 rankCandidates 的输出顺序——顺序由它自己决定。
//   ② 每次展开都重算重排（SD-7）。数据取自 lastFees / lastStats（openSceneEditor 已 await 取过）。
//   ③ `cached_at` 陈旧度必须显式（R-3b 的镜像面）：拿旧费率算出来的"推荐"必须让用户看得见它旧。
const SCENE_REC_STALE_MIN = 30; // ≥30 分钟就高亮提示；与 /api/fees 的后台刷新间隔（30min）同量级

// 推荐列表的展开态与当前 tab（模块级，只在设置弹层存活期间有效）
let sceneRecOpen = false;
let sceneRecTab = "fast";

// sceneCandOverride 只为一件事存在：让 ui-static-check 能注入"另一份证据等级数据"来断言
// 「改证据等级不改变排序」。生产路径永远为 null ⇒ 读常量。不设 setter 以外的入口，
// 避免变成"运行期可换候选池"的隐性口子（候选池的来源只能是 SR-002 数据文件）。
let sceneCandOverride = null;

// sceneCandData 读当前生效的候选池数据（生产 = SCENE_CANDIDATES 常量）。
function sceneCandData() { return sceneCandOverride || SCENE_CANDIDATES; }

// sceneEvidenceOf 取某模型在某场景的证据等级快照（只读，绝不参与排序）。
// 返回 null 表示该模型不在 SR-002 候选池里（例如用户从搜索框临时挑的）。
function sceneEvidenceOf(alias, id) {
  const sc = (sceneCandData().scenes || []).find((x) => x.alias === alias);
  const hit = ((sc && sc.candidates) || []).find((c) => c.id === id);
  return hit ? (hit.report_evidence || null) : null;
}

// sceneFeeAgeMinutes 解析 /api/fees 的 cached_at（形如 `10-29 12:10`，无年份）为「多少分钟前」。
// 返回 null = 解析不了（旧后端/格式变了），此时不显示年龄而不是显示 0（0 会被误读成"刚刚刷新"）。
// ⚠️ 跨年陷阱：该格式不带年份。今天 01-01 而 cached_at 写 `12-31 23:59` ⇒ 那是**去年**的，
//    按今年解析会得到一个"未来时刻"、减出负数。判据是：解析结果超过「未来容忍窗」即说明
//    这条快照属于上一年 ⇒ **减一年**（不是加一年；加了会把去年说成明年，年龄仍然错）。
//    容忍窗取 10 分钟：费率刷新就有分钟级抖动，不能把"时钟略慢"误判成跨年。
const SCENE_AGE_FUTURE_TOLERANCE_MS = 10 * 60 * 1000;
function sceneFeeAgeMinutes(cachedAt) {
  const m = /^(\d{2})-(\d{2})\s+(\d{2}):(\d{2})$/.exec(String(cachedAt || "").trim());
  if (!m) return null;
  const now = new Date();
  const build = (y) => new Date(y, +m[1] - 1, +m[2], +m[3], +m[4], 0, 0);
  let t = build(now.getFullYear());
  if (t.getTime() - now.getTime() > SCENE_AGE_FUTURE_TOLERANCE_MS) t = build(now.getFullYear() - 1);
  const mins = Math.floor((now.getTime() - t.getTime()) / 60000);
  return mins >= 0 ? mins : 0; // 极端时钟漂移下兜到 0，绝不返回负数让 UI 显示 "-617004 分钟"
}

// sceneAgeLabel 陈旧度文案。分档只为可读，判据只有一条：距上次拉取多久。
function sceneAgeLabel(minutes) {
  if (minutes === null) return "";
  if (minutes < 0) return "";
  if (minutes < 1) return "刚刚刷新";
  if (minutes < 60) return `${minutes} 分钟前`;
  const h = Math.floor(minutes / 60), r = minutes % 60;
  return `${h} 小时${r ? r + " 分" : ""}前`;
}

// sceneStateBadge 四态的展示文案（与 rankCandidates 的 state 字段一一对应）。
// ⚠️ UNPRICED 必须说清「消耗未知」而不是「免费」——这是 SR-002 §5.1.1 与 SR-008 §2.1 的底线。
const SCENE_STATE_META = {
  PRICED:    { label: "已定价", cls: "ok",   tip: "上游已下发倍率，消耗速率已知" },
  FREE:      { label: "免费",   cls: "free", tip: "零消耗，不占用期额度" },
  UNPRICED:  { label: "消耗未知", cls: "unk", tip: "上游未下发倍率（priced=false 且 free=false）——不是免费，实际花多少分无法核实" },
  UNAVAILABLE: { label: "已摘除路由", cls: "off", tip: "该模型所在渠道已被摘出全部路由" },
};

// renderSceneRec 渲染推荐候选列表（每次调用都重算重排，SD-7）。
// 只在展开时计算：收起状态下不算 = 不浪费 CPU，也不会让"没展开却显示旧结果"发生。
function renderSceneRec() {
  const box = $("sceneRecBody");
  if (!box) return;
  const panel = $("sceneRecPanel");
  if (panel) panel.classList.toggle("hidden", !sceneRecOpen);
  if (!sceneRecOpen) { box.innerHTML = ""; return; }

  // tab 高亮
  const tabs = $("sceneRecTabs");
  if (tabs) {
    tabs.innerHTML = SCENE_ALIASES.map(({ alias, label }) =>
      `<button class="btn small${alias === sceneRecTab ? " primary" : ""}" data-rec-tab="${alias}">${esc(label)}</button>`
    ).join("");
  }

  const age = sceneFeeAgeMinutes((lastFees && lastFees.cached_at) || "");
  const stale = age !== null && age >= SCENE_REC_STALE_MIN;
  const list = rankCandidates(lastFees, lastStats, sceneRecTab, sceneCandData());

  const head = `<div class="rec-head">
      <span class="hint" style="margin:0">费率数据 <b>${esc((lastFees && lastFees.cached_at) || "未加载")}</b>
      ${age !== null ? `（${esc(sceneAgeLabel(age))}${stale ? "，已超过 " + SCENE_REC_STALE_MIN + " 分钟，建议点上方「刷新费率」" : ""}）` : ""}</span>
      <span class="hint" style="margin:0">共 ${list.length} 个候选 · 推荐仅供参考，可自由选择任意模型</span>
    </div>`;

  const rows = list.map((r, rank) => {
    const ev = sceneEvidenceOf(sceneRecTab, r.id);
    const st = SCENE_STATE_META[r.state] || SCENE_STATE_META.UNPRICED;
    const rate = r.free ? "Free" : (r.priced ? `x${(+r.rate || 0).toFixed(2)}` : "未知");
    const ctx = r.ctx ? formatTokenCount(r.ctx) : "—";
    const caps = [
      r.tools === true ? "工具" : "",
      r.reasoning === true ? "推理" : "",
    ].filter(Boolean).join("·") || "—";
    const evBadges = ev ? Object.entries(ev).filter(([, v]) => v === "local-measured")
      .map(([k]) => `<span class="rec-ev" title="该维度为本机实测">${esc(k)}实测</span>`).join("") : "";
    const missTip = r.missing && r.missing.length ? `数据缺口：${r.missing.join("、")}` : "";
    return `<div class="rec-row" data-rec-id="${esc(r.id)}">
      <span class="rec-rank">${rank + 1}</span>
      ${chBadge(r.channel)}
      <span class="rec-id" title="${esc(r.id)}">${esc(r.id)}</span>
      <span class="rec-state ${st.cls}" title="${esc(st.tip)}">${esc(st.label)}</span>
      <span class="rec-num" title="当前倍率（上游按时段可能切换档位）">${esc(rate)}</span>
      <span class="rec-num" title="上下文窗口">${esc(ctx)}</span>
      <span class="rec-num" title="已声明的能力">${esc(caps)}</span>
      <span class="rec-acts">
        <button class="btn small" data-rec-set="${esc(r.id)}" title="把这个模型设为「${esc(SCENE_ALIASES.find((x) => x.alias === sceneRecTab).label)}」的主模型">设为主模型</button>
        <button class="btn small" data-rec-add="${esc(r.id)}" title="追加到该场景备胎链末位">加为备胎</button>
      </span>
      ${evBadges ? `<span class="rec-evbox" title="证据等级（SD-2：只做标注，不参与排序）">${evBadges}</span>` : `<span class="rec-evbox" title="${esc(missTip || "无本机实测证据")}"><span class="rec-ev none">未实测</span></span>`}
    </div>`;
  }).join("");

  box.innerHTML = head + (rows || `<div class="fb-empty">该场景暂无候选（SR-002 候选池里没有可用模型）</div>`);
}

// toggleSceneRec 展开/收起推荐列表。展开时强制重算（SD-7：每次打开都重算重排）。
function toggleSceneRec() {
  sceneRecOpen = !sceneRecOpen;
  if (sceneRecOpen) sceneRecTab = sceneRecTab || "fast";
  renderSceneRec();
  const btn = $("btnSceneRec");
  if (btn) btn.textContent = sceneRecOpen ? "收起推荐候选" : "展开推荐候选";
}

// renderSceneCards 渲染三张场景卡片。
// 交互走事件委托（#sceneCards 上），因此每轮 innerHTML 重建无需重挂监听。
// 行为：主模型行 = 可搜索单选 input（attachModelPicker）+ 备胎 chips（上移/下移/移除）+ 追加行。
function renderSceneCards() {
  const box = $("sceneCards");
  if (!box) return;
  const probs = sceneProblems(sceneRules);
  const byAlias = {};
  for (const p of probs) (byAlias[p.alias] ||= []).push(p);

  box.innerHTML = SCENE_ALIASES.map(({ alias, label, desc }) => {
    const r = sceneRules[alias] || { from: "", to: [] };
    const mine = byAlias[alias] || [];
    const errs = mine.filter((p) => p.level === "error");
    const tos = (r.to || []).map((t, j) => `
      <span class="fb-to-chip" data-scene-chip="${alias}:${j}" data-model="${esc(t)}" title="${esc(t)}（序号 = 降级优先级）">
        <span class="chip-idx">${j + 1}</span>
        <a data-scene-up="${alias}:${j}" title="上移一位（更优先）"${j === 0 ? ' class="is-off"' : ""}>↑</a>
        <a data-scene-down="${alias}:${j}" title="下移一位"${j === r.to.length - 1 ? ' class="is-off"' : ""}>↓</a>
        <span class="chip-txt">${esc(t)}</span>
        <a data-scene-del="${alias}:${j}" title="移除该备胎">×</a>
      </span>`).join("");
    const notes = mine.filter((p) => p.level !== "error")
      .map((p) => `<div class="hint" style="color:${p.level === "warn" ? "var(--warn)" : "var(--muted)"};margin-top:2px">${p.level === "warn" ? "⚠ " : "· "}${esc(p.msg)}</div>`).join("");
    const errHtml = errs.length
      ? `<div class="hint" style="color:var(--danger);margin-top:4px">${errs.map((p) => esc(p.msg)).join("；")}</div>` : "";
    return `<div class="fb-rule-card scene-card" data-scene="${alias}"${errs.length ? ' data-invalid="1"' : ""}>
      <div class="fb-rule-head">
        <span class="fb-rule-badge">${esc(alias)}</span>
        <span class="scene-label">${esc(label)}</span>
        <span class="hint" style="margin:0">${esc(desc)}</span>
      </div>
      <div class="fb-rule-body">
        <div class="row" style="margin-top:0">
          <label class="lbl scene-lbl">主模型</label>
          <span class="pk-wrap grow"><input class="input scene-from" data-scene-from="${alias}" value="${esc(r.from)}" spellcheck="false" placeholder="渠道/模型（支持模糊搜索下拉）"></span>
        </div>
        <div class="row" style="margin-top:4px;align-items:flex-start">
          <label class="lbl scene-lbl" style="margin-top:4px">备胎链</label>
          <div class="grow">
            <div class="fb-chips">${tos || '<span class="hint">（无备胎）</span>'}</div>
            <span class="pk-wrap" style="margin-top:4px;display:block"><input class="input scene-to-add" data-scene-add="${alias}" spellcheck="false" placeholder="＋ 追加备胎（模糊搜索 / 回车添加 / 逗号分隔多个）"></span>
          </div>
        </div>
        ${errHtml}${notes}
        <div class="hint scene-sum" style="margin-top:4px">降级顺序：${esc(sceneSummary(r))}</div>
      </div>
    </div>`;
  }).join("");

  attachScenePickers();
  renderSceneWarn();
}

// renderSceneWarn 汇总展示所有场景的非字段级提醒（互备信息 + 空链警告）。
function renderSceneWarn() {
  const el = $("sceneWarn");
  if (!el) return;
  const probs = sceneProblems(sceneRules);
  const infos = probs.filter((p) => p.level !== "error").map((p) => {
    const lab = SCENE_ALIASES.find((x) => x.alias === p.alias).label;
    return `${lab}：${p.msg}`;
  });
  el.textContent = infos.length ? infos.join("　|　") : "";
}

// attachScenePickers 给卡片里的输入框挂模糊搜索下拉（每轮 innerHTML 重建后重挂）。
// 主模型：单选（选中即回填并重渲染）。备胎追加：多选（点选即追加到链末）。
function attachScenePickers() {
  const box = $("sceneCards");
  if (!box) return;
  for (const inp of box.querySelectorAll(".scene-from")) {
    const alias = inp.dataset.sceneFrom;
    attachModelPicker(inp, {
      // 排除「其它场景的主模型」只是去噪提示，不阻止选择（同主模型由 sceneProblems 拦）。
      excludeSet: () => {
        const s = {};
        for (const { alias: a } of SCENE_ALIASES) {
          const f = String((sceneRules[a] || {}).from || "").trim();
          if (f && a !== alias) s[f] = true;
        }
        return s;
      },
      onPick: (id) => sceneSetFrom(alias, id),
    });
  }
  for (const inp of box.querySelectorAll(".scene-to-add")) {
    const alias = inp.dataset.sceneAdd;
    attachModelPicker(inp, {
      stayOpen: true,
      excludeSet: () => {
        const r = sceneRules[alias] || { from: "", to: [] };
        const s = { [r.from]: true };
        for (const t of r.to || []) s[t] = true;
        return s;
      },
      onPick: (id) => sceneAddTo(alias, id),
    });
  }
}

// parseSceneModelList 解析备胎追加输入（逗号/空格/换行分隔）。
function parseSceneModelList(raw) {
  const out = [];
  for (const p of String(raw || "").split(/[,，\s]+/)) {
    const t = p.trim();
    if (!t) continue;
    if (!/^[^/]+\/.+/.test(t)) return [null, `备胎 ${t} 需为 渠道/模型 形式（如 traework/deepseek-v4.1-flash）`];
    out.push(t);
  }
  return [out, ""];
}

// sceneDryRun 点「预检」：调端点 dry_run=true，只校验 + 回提醒，不写盘。
async function sceneDryRun() {
  const local = sceneProblems(sceneRules).filter((p) => p.level === "error");
  if (local.length) {
    $("sceneErr").textContent = local.map((p) => p.msg).join("；");
    return;
  }
  $("sceneErr").textContent = "";
  $("btnSceneDryRun").disabled = true;
  try {
    const res = await api("/api/config/scene_routing", scenePayload({ dry_run: true }));
    showSceneServerWarn(res);
    toast("预检通过（未写盘）");
  } catch (e) {
    $("sceneErr").textContent = e.message;
  } finally {
    $("btnSceneDryRun").disabled = false;
  }
}

// showSceneServerWarn 展示服务端 raw warnings（I6 目录里查不到 / I7 首选备胎同渠道）。
function showSceneServerWarn(res) {
  const ws = (res && res.warnings) || [];
  $("sceneWarn").textContent = ws.length ? `服务端提醒：${ws.join("　|　")}` : "";
}

// saveSceneRouting 点「保存三场景」：本地预检 → **静默复制旧值** → 原子写 → 回读校验。
// 顺序不能反：备份必须在**发出写请求之前**完成。若等请求回来再备份，服务端已经是新值，
// 剪贴板里就是新值，等于没有备份（R-3 想防的正是这个）。
async function saveSceneRouting() {
  const local = sceneProblems(sceneRules).filter((p) => p.level === "error");
  if (local.length) {
    const msg = local.map((p) => p.msg).join("；");
    $("sceneErr").textContent = msg;
    toast(msg);
    return;
  }
  $("sceneErr").textContent = "";
  const btn = $("btnSceneSave");
  btn.disabled = true;
  let backedUp = false;
  try {
    backedUp = await sceneBackup();
    // ❗ 回读校验必须拿「**发请求前**的期望值」比对。
    //
    // 2026-10-29 独立审核抓到的缺陷：原实现顺序是「先用 res 覆盖 state → 再用 res 重算 sceneRules
    // → 再拿 sceneRules 与 res 比」= **自证**，生产路径上永远返回一致；且只比 from 不比 to
    // （服务端会对 to 做 trim/去空项，这类差异检不出）。用户拿到的"已回读校验"是假的。
    // 现在：请求前把期望值（含 to 数组）固定成快照，校验只读快照，不读被覆盖后的编辑态。
    const expected = scenePayload();
    const res = await api("/api/config/scene_routing", expected);
    // 用服务端返回值覆盖编辑态与基准（不信前端算的，A-3）
    if (res && res.model_map) {
      state.compat = state.compat || {};
      state.compat.model_map = res.model_map;
      state.compat.default_channel = res.default_channel;
      state.compat.max_tokens_cap = res.max_tokens_cap;
    }
    if (res && res.model_fallback) state.model_fallback = res.model_fallback;
    sceneRules = sceneNormalize(state.compat, state.model_fallback);
    renderSceneCards();
    showSceneServerWarn(res);
    // 回读校验：服务端口径才是真相，比对三别名指向与服务端返回值是否一致。
    const mismatch = verifySceneReadback(res, expected);
    if (mismatch) {
      $("sceneErr").textContent = `已保存，但回读不一致：${mismatch}`;
      toast("已保存但回读不一致，请检查");
    } else {
      toast(backedUp ? "三场景已保存（旧值已复制到剪贴板）" : "三场景已保存（⚠ 旧值复制失败，请手动备份）");
    }
    syncSceneSideViews(); // 同步 mapText / fbRules 派生视图（防"通用保存把别名值回滚"与"假脏标记"）
    loadState(); // 让面板其它区域（compat 摘要、降级链列表）同步到新值
  } catch (e) {
    $("sceneErr").textContent = e.message;
    toast(backedUp ? `保存失败：${e.message}（旧值已在剪贴板）` : `保存失败：${e.message}`);
  } finally {
    btn.disabled = false;
  }
}

// verifySceneReadback 比对「**发请求前**的期望值」与「服务端返回的实际值」，不一致返回差异描述（空串=一致）。
//
// ⚠️ expected 必须是请求前的快照（由 saveSceneRouting 传入），**不能**读 sceneRules ——
// 那时 sceneRules 已被 res 覆盖，拿它比对等于自证（2026-10-29 独立审核实测抓到的缺陷）。
//
// 比两样：① 每个别名的 from ② 该场景降级链的 to **逐项**（顺序有语义，服务端还会 trim/去空项，
// 这些差异必须在用户面前显形，否则"回读一致"是假的）。
function verifySceneReadback(res, expected) {
  if (!res || !res.model_map || !expected) return "";
  const diff = [];
  const resTo = {};
  for (const r of res.model_fallback || []) {
    resTo[String(r.from || "").trim()] = (r.to || []).map((t) => String(t || "").trim()).filter(Boolean);
  }
  for (const sc of expected.scenes || []) {
    const alias = sc.alias;
    const wantFrom = String(sc.from || "").trim();
    const gotFrom = String(res.model_map[alias] || "").trim();
    if (wantFrom && gotFrom !== wantFrom) {
      diff.push(`${alias} 主模型期望 ${wantFrom}，服务端为 ${gotFrom || "（空）"}`);
      continue; // from 都不一致时链已无对照意义
    }
    const wantTo = (sc.to || []).map((t) => String(t || "").trim()).filter(Boolean);
    const gotTo = resTo[wantFrom] || [];
    if (wantTo.length !== gotTo.length) {
      diff.push(`${alias} 备胎链项数期望 ${wantTo.length}，服务端为 ${gotTo.length}（${gotTo.join(" → ") || "空"}）`);
    } else if (wantTo.join("\n") !== gotTo.join("\n")) {
      diff.push(`${alias} 备胎链顺序/内容不一致：期望 ${wantTo.join(" → ")}，服务端 ${gotTo.join(" → ")}`);
    }
  }
  return diff.join("；");
}

// openSceneEditor 打开设置时初始化编辑态。
// **必须 await 取数**（SD-7 连带约束 1 / R-3b）：推荐列表要用活的 lastFees/stats，
// 拿上一次缓存冒充新数据会让「重算重排」变成假动作。这里只管编辑区，
// 但取数放在同一处，SR-005c 的列表直接复用这份数据，不会各拉一遍。
async function openSceneEditor() {
  sceneRules = sceneNormalize(state && state.compat, state && state.model_fallback);
  renderSceneCards();
  // 推荐列表默认收起：不看就不算（SD-7 只承诺"每次展开重算"，不承诺"一直开着"）
  sceneRecOpen = false;
  sceneRecTab = "fast";
  const recBtn = $("btnSceneRec");
  if (recBtn) recBtn.textContent = "展开推荐候选";
  renderSceneRec();
  $("sceneErr").textContent = "";
  // 费率与统计：只有缺的时候才拉（已有则不重复打接口）；下载失败不阻塞编辑区。
  if (!lastFees) { try { await loadFees(); } catch (e) { /* 不阻塞 */ } }
  if (!lastStats) { try { await loadStats(); } catch (e) { /* 不阻塞 */ } }
}
