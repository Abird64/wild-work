"use strict";
// ---------- 模型降级链（设置弹层：原模型无可用账号时按链顶备胎，不粘滞自动切回） ----------
// fbRules 编辑态副本（openSettings 时从 state.model_fallback 深拷贝；保存成功后由 loadState 刷新）
let fbRules = [];

// fbFromSet 汇总当前全部主力模型ID（跨规则）。
// 用途只有两处：① 候选下拉把「已作主力」的模型从备胎候选里排掉（去噪，避免点了一个注定
// 无效的项）；② 新增/追加备胎时给即时反馈。**它不再驱动任何数据删除**（SR-003 / SD-3 甲）。
function fbFromSet() {
  const s = {};
  for (const r of fbRules) if (r.from) s[r.from] = true;
  return s;
}

// fbSelfLoops 自环检查：返回 `{ from, to }` 列表（某规则的备胎等于它自己的主力）。
// 这是**唯一**的不合法形态——`To` 允许包含其它规则（甚至其它场景）的 `From`：
// 三场景互为备胎（code 主 traework / 首选备胎 raccoon，complex 反之）是 MS-007 的**有意设计**，
// 现役配置里那两条互备关系各有 430 / 110 次真实请求在跑（SR-003 任务卡取证）。
// 旧 fbSanitize 把「任意规则的主力」从**每条**备胎链里剔掉，等于把这份互备当脏数据清洗，
// 打开设置即静默削弱降级链，还顺带点亮「未保存」提示催用户去落盘——已按甲分支彻底移除。
// 只读：不改 fbRules，不产生副作用，可在渲染/保存/输入校验任何时机调用。
function fbSelfLoops(rules) {
  const out = [];
  for (const r of (rules || fbRules) || []) {
    if (!r || !r.from) continue;
    for (const t of r.to || []) if (t === r.from) out.push({ from: r.from, to: t });
  }
  return out;
}

// fbSavePayload 生成落盘载荷：**原样**拷贝 fbRules，只做「去空串」与「自环剔除」，
// 返回 `{ rules, err }`——有自环时 rules = null 且 err 给可辨识文案（宁可整次拒绝，
// 也不留部分写入，与契约 SR-001「失败语义」一致）。
// 注意：这里**不做**跨规则剔除。落盘是 G1 的最后一道口子——一旦删，互备关系永久丢失。
function fbSavePayload() {
  const loops = fbSelfLoops(fbRules);
  if (loops.length > 0) {
    const uniq = [...new Set(loops.map((x) => x.from))];
    return {
      rules: null,
      err: `备胎不能是它自己的主力模型（自环）：${uniq.join("、")}——请先移除对应备胎`,
    };
  }
  const rules = fbRules.map((r) => ({
    from: r.from,
    to: (r.to || []).filter((t) => String(t || "").trim() !== ""),
  }));
  return { rules, err: null };
}

// fbSaveGuard 保存前的自环预检：无自环返回空串；有则返回文案（供 #fbErr 直接展示）。
function fbSaveGuard() {
  const p = fbSavePayload();
  return p.err || "";
}

// updateFbDirty / updateEffDirty：编辑态与服务端态比对，有差异时亮「未保存」提示。
// 比对基于 JSON 序列化（fbRules/to 顺序敏感——改序也算未保存，语义正确）；保存成功
// 后服务端态刷新（loadState → openSettings 未重开时由 save 回调手动清），零差异即隐藏。
// **语义收窄（SR-003 / G1）**：脏标记只反映**用户动作**造成的差异。渲染期不再有任何
// 程序自身的数据裁剪（fbSanitize 已删），故「刚打开设置即亮红」不会再出现；反过来，
// 任何一次 fbRules 的变化都必然来自用户（增/删/改序/改主模型），因此这里不会漏报。
function fbServerRules() { return JSON.stringify(state.model_fallback || []); }
function updateFbDirty() {
  const el = $("fbDirty");
  if (!el) return;
  el.style.display = JSON.stringify(fbRules) === fbServerRules() ? "none" : "";
}
function updateEffDirty() {
  const el = $("effDirty");
  if (!el) return;
  const effs = state.reasoning_efforts || {};
  const g = $("effGlobalInput").value.trim();
  const cur = {}; if (g) cur["*"] = g;
  for (const r of effRules) cur[r.model] = r.effort;
  const norm = (o) => JSON.stringify(Object.keys(o).sort().map((k) => [k, o[k]]));
  el.style.display = norm(cur) === norm(effs) ? "none" : "";
}

