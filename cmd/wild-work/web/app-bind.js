"use strict";
// ---------- 事件绑定 ----------
function bind() {
  $("btnAddWB").onclick = () => promptLogin("workbuddy");
  $("btnAddWBAI").onclick = () => promptLogin("workbuddyai");
  $("btnAddTrae").onclick = () => promptLogin("traework");
  $("btnAddQoderCN").onclick = () => promptLogin("qodercn");
  $("btnAddQoderCOM").onclick = () => promptLogin("qodercom");
  $("btnAddQwen").onclick = () => promptLogin("qwenwork");
  $("btnAddMonkeyCode").onclick = () => promptLogin("monkeycode");
  $("btnAddRaccoon").onclick = () => promptLogin("raccoon");
  $("btnAddLoomy").onclick = () => promptLogin("loomy");
  $("btnAddGLM").onclick = () => startGLMLogin();
  $("btnGLMSubmit").onclick = submitGLMToken;
  $("btnGLMCancel").onclick = cancelGLMLogin;
  $("btnGLMToggleManual").onclick = () => $("glmManual").classList.toggle("hidden");
  $("btnAddCline").onclick = () => promptLogin("cline");
  $("btnClineSubmit").onclick = submitClineKey;
  $("btnClineCancel").onclick = () => $("clineOverlay").classList.add("hidden");
  $("btnAddOfficeAce").onclick = () => promptLogin("officeace");
  $("btnAddMiniMax").onclick = () => promptLogin("minimax");
  $("btnAddLobsterAI").onclick = () => promptLogin("lobsterai");
  $("btnAddZCode").onclick = () => promptLogin("zcode");
  $("btnAddGemini").onclick = () => promptLogin("gemini");
  $("btnAddCodeArts").onclick = () => promptLogin("codearts");
  $("btnCheckinAll").onclick = checkinAll;
  $("btnRefreshAll").onclick = refreshAll;
  $("btnAddTime").onclick = addTime;
  $("btnTimeOk").onclick = commitTime;
  $("btnTimeCancel").onclick = closeTimeEditor;
  $("timePicker").onkeydown = (e) => {
    if (e.key === "Enter") { e.preventDefault(); commitTime(); }
    else if (e.key === "Escape") { e.preventDefault(); closeTimeEditor(); }
  };
  // 点选择器之外的地方即收起（与其它浮层同口径：不落库、不留半开状态）
  document.addEventListener("click", (e) => {
    const ed = $("timeEditor");
    if (ed.hidden) return;
    const t = e.target;
    if (ed.contains(t) || t === $("btnAddTime")) return;
    closeTimeEditor();
  });
  $("btnCopyUrl").onclick = copyUrl;
  $("btnSubmitCallback").onclick = submitCallback;
  $("btnCancelLogin").onclick = cancelLogin;
  $("btnRefreshFees").onclick = refreshFees;
  // 模型查找面板（纯前端）：入口按钮 + 关闭 + 输入即过滤 + 结果点击复制完整 ID。
  $("btnModelSearch").onclick = openModelSearch;
  $("btnModelSearchClose").onclick = closeModelSearch;
  // 设置弹层内「模型降级链 / 思考强度」标题右侧的查找入口：与首页共用同一套弹层与渲染，
  // 故只做入口接线（这些按钮在动态渲染的配置区之外，属静态 DOM，用属性选择器统一绑定）。
  for (const el of document.querySelectorAll("[data-model-search]")) {
    el.onclick = openModelSearch;
  }
  $("modelSearchOverlay").onclick = (e) => { if (e.target === $("modelSearchOverlay")) closeModelSearch(); };
  $("modelSearchOverlay").onkeydown = (e) => { if (e.key === "Escape") closeModelSearch(); };
  $("modelSearchInput").oninput = (e) => renderModelSearch(e.target.value);
  $("modelSearchInput").onkeydown = (e) => { if (e.key === "Escape") closeModelSearch(); };
  $("modelSearchList").onclick = (e) => {
    const row = e.target.closest(".ms-row");
    if (row) copyText(row.dataset.mid, "模型 ID");
  };
  $("modelSearchList").onkeydown = (e) => {
    if (e.key !== "Enter" && e.key !== " ") return;
    const row = e.target.closest(".ms-row");
    if (row) { e.preventDefault(); copyText(row.dataset.mid, "模型 ID"); }
  };
  $("chkAutostart").onchange = toggleAutostart;
  $("chkBodyLog").onchange = toggleBodyLog;
  $("btnAdminLogout").onclick = () => logout();
  $("btnClearAdminPass").onclick = clearAdminPassword;
  $("btnAdminLogin").onclick = submitLogin;
  $("adminLoginPass").onkeydown = (e) => { if (e.key === "Enter") submitLogin(); };

  $("apiAddr").onclick = () => {
    const v = $("apiAddr").querySelector(".val").textContent;
    copyText(v, "OpenAI 接口地址");
  };
  $("apiKeyDisplay").onclick = () => {
    const v = $("apiKeyDisplay").dataset.key;
    if (v === undefined || v === "") { toast("当前未设置 API-Key"); return; }
    copyText(v, "API-Key");
  };
  // 顶栏齿轮进入统一设置（点击地址/Key 文本仍为复制）
  $("btnSettings").onclick = openSettings;

  // 统一设置弹层
  $("btnSettingsSave").onclick = saveSettings;
  // btnSettingsCancel 已随设置弹层删除（设置现在是「其他」页，无「取消」概念：
  // 各字段本来就是「改完点保存」或「即时生效」两种，不存在整页回滚）。
  // 三场景路由（SR-005b）：复制当前配置 / 预检（dry_run，不写盘）/ 保存（存前静默复制旧值）
  $("btnSceneCopy").onclick = () => copyText(sceneConfigText(), "当前三场景配置");
  $("btnSceneDryRun").onclick = sceneDryRun;
  $("btnSceneSave").onclick = saveSceneRouting;
  // 卡片内交互全部委托在 #sceneCards 上（每轮 innerHTML 重建后委托仍有效）：
  // 主模型输入框 change = 确认回填；备胎 ↑↓ 改序、× 移除；追加框回车 = 追加。
  $("sceneCards").addEventListener("change", (e) => {
    if (!e.target.classList.contains("scene-from")) return;
    const alias = e.target.dataset.sceneFrom;
    const v = String(e.target.value || "").trim();
    const cur = String((sceneRules[alias] || {}).from || "");
    if (!v || v === cur) { e.target.value = cur; return; }
    // 失败时重渲染会把输入框刷回原值（错误文案留在 #sceneErr 里，不静默吞掉）
    sceneSetFrom(alias, v);
  });
  $("sceneCards").addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    if (!e.target.classList.contains("scene-to-add")) return;
    const alias = e.target.dataset.sceneAdd;
    const [tos, err] = parseSceneModelList(e.target.value);
    if (err) { $("sceneErr").textContent = err; return; }
    if (tos.length === 0) return;
    const before = (sceneRules[alias].to || []).length;
    for (const t of tos) sceneAddTo(alias, t);
    if ((sceneRules[alias].to || []).length > before) $("sceneErr").textContent = "";
  });
  $("sceneCards").addEventListener("click", (e) => {
    const up = e.target.closest("[data-scene-up]");
    if (up) { const [a, j] = up.dataset.sceneUp.split(":"); sceneMoveTo(a, +j, -1); return; }
    const down = e.target.closest("[data-scene-down]");
    if (down) { const [a, j] = down.dataset.sceneDown.split(":"); sceneMoveTo(a, +j, 1); return; }
    const del = e.target.closest("[data-scene-del]");
    if (del) { const [a, j] = del.dataset.sceneDel.split(":"); sceneDelTo(a, +j); return; }
  });
  // 推荐候选列表（SR-005c）：展开/收起 + 切场景 tab + 行内的「设为主模型 / 加为备胎」。
  // 行内动作全部**只改编辑态**（sceneRules），落盘仍要用户点「保存三场景」——
  // 推荐列表不是第二个写入口（SD-6 只有一条）。
  $("btnSceneRec").onclick = toggleSceneRec;
  $("sceneRecTabs").addEventListener("click", (e) => {
    const b = e.target.closest("[data-rec-tab]");
    if (!b) return;
    // 切 tab 也重算（SD-7：不是只换显示，而是按该场景重新排序）
    sceneRecTab = b.dataset.recTab;
    renderSceneRec();
  });
  $("sceneRecBody").addEventListener("click", (e) => {
    const set = e.target.closest("[data-rec-set]");
    if (set) {
      sceneSetFrom(sceneRecTab, set.dataset.recSet);
      renderSceneRec(); // 主模型变了 ⇒ 该场景的排序与互备提示都要跟着变
      return;
    }
    const add = e.target.closest("[data-rec-add]");
    if (add) { sceneAddTo(sceneRecTab, add.dataset.recAdd); renderSceneRec(); return; }
  });
  $("btnCompatMap").onclick = toggleMapEditor;
  $("selHost").onchange = () => {
    $("customHostRow").classList.toggle("hidden", $("selHost").value !== "__custom__");
    syncListenRisk(); // 监听地址变化 → 实时间同步警告条与必填星号
  };
  // 自定义主机名边输边判（可能一开始就填着非本机地址）
  $("inHost").addEventListener("input", syncListenRisk);
  $("keyInput").addEventListener("keydown", (e) => { if (e.key === "Enter") saveSettings(); });

  // 显示名修改弹层
  $("btnRenameSave").onclick = saveRename;
  $("btnRenameCancel").onclick = closeRename;
  $("rnInput").addEventListener("keydown", (e) => { if (e.key === "Enter") saveRename(); });
  // oczen key 用户改动标记：避免把脱敏回显值误存回
  $("oczenKeyInput").addEventListener("input", (e) => { e.target.dataset.touched = "1"; });
  $("btnOczenTest").onclick = testOczenKey;

  // 模型降级链：添加规则 / 保存 / 列表内事件委托（删规则、删备胎、追加备胎、回车添加、拖拽排序）
  $("btnFbAdd").onclick = () => {
    const from = $("fbFromInput").value.trim();
    if (!/^[^/]+\/.+/.test(from)) { $("fbErr").textContent = "主力模型ID 需为 channel/model 形式（如 workbuddyai/deepseek-v4.1-flash）"; return; }
    if (fbRules.some((r) => r.from === from)) { $("fbErr").textContent = `主模型 ${from} 已有规则（每个主模型只允许一条）`; return; }
    const [tos, err] = parseModelList($("fbToInput").value);
    if (err) { $("fbErr").textContent = err; return; }
    if (tos.length === 0) { $("fbErr").textContent = "至少填一个备胎模型ID"; return; }
    // 只拦自环（备胎 == 本规则主力）：这是唯一不合法形态。
    // **不再过滤其它规则的主力**——那会把「互为备胎」当互斥脏数据挡掉（SR-003 / SD-3 甲）。
    const loops = tos.filter((t) => t === from);
    const kept = tos.filter((t) => t !== from);
    if (kept.length === 0) { $("fbErr").textContent = "备胎不能是它自己的主力模型（自环），请更换备胎"; return; }
    if (loops.length > 0) toast(`已忽略 ${loops.length} 个自环备胎（与主模型 ${from} 相同）`);
    const others = fbFromSet();
    const crossScene = kept.filter((t) => others[t]);
    fbRules.push({ from, to: kept });
    $("fbFromInput").value = ""; $("fbToInput").value = "";
    renderFbList(); // 渲染只读：不再做任何清洗
    if (crossScene.length > 0) {
      toast(`已加入 ${crossScene.length} 个互备（${crossScene.join("、")} 是其它场景的主力，属合法设计；点保存生效）`);
    }
  };
  $("btnFbSave").onclick = saveModelFallback;
  $("fbList").addEventListener("click", async (e) => {
    const addBtn = e.target.closest("[data-add-to]");
    if (addBtn) {
      const i = +addBtn.dataset.addTo;
      const inp = document.querySelector(`.fb-to-add[data-add-idx="${i}"]`);
      const [tos, err] = parseModelList(inp && inp.value);
      if (err) { $("fbErr").textContent = err; return; }
      if (tos.length === 0) { $("fbErr").textContent = "先在输入框填备胎模型ID 再点 ＋"; return; }
      const r = fbRules[i];
      if (!r) return;
      const fresh = tos.filter((t) => t !== r.from && !r.to.includes(t)); // 仅拦自环 + 去重
      const loopCnt = tos.filter((t) => t === r.from).length;
      if (fresh.length === 0) {
        $("fbErr").textContent = loopCnt === tos.length
          ? "备胎不能是它自己的主力模型（自环）"
          : "这些模型已在该链上（无需重复添加）";
        return;
      }
      r.to.push(...fresh);
      renderFbList();
      if (loopCnt > 0) toast(`已忽略 ${loopCnt} 个自环备胎`);
      return;
    }
    const delTo = e.target.closest("[data-del-to]");
    if (delTo) {
      const [i, j] = delTo.dataset.delTo.split(":").map(Number);
      fbRules[i].to.splice(j, 1);
      renderFbList();
      return;
    }
    const delRule = e.target.closest("[data-del-rule]");
    if (delRule) {
      fbRules.splice(+delRule.dataset.delRule, 1);
      renderFbList();
    }
  });
  // 行内备胎输入框回车 = 追加到该规则（事件委托 keydown）；主模型输入框回车 = blur 确认。
  // combobox 的 Enter 在下拉开着时已被其自身 stopPropagation 消化，走到这里的必是「下拉关闭态」。
  $("fbList").addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    if (e.target.classList.contains("fb-from-edit")) { e.target.blur(); return; }
    if (!e.target.classList.contains("fb-to-add")) return;
    const i = +e.target.dataset.addIdx;
    const [tos, err] = parseModelList(e.target.value);
    if (err) { $("fbErr").textContent = err; return; }
    if (tos.length === 0) return;
    const r = fbRules[i];
    if (!r) return;
    const fresh = tos.filter((t) => t !== r.from && !r.to.includes(t)); // 仅拦自环 + 去重
    const loopCnt = tos.filter((t) => t === r.from).length;
    if (fresh.length === 0) {
      $("fbErr").textContent = loopCnt === tos.length
        ? "备胎不能是它自己的主力模型（自环）"
        : "这些模型已在该链上（无需重复添加）";
      return;
    }
    r.to.push(...fresh);
    renderFbList();
    if (loopCnt > 0) toast(`已忽略 ${loopCnt} 个自环备胎`);
  });
  // 行内主模型 ID 编辑：change（失焦/回车后 blur）提交——形态校验 + 主模型唯一性校验，
  // 失败回滚输入框；成功写回 fbRules 并滤掉与新 from 自环的备胎，重渲染刷新 title。
  $("fbList").addEventListener("change", (e) => {
    if (!e.target.classList.contains("fb-from-edit")) return;
    const i = +e.target.dataset.editIdx;
    const from = String(e.target.value || "").trim();
    const cur = fbRules[i].from;
    if (!from || from === cur) { e.target.value = cur; return; }
    if (!/^[^/]+\/.+/.test(from)) { $("fbErr").textContent = `主力模型ID ${from} 需为 channel/model 形式（如 workbuddyai/deepseek-v4.1-flash）`; e.target.value = cur; return; }
    if (fbRules.some((r, k) => k !== i && r.from === from)) { $("fbErr").textContent = `主模型 ${from} 已有规则（每个主模型只允许一条）`; e.target.value = cur; return; }
    // 改成新 from 后，链上若已有同名项就成了自环（唯一不合法形态）⇒ 整次改名拒绝并给出文案，
    // 而不是顺手把那个备胎删掉（渲染/编辑路径一律不得静默删除数据，SR-003 / G1）。
    if ((fbRules[i].to || []).includes(from)) {
      $("fbErr").textContent = `主模型不能改成 ${from}：它已在第 ${i + 1} 条规则的备胎链上（会成为自环），请先移除该备胎`;
      e.target.value = cur;
      return;
    }
    fbRules[i].from = from;
    renderFbList();
  });
  // 备胎 chip 拖拽排序（委托在 #fbList；仅同规则内拖动）。drop 后就地 splice fbRules[i].to
  // 并重渲染（chip 序号随之刷新）；只改编辑态，点「保存降级链」才落盘。
  // 从 × 删除链接起手不拖（chip 内 a[data-del-to] 命中时跳过 dragstart）。
  let fbDragSrc = null; // { i, j, el }
  $("fbList").addEventListener("dragstart", (e) => {
    const chip = e.target.closest(".fb-to-chip");
    if (!chip || e.target.closest("[data-del-to]")) { e.preventDefault(); return; }
    const [i, j] = chip.dataset.chip.split(":").map(Number);
    fbDragSrc = { i, j, el: chip };
    chip.classList.add("dragging");
    try { e.dataTransfer.setData("text/plain", "x"); e.dataTransfer.effectAllowed = "move"; } catch (err) {}
  });
  $("fbList").addEventListener("dragover", (e) => {
    if (!fbDragSrc) return;
    e.preventDefault();
    const chip = e.target.closest(".fb-to-chip");
    // 清掉上一轮指示条，再按指针相对 chip 左右半标记插入位
    document.querySelectorAll(".fb-to-chip.drop-before,.fb-to-chip.drop-after").forEach((c) => c.classList.remove("drop-before", "drop-after"));
    if (!chip || chip === fbDragSrc.el) return;
    const r = chip.getBoundingClientRect();
    chip.classList.add(e.clientX - r.left < r.width / 2 ? "drop-before" : "drop-after");
  });
  $("fbList").addEventListener("drop", (e) => {
    e.preventDefault();
    const chip = e.target.closest(".fb-to-chip");
    const mark = document.querySelector(".fb-to-chip.drop-before, .fb-to-chip.drop-after");
    if (chip && mark && fbDragSrc && chip.dataset.chip && fbDragSrc.el.dataset.chip) {
      const [si, sj] = fbDragSrc.el.dataset.chip.split(":").map(Number);
      const [ti] = chip.dataset.chip.split(":").map(Number);
      if (si === ti) { // 仅同规则内拖动
        const before = mark.classList.contains("drop-before");
        let dj = +chip.dataset.chip.split(":")[1] + (before ? 0 : 1);
        const arr = fbRules[si].to;
        const [moved] = arr.splice(sj, 1);
        if (dj > sj) dj--; // 移除原位后修正目标下标
        arr.splice(dj, 0, moved);
        renderFbList();
        toast("顺序已调整（序号已同步），点「保存降级链」生效");
      }
    }
    document.querySelectorAll(".fb-to-chip.drop-before,.fb-to-chip.drop-after").forEach((c) => c.classList.remove("drop-before", "drop-after"));
    fbDragSrc = null;
  });
  $("fbList").addEventListener("dragend", () => {
    if (fbDragSrc) fbDragSrc.el.classList.remove("dragging");
    fbDragSrc = null;
    document.querySelectorAll(".fb-to-chip.drop-before,.fb-to-chip.drop-after").forEach((c) => c.classList.remove("drop-before", "drop-after"));
  });

  // 思考强度：添加映射 / 保存 / 列表内删除 / 回车添加
  $("btnEffAdd").onclick = () => {
    const model = $("effModelInput").value.trim();
    const effort = $("effEffortInput").value.trim();
    if (!model) { $("effErr").textContent = "模型ID 不能为空（全局默认请在上方「全局默认」行填）"; return; }
    if (model === "*") { $("effErr").textContent = "* 是全局默认专用键，请在上方「全局默认」行填"; return; }
    if (!effort) { $("effErr").textContent = "思考档位不能为空"; return; }
    if (effRules.some((r) => r.model === model)) { $("effErr").textContent = `模型 ${model} 已有档位（每个模型只允许一条）`; return; }
    effRules.push({ model, effort });
    $("effModelInput").value = ""; $("effEffortInput").value = "";
    renderEffList();
  };
  $("btnEffSave").onclick = saveReasoningEfforts;
  $("effList").addEventListener("click", (e) => {
    const del = e.target.closest("[data-del-eff]");
    if (del) {
      effRules.splice(+del.dataset.delEff, 1);
      renderEffList();
    }
  });

  // 配置区模糊搜索下拉接线（静态输入框一次性挂；行内输入框由 attachFbPickers 每轮重挂）。
  // 回车链：底部主力框 → 跳备胎框；备胎框 → 添加规则；映射模型框 → 跳档位框；档位框 → 添加映射。
  attachModelPicker($("fbFromInput"), {
    excludeSet: () => fbFromSet(), // 已配置的主力从候选隐藏（重复由下方添加校验兜底），与备胎框行为对齐
    enterAction: () => $("fbToInput").focus(),
  });
  attachModelPicker($("fbToInput"), {
    stayOpen: true, multiAccum: true, // 累积多选：点选逐个逗号追加，末段继续搜
    excludeSet: () => fbFromSet(), // 已配置的主力模型ID从备胎候选隐藏（实时取，主力变更即同步）
    enterAction: () => $("btnFbAdd").onclick(),
  });
  attachModelPicker($("effModelInput"), { enterAction: () => $("effEffortInput").focus() });
  attachEffortPicker($("effGlobalInput"));
  attachEffortPicker($("effEffortInput"), { enterAction: () => $("btnEffAdd").onclick() });
  // 全局默认框手输/选择后即时刷新未保存提示（effRules 变化经 renderEffList 已覆盖）
  $("effGlobalInput").addEventListener("input", updateEffDirty);

  // 自定义渠道（R66）：测试、停用/启用、删除、添加、保存
  $("customChList").addEventListener("click", async (e) => {
    const editChBtn = e.target.closest("[data-edit-custom]");
    if (editChBtn) {
      openChannelEditor(+editChBtn.dataset.editCustom);
      return;
    }
    // 模型行：编辑（进模型面板的手填页签，R122）
    const editModelBtn = e.target.closest("[data-edit-model-in]");
    if (editModelBtn) {
      const [ci, mi] = editModelBtn.dataset.editModelIn.split(":").map(Number);
      openModelPick(ci, "manual", mi);
      return;
    }
    const delModelBtn = e.target.closest("[data-del-model-in]");
    if (delModelBtn) {
      const [ci, mi] = delModelBtn.dataset.delModelIn.split(":").map(Number);
      const ch = editCustomChannels[ci];
      if (ch && ch.models && ch.models[mi] !== undefined) {
        ch.models.splice(mi, 1);
        renderCustomChList();
        $("customChOk").textContent = "已移除模型，点「保存」生效";
      }
      return;
    }
    // Provider 行：「＋ 模型」直接进面板上游列表页签（R122：导入与手填已合并）
    const addModelBtn = e.target.closest("[data-add-model-in]");
    if (addModelBtn) {
      openModelPick(+addModelBtn.dataset.addModelIn, "api");
      return;
    }
    const delBtn = e.target.closest("[data-del-custom]");
    if (delBtn) {
      const idx = +delBtn.dataset.delCustom;
      editCustomChannels.splice(idx, 1);
      renderCustomChList();
      return;
    }
    // Provider 行的脱敏 key 位：点一下复制完整明文（R119）
    const keyChip = e.target.closest(".cm-key");
    if (keyChip && keyChip.dataset.chName) copyCustomKey(keyChip.dataset.chName);
  });

  // R82：新增渠道走独立弹窗（不再依赖底部输入行）
  $("btnCustomAdd").onclick = () => openChannelEditor(-1);
  // ---- 自定义模型弹窗（R85）----
  $("btnOpenCustomModels").onclick = () => openCustomModels();
  // 关闭 = 有改动先保存（R122），与「保存」区别只在关掉窗口；保存失败不关窗。
  $("btnCustomClose").onclick = () => closeCustomModels();
  $("btnCustomSave").onclick = () => saveCustomChannels();
  $("customModelsOverlay").onclick = (e) => { if (e.target === $("customModelsOverlay")) closeCustomModels(); };


  // ---- 渠道编辑弹窗（R82） ----
  $("btnCeCancel").onclick = () => closeChannelEditor();
  $("btnCeSniff").onclick = () => sniffFromEditor(); // R121：规范化 + 确认真实端点，结论回填输入框
  $("btnCeSave").onclick = () => saveChannelEditor();
  // 切鉴权方式时改占位提示（basic 要 app_key:app_secret，bearer 才是「可选」）
  $("ceAuth").onchange = () => syncCeKeyHint();
  $("channelEditorOverlay").onclick = (e) => { if (e.target === $("channelEditorOverlay")) closeChannelEditor(); };
  $("ceURL").onkeydown = (e) => { if (e.key === "Enter") saveChannelEditor(); };

  // ---- 模型面板（R122：上游列表 + 手填合成一个 tab panel）----
  $("btnPickClose").onclick = () => closeModelPick();
  $("modelPickOverlay").onclick = (e) => { if (e.target === $("modelPickOverlay")) closeModelPick(); };
  $("modelPickOverlay").addEventListener("click", (e) => {
    const t = e.target.closest("[data-pick-tab]");
    if (!t) return;
    e.preventDefault();
    // 点「手动添加 / 编辑」要带一张干净表单进来（否则会看到上一个模型的残留值）
    if (t.dataset.pickTab === "manual") openModelEditor(-1);
    else switchPickTab("api");
  });
  $("btnImportAgain").onclick = () => loadUpstreamModels();
  $("btnMeSave").onclick = () => saveModelEditor();
  $("meId").onkeydown = (e) => { if (e.key === "Enter") saveModelEditor(); };
  // 行尾 ＋/− 与「更新能力」：列表整块重建，一律委托在容器上。
  // 不用 label 包 input（旧版双切换 Bug 就这来的），按钮就是按钮。
  $("importList").onclick = (e) => {
    const tg = e.target.closest("[data-pick-toggle]");
    if (tg) { togglePickModel(+tg.dataset.pickToggle); return; }
    const capBtn = e.target.closest("[data-apply-cap]");
    if (capBtn) applyImportedCap(+capBtn.dataset.applyCap);
  };
  $("importFilter").oninput = () => renderImportList();

  // 眼睛图标（R122）：编辑渠道时也能看明文，与复制链路同样「点一下才取那一条」。
  $("btnCeEye").onclick = () => toggleCeKeyEye();

  $("renameOverlay").onclick = (e) => { if (e.target === $("renameOverlay")) closeRename(); };
  $("helpOverlay").onclick = (e) => { if (e.target === $("helpOverlay")) closeHelp(); };

  // 积分明细浮窗的外部关闭路径（✕ / 点浮窗之外 / Esc）：document 级委托，绑定一次。
  // 浮窗与账号卡片都会随时重建，委托不受重建影响。
  bindCreditDetailDismiss();

  $("btnHelp").onclick = openHelp;
  $("btnAbout").onclick = openAbout;
  $("btnHelpClose").onclick = closeHelp;
  $("btnAboutClose").onclick = closeAbout;

  // 登录确认弹层
  $("btnLoginConfirm").onclick = confirmLogin;
  $("btnLoginConfirmCancel").onclick = () => $("loginConfirmOverlay").classList.add("hidden");
  $("loginConfirmOverlay").onclick = (e) => { if (e.target === $("loginConfirmOverlay")) $("loginConfirmOverlay").classList.add("hidden"); };

  $("aboutOverlay").onclick = (e) => { if (e.target === $("aboutOverlay")) closeAbout(); };
}

