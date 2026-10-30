"use strict";
// ---------- 模型面板（R122：导入与手填合并成一个 tab panel） ----------
// 旧版是三个弹窗（模型管理 / 单模型编辑 / 导入勾选）。一个渠道的模型只有两个来路
// （上游列出来的 / 自己填的），分三个入口只会让用户来回找；列表也从「勾选 → 导入选中」
// 改成逐行 ＋/− 立即增删：勾完还是要点保存，中间那步没有意义。
let mgrChIdx = -1;      // 当前面板服务的渠道（editCustomChannels 下标）
let editModelIdx = -1;  // 手填页签正在编辑的模型下标（-1=新增）
let pickTab = "api";
let pendingRenames = [];
// upstreamModels 只存拉到的原始条目 {id, cap}；「是否已添加」渲染时现算，
// 不在这里夹 exists/checked 状态，否则增删后要同步两处必漂移。
let upstreamModels = [];

function openModelPick(idx, tab, modelIdx) {
  const ch = editCustomChannels[idx];
  if (!ch) return;
  mgrChIdx = idx;
  pickTab = tab === "manual" ? "manual" : "api";
  editModelIdx = typeof modelIdx === "number" ? modelIdx : -1;
  $("modelPickOverlay").classList.remove("hidden");
  if (pickTab === "manual") {
    // 从模型行「编辑」进来时只想改个能力，不该顺手打上游一发请求（手填页签自绘表单）
    openModelEditor(editModelIdx);
  } else {
    switchPickTab("api");
    loadUpstreamModels();
  }
}

// switchPickTab 切页签：只改可见性，不重拉上游（重新拉靠「↻」按钮）。
function switchPickTab(tab) {
  pickTab = tab === "manual" ? "manual" : "api";
  document.querySelectorAll("#modelPickOverlay [data-pick-tab]").forEach((b) => {
    b.classList.toggle("active", b.dataset.pickTab === pickTab);
  });
  $("pickPaneApi").classList.toggle("hidden", pickTab !== "api");
  $("pickPaneManual").classList.toggle("hidden", pickTab !== "manual");
  const ch = editCustomChannels[mgrChIdx];
  $("pickChName").textContent = ch ? `${ch.name}（已配置 ${(ch.models || []).length} 个）` : "";
  $("btnMeSave").textContent = editModelIdx >= 0 ? "保存此模型" : "添加此模型";
}

function closeModelPick() {
  $("modelPickOverlay").classList.add("hidden");
  $("meId").value = "";
  upstreamModels = [];
  mgrChIdx = -1;
  editModelIdx = -1;
}

// renderImportList 上游清单逐行：行尾一个 ＋/− 按钮（未添加＝＋，已添加＝－），
// 已添加的行另给「更新能力」（把旧版编造的默认值洗成上游真值）。
function renderImportList() {
  const ch = editCustomChannels[mgrChIdx];
  if (!ch) { $("importList").innerHTML = ""; return; }
  const have = new Set((ch.models || []).map((m) => (typeof m === "string" ? m : m.id)));
  const kw = $("importFilter").value.trim().toLowerCase();
  const rows = upstreamModels.map((m, i) => {
    if (kw && !m.id.toLowerCase().includes(kw)) return "";
    const on = have.has(m.id);
    const btn = on
      ? `<button class="btn small danger" data-pick-toggle="${i}" title="从本渠道移除该模型">－</button>` +
        `<button class="btn small cm-btn-cap" data-apply-cap="${i}"${m.cap ? "" : " disabled"} title="${m.cap ? "用本次拉到的上游能力覆写已存模型（不动模型 ID）" : "本次未拉到该模型的能力信息"}">更新能力</button>`
      : `<button class="btn small primary" data-pick-toggle="${i}" title="加入本渠道（写入配置仍需点「保存」）">＋</button>`;
    return `<div class="import-row${on ? " picked" : ""}"><label class="import-name"><code>${esc(m.id)}</code>${capSummaryHtml(m.cap)}</label>${btn}</div>`;
  }).join("");
  $("importList").innerHTML = rows || '<div class="hint">（没有匹配的模型）</div>';
  switchPickTab(pickTab); // 同步标题里的「已配置 N 个」与按钮文案
}