// renderFbList 渲染降级链编辑区：每条规则一个卡片区块（自定义渠道卡片同款设计语言）——
// head 行 = 「规则 N」徽标 + 主力模型ID 输入框 + 🗑；body = 备胎链 chips（含序号与拖拽）+ 添加行。
// chip 序号即降级优先级（内核 pickModelFallback 按 rule.To 顺序取第一个可承载备胎），
// 拖动改序只改编辑态 fbRules，点「保存降级链」才落盘（语义与上游一致）。
// 交互全部事件委托在 #fbList 上（data-del-rule / data-del-to / data-add-to / data-edit-idx /
// dragstart / dragover / drop / dragend），每轮 innerHTML 重建后委托仍然有效。
function renderFbList() {
  // 渲染只读（SR-003 / G1）：本函数**不得**对 fbRules 做任何写操作。
  // 旧实现在这里调 fbSanitize() 剔除跨规则备胎 ⇒ 打开设置那一刻编辑态已被削弱，
  // 且 updateFbDirty() 因这份程序自身造成的差异而亮起「⚠ 有未保存的调整」。
  // 现在：渲染只按当前数据画卡片；自环拦截只在输入/保存路径（fbSelfLoops / fbSaveGuard）。
  const loops = fbSelfLoops(fbRules);
  const rows = fbRules.map((r, i) => {
    const tos = r.to.map((t, j) => {
      const isLoop = t === r.from; // 仅标记，**不改数据**（拦截在保存/输入路径）
      return `<span class="fb-to-chip"${isLoop ? ' data-loop="1"' : ""} draggable="true" data-chip="${i}:${j}" data-model="${esc(t)}" title="${esc(t)}${isLoop ? "（自环：不能是自己主力的备胎）" : ""}（拖动调整顺序，序号 = 降级优先级）">`
      + `<span class="chip-idx">${j + 1}</span><span class="chip-grip">⠿</span>${esc(t)}`
      + `<a data-del-to="${i}:${j}" title="移除该备胎">×</a></span>`;
    }).join("");
    return `<div class="fb-rule-card">
      <div class="fb-rule-head">
        <span class="fb-rule-badge">规则 ${i + 1}</span>
        <span class="pk-wrap fb-from-wrap"><input class="input fb-from-edit" data-edit-idx="${i}" value="${esc(r.from)}" spellcheck="false" title="主力模型ID（channel/model，可编辑；失焦或回车确认，点「保存降级链」生效）" placeholder="主力模型ID（channel/model）"></span>
        <button class="icon-btn" data-del-rule="${i}" title="删除整条规则">🗑</button>
      </div>
      <div class="fb-rule-body">
        <div class="fb-chips">${tos || `<span class="hint">（无备胎）</span>`}</div>
        <div class="row" style="margin-top:2px">
          <span class="pk-wrap grow"><input class="input fb-to-add" data-add-idx="${i}" spellcheck="false" placeholder="＋ 备胎模型ID（模糊搜索 / 回车添加 / 逗号分隔多个）"></span>
          <button class="icon-btn" data-add-to="${i}" title="把添加框中的备胎追加到该规则末尾">＋</button>
        </div>
      </div>
    </div>`;
  }).join("");
  $("fbList").innerHTML = (rows || `<div class="fb-empty">暂无规则——原模型无可用账号时将直接返回 503 提示</div>`)
    // 自环是**唯一**不合法形态，必须在编辑区就能看见（只提示，不删数据）
    + (loops.length > 0
      ? `<div class="hint" style="color:var(--danger);margin-top:6px">⚠ 自环：${esc([...new Set(loops.map((x) => x.from))].join("、"))} 的备胎里含它自己的主力模型——保存会被拒绝，请先移除该备胎</div>`
      : "");
  $("fbErr").textContent = "";
  attachFbPickers();
  updateFbDirty();
}