// ---------- 用量与流水面板（变量声明必须在 init 执行前——bindUsage() 在 init 顶部调用） ----------
// usageDays 从 localStorage 恢复上次选择（今日/7日），刷新页面不重置回 7 日。
// 存储键与 TOAST_SEEN_KEY 同族（usage-range-seen）；非法值回退默认 7。
// ⚠️ 本段必须在 init() 之前执行：bindUsage() 在 init 顶部被调用并引用 usageDays，
//    若声明滞后于 init（IIFE 立即执行），let 处于暂时性死区（TDZ）会抛
//    ReferenceError 中断整个 init —— 实测表现：账号卡片/费率表全空白、
//    控制台报 "Cannot access 'usageDays' before initialization"。
const USAGE_DAYS_KEY = "usage-range-seen";
const USAGE_DAYS_VALID = [1, 7];
let usageDays = 7;
try {
  const v = parseInt(localStorage.getItem(USAGE_DAYS_KEY) || "", 10);
  if (USAGE_DAYS_VALID.includes(v)) usageDays = v;
} catch (e) { /* localStorage 不可用时保持默认 */ }
// persistUsageDays 落盘当前区间（不可用存储时静默失败，不阻塞功能）。
function persistUsageDays() {
  try { localStorage.setItem(USAGE_DAYS_KEY, String(usageDays)); } catch (e) { /* 忽略 */ }
}
let recentAll = [];       // 全量最近流水（分页源，时间升序）
let recentPage = 0;
let lastNames = {};       // 最近一次渲染的 uid→昵称表（翻页时复用）
const RECENT_PAGE_SIZE = 20;
const MODEL_TOP_N = 20;   // 模型榜只保留前 N 名