// togglePickModel 行尾 ＋/−：直接改编辑态（与删模型行一致，统一由「保存」落盘）。
function togglePickModel(i) {
  const ch = editCustomChannels[mgrChIdx];
  const row = upstreamModels[i];
  if (!ch || !row) return;
  ch.models = ch.models || [];
  const k = ch.models.findIndex((m) => (typeof m === "string" ? m : m.id) === row.id);
  if (k >= 0) {
    ch.models.splice(k, 1);
    $("customChOk").textContent = `已移除 ${row.id}，点「保存」生效`;
  } else {
    ch.models.push(row.cap ? capToModelConfig(row.cap) : { id: row.id });
    $("customChOk").textContent = `已添加 ${row.id}，点「保存」生效`;
  }
  renderCustomChList();
  renderImportList();
}

// loadUpstreamModels 拉一次该渠道的上游目录（后端已做规范化 + 候选根嗅探 + 能力解析）。
async function loadUpstreamModels() {
  const ch = editCustomChannels[mgrChIdx];
  if (!ch) return;
  upstreamModels = [];
  $("importHint").textContent = `从 ${ch.base_url} 的 /models 拉取…`;
  $("importErr").textContent = "";
  $("importList").innerHTML = '<div class="hint">加载中…</div>';
  try {
    const res = await probeCustomChannel(ch);
    // 确认过的根就地回写（R121）：用户粘的可能是漏协议头或带 /chat/completions 尾巴的地址。
    if (res.base_url && res.base_url !== ch.base_url) {
      ch.base_url = res.base_url;
      renderCustomChList();
    }
    $("importList").innerHTML = "";
    if (!res.ok) {
      const tried = (res.tries || []).map((t) => `${t.url} → ${t.status || t.err || "?"}`).join("； ");
      $("importErr").textContent = `拉取失败：${res.error || "上游未返回模型列表"}${tried ? `（试过：${tried}）` : ""}`;
      $("importHint").textContent = res.base_url ? `接入点 ${res.base_url}` : "";
      return;
    }
    const caps = res.caps && res.caps.length ? res.caps : null;
    const list = caps || (res.models || []).filter((id) => id).map((id) => ({ id }));
    upstreamModels = list.filter((c) => c && c.id).map((c) => ({ id: c.id, cap: caps ? c : null }));
    const noCap = upstreamModels.filter((m) => !m.cap).length;
    $("importHint").textContent = `接入点 ${ch.base_url}${res.chat_ok ? "（对话端点已确认）" : ""}｜上游共 ${upstreamModels.length} 个模型。` +
      (noCap ? `有 ${noCap} 个上游没声明能力，加入后会标「未探测」，需要的话请手动补填。` : "") +
      "点行尾 ＋ 加入，－ 移除。";
    renderImportList();
  } catch (e) {
    $("importErr").textContent = `拉取失败：${e.message}`;
    $("importList").innerHTML = "";
  }
}