// parseModelList 解析备胎输入：逗号/空格/换行分隔多个 channel/model，本地形态校验。
function parseModelList(raw) {
  const out = [];
  for (const p of String(raw || "").split(/[,，\s]+/)) {
    const t = p.trim();
    if (!t) continue;
    if (!/^[^/]+\/.+/.test(t)) return [null, `备胎模型 ${t} 需为 channel/model 形式（如 workbuddyai/glm-5.2）`];
    out.push(t);
  }
  return [out, ""];
}

// attachFbPickers 给 renderFbList 产出的行内输入框挂模糊搜索下拉（每轮 innerHTML 重建后重挂）：
// - 主力模型ID（.fb-from-edit）：单选下拉，选中回填；Enter 无候选时 blur 确认（原行为）。
// - 行内备胎添加框（.fb-to-add）：多选下拉——点选即追加到备胎链末尾（精确等值去重，
//   与上游 to.includes 全等语义一致）、已上链模型从候选隐藏、可连续选择多个。
// fbToPick 备胎候选的排除集 + 追加判定（单一来源，避免「候选能选、点选又静默不做」两套语义）。
// **只排除两类**：① 本规则自己的主力（自环，唯一不合法形态）；② 本规则已上链的（去重）。
// 绝不排除「其它规则的主力」——那正是三场景互为备胎（MS-007 有意设计），
// 旧实现把它当互斥脏数据静默挡掉/剔掉，是 G1 的另一半。
// fbFromSet 仍保留，但只用于**提示**（见 attachFbPickers 的 title 标注），不再驱动任何删除或拒绝。
function fbToPick(i) {
  const r = fbRules[i] || { from: "", to: [] };
  const exclude = { [r.from]: true };
  for (const t of r.to || []) exclude[t] = true;
  return { exclude, blocked: (id) => id === r.from || (r.to || []).includes(id) };
}

function attachFbPickers() {
  const fset = fbFromSet(); // 只读快照：给备胎候选加「其它场景主力」提示用
  for (const inp of $("fbList").querySelectorAll(".fb-from-edit")) {
    const self = fbRules[+inp.dataset.editIdx].from; // 自身当前值保留可见，仅排除其他规则主力（重复由 change 唯一性校验兜底）
    attachModelPicker(inp, { excludeSet: () => { const s = { ...fbFromSet() }; delete s[self]; return s; } });
  }
  for (const inp of $("fbList").querySelectorAll(".fb-to-add")) {
    const i = +inp.dataset.addIdx;
    const pick = fbToPick(i);
    attachModelPicker(inp, {
      stayOpen: true,
      excludeSet: () => pick.exclude,
      onPick: (id) => {
        const r = fbRules[i];
        if (!r) return;
        if (pick.blocked(id)) return; // 自环或已上链 → 不追加（候选已隐藏，双保险）
        r.to.push(id);
        renderFbList();
        toast(fset[id]
          ? `已作为互备加入第 ${r.to.length} 位（${id} 是其它场景的主力；点保存生效）`
          : `已加入备胎链第 ${r.to.length} 位（点保存生效）`);
      },
    });
  }
}

async function saveModelFallback() {
  // 落盘路径同样**不得**做跨规则删除（SR-003 / G1 的最后一道口子：一删就永久）。
  // fbSavePayload 只去空串 + 拦自环，有自环即整次拒绝（不留部分写入）。
  const payload = fbSavePayload();
  if (payload.err) {
    $("fbErr").textContent = payload.err;
    toast(payload.err);
    return;
  }
  try {
    await api("/api/config/model_fallback", { rules: payload.rules });
    toast("降级链已保存（热生效）");
    $("fbErr").textContent = "";
    // 即时同步服务端态基准：用实际提交的载荷（已去空串），否则提示恒亮
    state.model_fallback = JSON.parse(JSON.stringify(payload.rules));
    updateFbDirty();
    loadState();
  } catch (e) { $("fbErr").textContent = e.message; }
}