// ---------- 手填/编辑模型（R122：不再独立弹窗，住在模型面板第 2 个页签） ----------
function openModelEditor(modelIdx) {
  const ch = editCustomChannels[mgrChIdx];
  if (!ch) return;
  editModelIdx = typeof modelIdx === "number" ? modelIdx : -1;
  const m = editModelIdx >= 0 ? ch.models[editModelIdx] : null;
  const o = typeof m === "object" && m ? m : {};
  $("meId").value = m ? (typeof m === "string" ? m : m.id) : "";
  // 默认值一律留空：旧版新建时预填「1M / 64k / 三能力全勾」，保存即变成「上游声明」写进配置
  // 并进 /v1/models（R49 禁止编造）。拉不到就空着，由上游列表页签从上游拿或由用户自己填。
  $("meCtx").value = o.context_window ? formatTokenCount(parseTokens(o.context_window, 0)) : "";
  $("meOut").value = o.max_tokens ? formatTokenCount(parseTokens(o.max_tokens, 0)) : "";
  $("meEff").value = (o.reasoning_efforts || []).join(", ");
  $("meImg").checked = !!o.supports_images;
  $("meRsn").checked = !!o.supports_reasoning || (o.reasoning_efforts || []).length > 0;
  $("meTools").checked = typeof m === "string" ? true : o.supports_tools !== false;
  $("meErr").textContent = "";
  if (!$("modelPickOverlay").classList.contains("hidden") || m) switchPickTab("manual");
  if ($("meCapSrc")) $("meCapSrc").innerHTML = capSourceTag(o.cap_source) +
    (m ? "" : ' <span class="hint">新增模型默认不预填任何能力：留空保存就是「未探测」。</span>');
  setTimeout(() => $("meId").focus(), 50);
}

function saveModelEditor() {
  const ch = editCustomChannels[mgrChIdx];
  if (!ch) return;
  const id = $("meId").value.trim();
  if (!id) return ($("meErr").textContent = "请填入模型 ID");
  ch.models = ch.models || [];
  if (ch.models.some((m, j) => j !== editModelIdx && (typeof m === "string" ? m : m.id) === id)) {
    return ($("meErr").textContent = `该渠道下已存在模型 ${id}`);
  }
  // 思考档位：逗号/顿号/空格分隔都接受，去空去重保序
  const efforts = $("meEff").value.split(/[,，、\s]+/).map((x) => x.trim()).filter(Boolean)
    .filter((x, k, arr) => arr.indexOf(x) === k);
  const rec = {
    id,
    context_window: parseTokens($("meCtx").value, 0),
    max_tokens: parseTokens($("meOut").value, 0),
    supports_images: $("meImg").checked,
    supports_reasoning: $("meRsn").checked || efforts.length > 0,
    supports_tools: $("meTools").checked,
    reasoning_efforts: efforts,
  };
  if (efforts.length) rec.default_reasoning_effort = efforts[0];
  // 面板保存的就是人手填的值 ⇒ 标 "user"；一个字都没改（直接点确定）则保留原来源，
  // 避免把上游声明的能力误标成手填（那会让 tooltip 说「您手填」而逐辑上根本没动）。
  const prev = editModelIdx >= 0 && typeof ch.models[editModelIdx] === "object" ? ch.models[editModelIdx] : null;
  const dirty = !prev || ["context_window", "max_tokens", "supports_images", "supports_reasoning", "supports_tools"].some((k) => (prev[k] || 0) !== (rec[k] || 0))
    || ((prev.reasoning_efforts || []).join(",")) !== efforts.join(",");
  rec.cap_source = dirty ? "user" : (prev.cap_source || "user");
  if (!rec.context_window) delete rec.context_window;
  if (!rec.max_tokens) delete rec.max_tokens;
  if (editModelIdx >= 0) ch.models[editModelIdx] = rec; else ch.models.push(rec);
  editModelIdx = -1;
  renderCustomChList();
  renderImportList(); // 行尾的 ＋/− 状态要跟着变
  $("customChOk").textContent = "已更新，点击「保存全部配置」生效";
  $("customChErr").textContent = "";
}

// ---------- 自定义模型弹窗（R85） ----------
// 设置里只留一个「配置自定义模型」按钮，实际增删改全在这个弹窗里。
// editCustomBaseline 是打开时的快照，用来判「有没有改过」（R122：关闭要带保存语义）。
let editCustomBaseline = "";

function openCustomModels() {
  editCustomChannels = JSON.parse(JSON.stringify(state.custom_channels || []));
  editCustomBaseline = JSON.stringify(editCustomChannels);
  pendingRenames = [];
  renderCustomChList();
  $("customModelsOverlay").classList.remove("hidden");
}

// closeCustomModels 关闭（R122）：旧版直接把弹层藏起来，用户改完点「关闭」= 全部丢弃，
// 而按钮长得跟「确定」一样。现改成：有改动先保存（热生效），再刷新模型列表与费率页，
// 确保新 provider 当场出现在「模型列表和费率」里；保存失败则不关（错误得留在原地）。
async function closeCustomModels() {
  if (JSON.stringify(editCustomChannels) !== editCustomBaseline || pendingRenames.length) {
    const ok = await saveCustomChannels();
    if (!ok) return; // 保存失败：不关窗，错误已写在 customChErr
  }
  $("customModelsOverlay").classList.add("hidden");
}

// saveCustomChannels 提交当前编辑态 + 改名映射，回 true=成功。
// renames 不能不供：后端只比新旧两张表无法区分「改名」与「删一个 + 新增一个」，
// 后者会把旧名的真 key 丢到新名上（R122）。
async function saveCustomChannels() {
  try {
    const res = await api("/api/config/custom_channels", { channels: editCustomChannels, renames: pendingRenames });
    const n = (res && res.renamed) || 0;
    toast(n ? `自定义模型已保存（热生效），其中 ${n} 个渠道已改名` : "自定义模型已保存（热生效）");
    $("customChErr").textContent = "";
    $("customChOk").textContent = "已保存并立即生效";
    pendingRenames = [];
    editCustomBaseline = JSON.stringify(editCustomChannels);
    // 刷新两处口径：模型列表（state.models）与费率页（各渠道模型面），新渠道 must 当场出现
    await Promise.all([loadState(), loadFees()]);
    return true;
  } catch (e) {
    $("customChErr").textContent = e.message;
    $("customChOk").textContent = "";
    return false;
  }
}

// ---------- 能力搬运辅助（R119/R122）----------
// 上游 /models 尽力解析出的能力原样搬进 ModelConfig，拉不到的字段一律不写。
// 旧版在这里写死 1M/64k/全能力，面板与 /v1/models 于是把「不知道」说成「上游说的」
// ——R119 推翻。cap_source="api" 才能被当作声明展示（与后端 custom.ModelCap.ToModelConfig 同口径）。
function capToModelConfig(c) {
  const rec = { id: c.id };
  if (c.context_window) rec.context_window = c.context_window;
  if (c.max_tokens) rec.max_tokens = c.max_tokens;
  if (c.supports_images) rec.supports_images = true;
  if (c.supports_reasoning) rec.supports_reasoning = true;
  if (c.supports_tools === true || c.supports_tools === false) rec.supports_tools = c.supports_tools;
  if (c.reasoning_efforts && c.reasoning_efforts.length) rec.reasoning_efforts = c.reasoning_efforts.slice();
  if (c.default_reasoning_effort) rec.default_reasoning_effort = c.default_reasoning_effort;
  if (c.cap_source) rec.cap_source = c.cap_source;
  return rec;
}

// capSummaryHtml 上游行后面的能力摘要：拉到什么写什么，拉不到就标「未探测」。
// 这里绝不能出现「默认 1M」这类括号里补出来的字——那一行就是用户判断
// 「该不该手工补填」的唯一依据（R119，推翻旧版编造默认值）。
function capSummaryHtml(c) {
  if (!c) return `<span class="cm-meta cap-unknown" title="上游没给出该模型的能力/上下文信息">未探测</span>`;
  const bits = [];
  if (c.context_window) bits.push(`上下文 ${formatTokenCount(c.context_window)}`);
  if (c.max_tokens) bits.push(`输出 ${formatTokenCount(c.max_tokens)}`);
  if (c.supports_images) bits.push("识图");
  if (c.supports_reasoning) bits.push("思考");
  if (c.supports_tools === false) bits.push("无工具");
  if (c.reasoning_efforts && c.reasoning_efforts.length) bits.push(`档位 ${c.reasoning_efforts.join("/")}`);
  if (!bits.length) return `<span class="cm-meta cap-unknown" title="上游只给了模型 ID，没有能力字段">未探测</span>`;
  return `<span class="cm-meta cap-api">${esc(bits.join("·"))}</span>`;
}

// applyImportedCap 对「已添加」的模型只覆写能力字段（保留模型 ID 与其他改动）：
// 旧版导入把 1M/64k 这类编造值写进了配置，重新拉取后必须能就地把它们洗成上游真值，
// 否则用户只能删了重新加。
function applyImportedCap(i) {
  const ch = editCustomChannels[mgrChIdx];
  const row = upstreamModels[i];
  if (!ch || !row || !row.cap) return;
  const k = (ch.models || []).findIndex((m) => (typeof m === "string" ? m : m.id) === row.id);
  if (k < 0) return;
  const cap = capToModelConfig(row.cap);
  cap.id = row.id;
  ch.models[k] = cap;
  $("customChOk").textContent = `已用上游能力覆写 ${row.id}，点击「保存全部配置」生效`;
  renderCustomChList();
  renderImportList();
}


// validateBackupProxy 校验备用上游通道地址，返回 [url, err]（空串合法 = 关闭）。
// 判据与后端 backuplink.Parse 同源：仅 http/https/socks5，且必须有主机名。
function validateBackupProxy() {
  const v = $("backupProxyInput").value.trim();
  if (!v) return ["", ""];
  let u;
  try { u = new URL(v); } catch { return [null, `备用代理地址无效：${v}`]; }
  if (!["http:", "https:", "socks5:"].includes(u.protocol)) {
    return [null, `备用代理协议不支持（仅 http/https/socks5）：${v}`];
  }
  if (!u.hostname) return [null, `备用代理缺少主机名：${v}`];
  return [v, ""];
}

// testOczenKey 「测试」按钮：用输入框当前的候选 key（含脱敏值/空）调后端实测。
// 后端逻辑：候选 key 临时写入渠道 client → big-pickle 发最小对话 → 200/429 均算通过
// （200 = 配额正常；429 = 连通但匿名共享配额限流，属预期现象）；测试后恢复配置原值。
// 与真实对话同源（issue #117）：直连被地区拒时后端会自动改走备用通道，
// via_backup 标出答复来自哪个出口——否则用户配了代理点测试仍看到 403 会误以为功能没生效。
async function testOczenKey() {
  const btn = $("btnOczenTest");
  if (btn.disabled) return;
  btn.disabled = true;
  const old = btn.textContent;
  btn.textContent = "测试中…";
  $("proxyErr").textContent = "";
  try {
    let k = $("oczenKeyInput").value.trim();
    if (isMaskedKey(k)) k = ""; // 脱敏形态 = 未改动，用当前配置值测
    const r = await api("/api/config/oczen_test", { api_key: k });
    const via = r.via_backup ? "经备用通道" : "直连";
    if (r.ok) {
      toast(r.status === 200 ? `✓ OpenCodeZen 凭证可用（200，${via}）` : `✓ 连通正常（429 匿名限流，属预期，${via}）`);
    } else {
      const brief = (r.body || r.error || "").slice(0, 90).replace(/\s+/g, " ");
      // 403 且经直连 = 最可能是地区限制，而备用通道未配置/未生效：给出可行动提示
      const hint = (!r.via_backup && r.status === 403) ? "（未走备用通道；若已配代理仍为直连，说明本次不是地区拒绝或备用链路拨不动，详见日志）" : "";
      toast(`✗ 测试失败 HTTP ${r.status}${hint}：${brief}`);
    }
  } catch (e) {
    toast("✗ 测试失败：" + e.message);
  } finally {
    btn.disabled = false;
    btn.textContent = old;
  }
}

async function saveSettings() {
  // 1) 监听地址 + 管理密码（同请求提交：后端先存密码再切监听）
  let host = $("selHost").value;
  if (host === "__custom__") host = $("inHost").value.trim() || "127.0.0.1";
  const port = parseInt($("inPort").value, 10);
  const adminPass = $("adminPassInput").value.trim();
  const listenBody = { host, port };
  if (adminPass) listenBody.admin_password = adminPass; // 空 = 不改动，避免覆盖已设密码
  try {
    await api("/api/config/listen", listenBody);
  } catch (e) { toast(e.message); return; }
  // 刚设置/修改密码：旧会话已被作废，立即用新密码重新登录，保证本次保存的后续步骤可用。
  // （从「本机监听 + 无密码」切到 0.0.0.0 时鉴权刚启用，此前根本没有 cookie。）
  if (adminPass) {
    try {
      const r = await api("/api/auth/login", { password: adminPass });
      authSession = r.session || "";
    } catch (e) { toast(e.message); closeSettings(); showLogin("管理密码已变更，请重新登录"); return; }
  }

  // 2) 备用上游通道（先本地校验，失败阻断保存）+ oczen key（仅在用户改动过时提交，避免把脱敏回显值存回）
  const [proxy, perr] = validateBackupProxy();
  if (perr) { $("proxyErr").textContent = perr; toast(perr); return; }
  const body = { proxy };
  const kIn = $("oczenKeyInput");
  if (kIn.dataset.touched === "1") {
    let k = kIn.value.trim();
    // 值仍是脱敏形态（含 …）则视为未修改，保持服务端现值
    if (k && isMaskedKey(k)) k = undefined;
    if (k !== undefined) body.oczen_api_key = k; // undefined 时不携带字段 = 不改动；空串 = 清除回匿名
  }
  try {
    await api("/api/config/proxy", body);
  } catch (e) { toast(e.message); return; }

  // 3) 模型映射：优先读编辑器；编辑器从未展开过则用原值（保证「只改监听/渠道不碰映射」）
  let modelMap = state.compat?.model_map || {};
  if (!$("mapEditor").classList.contains("hidden")) {
    const [parsed, err] = parseMapText($("mapText").value);
    if (err) { $("mapErr").textContent = err; toast(err); return; }
    modelMap = parsed;
  }
  const defaultChannel = $("selCh").value;
  const maxTokensCap = parseInt($("inMaxTok").value, 10) || 0;
  try {
    await api("/api/config/compat", { default_channel: defaultChannel, max_tokens_cap: maxTokensCap, model_map: modelMap });
  } catch (e) { toast(e.message); return; }

  // 4) 思考链防癫开关（独立端点即时生效）
  const loopGuardOn = $("chkLoopGuard").checked;
  if (loopGuardOn !== (state.loop_guard !== false)) {
    try {
      await api("/api/config/loop_guard", { on: loopGuardOn });
    } catch (e) { toast(e.message); return; }
  }

  // 5) API-Key（最后保存：改 Key 可能影响当前会话的后续请求）
  try {
    await api("/api/config/api_key", { key: $("keyInput").value.trim() });
  } catch (e) { toast(e.message); return; }

  // 6) 密码已在第 1 步生效（会话已换新），无需再强制重登
  toast(adminPass ? "设置已保存，管理密码已生效" : "设置已保存");
  closeSettings();
  loadState();
}

// clearAdminPassword 「清除」按钮：关闭面板鉴权（仅环回监听允许，后端会校验）。
async function clearAdminPassword() {
  if (!confirm("确定清除管理密码？清除后面板将不再鉴权（仅允许监听 127.0.0.1）。")) return;
  try {
    await api("/api/config/admin_password", { password: "" });
  } catch (e) { toast(e.message); return; }
  authSession = "";
  toast("管理密码已清除，面板鉴权已关闭");
  closeSettings();
  await ensureSession();
}
