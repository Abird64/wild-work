"use strict";
// ---------- 显示名修改弹层 ----------
let renameUid = null;

function openRename(uid, currentName) {
  if (uid === "oczen-anon") { toast("OpenCodeZen 匿名通道账号不可改名"); return; }
  if (uid === "kilo-anonymous") { toast("Kilo 免费池账号不可改名"); return; }
  renameUid = uid;
  $("rnUid").textContent = shortUid(uid);
  $("rnInput").value = currentName === shortUid(uid) ? "" : (currentName || "");
  $("renameOverlay").classList.remove("hidden");
  $("rnInput").focus();
}

function closeRename() {
  $("renameOverlay").classList.add("hidden");
  renameUid = null;
}

async function saveRename() {
  if (!renameUid) return;
  const nickname = $("rnInput").value.trim();
  if (!nickname) { toast("显示名不能为空"); return; }
  try {
    await api("/api/account/nickname", { uid: renameUid, nickname });
    toast("显示名已更新");
    closeRename();
    loadState();
  } catch (e) { toast(e.message); }
}

// ---------- 模型映射编辑器 ----------

// CHANNEL_PRESETS 各渠道的缺省建议映射（模型为该渠道常用/免费模型）。
// 用途：未绑定某渠道账号、或不知道该渠道有哪些模型时，给出可点选的起点。
const CHANNEL_PRESETS = {
  workbuddy:   { label: "Claude Code → workbuddy",  items: ["claude-* = workbuddy/glm-5.2", "claude-sonnet-* = workbuddy/kimi-k2.7"] },
  traework:    { label: "Codex → traework",          items: ["gpt-5* = traework/glm-5.2", "codex-* = traework/DeepSeek-V4-Pro"] },
  // TraeCode 与 TraeWork 共用账号，预设沿用 Codex 语义（面向代码场景的新版模型）。
  traecode:    { label: "Codex → traecode",          items: ["gpt-5* = traecode/deepseek-v4.1-flash", "codex-* = traecode/glm-5.3-flash"] },
  workbuddyai: { label: "Claude Code → workbuddyai", items: ["claude-* = workbuddyai/deepseek-v4.1-flash"] },
  qodercn:     { label: "→ qodercn",                 items: ["gpt-* = qodercn/glm-5.3"] },
  qodercom:    { label: "→ qodercom",                items: ["gpt-* = qodercom/glm-5.3"] },
  qwenwork:    { label: "→ qwenwork",                items: ["gpt-* = qwenwork/flash", "claude-* = qwenwork/pro"] },
  glm:         { label: "→ glm (智谱清言)",           items: ["gpt-* = glm/chatglm", "claude-* = glm/chatglm-think"] },
  oczen:       { label: "→ oczen (匿名免费)",        items: ["claude-* = oczen/mimo-v2.6-flash-free", "gpt-* = oczen/big-pickle"] },
};

// renderMapPresets 按当前已接入渠道渲染缺省建议按钮。
// 仅列出「已接入（有账号）」的渠道——未绑定的渠道点了也会因无账号而失败，不给误导性入口。
function renderMapPresets(channels) {
  const bound = new Set((state.accounts || []).map(a => a.group));
  // TraeCode 与 TraeWork 共用账号，账号列表里只会出现 traework；
  // 但 TraeCode 是可独立路由的渠道，其预设也应可见。
  if (bound.has("traework")) bound.add("traecode");
  const box = $("mapPresets");
  box.innerHTML = channels.filter(c => CHANNEL_PRESETS[c] && bound.has(c)).map(c => {
    const p = CHANNEL_PRESETS[c];
    return `<span class="btn tiny preset" data-ch="${c}" title="${esc(p.items.join("\n"))}">${esc(p.label)}</span>`;
  }).join(" ") || `<span class="hint">（尚未接入任何渠道，先在账号管理添加账号）</span>`;
  box.querySelectorAll(".preset").forEach(b => {
    b.onclick = () => {
      const p = CHANNEL_PRESETS[b.dataset.ch];
      // 追加尚未存在的行，避免重复插入
      const cur = $("mapText").value.split(/\r?\n/).map(s => s.trim()).filter(Boolean);
      const have = new Set(cur.map(l => l.split("=")[0].trim()));
      const add = p.items.filter(it => !have.has(it.split("=")[0].trim()));
      if (!add.length) { toast(`${p.label} 的建议映射已存在`); return; }
      $("mapText").value = cur.concat(add).join("\n");
    };
  });
}

// toggleMapEditor 展开/收起映射编辑器。
function toggleMapEditor() {
  $("mapEditor").classList.toggle("hidden");
}

// parseMapText 校验编辑器内容，返回 (modelMap, 错误信息)。
// 语法错误就地提示并阻断保存，不再弹 prompt 循环。
function parseMapText(raw) {
  const map = {};
  const channels = new Set(state.compat?.channels || []);
  for (const line0 of raw.split(/\r?\n/)) {
    const line = line0.trim();
    if (!line || line.startsWith("#")) continue;
    const idx = line.indexOf("=");
    if (idx < 0) return [null, `格式错误（缺少 =）：${line}`];
    const k = line.substring(0, idx).trim(), v = line.substring(idx + 1).trim();
    if (!k || !v) return [null, `格式错误（键或值为空）：${line}`];
    const vi = v.indexOf("/");
    if (vi <= 0 || !v.substring(vi + 1).trim()) return [null, `映射目标必须是「渠道/模型」形式：${v}`];
    const ch = v.substring(0, vi);
    if (channels.size && !channels.has(ch)) {
      return [null, `未知渠道「${ch}」；已知渠道：${[...channels].join(", ")}`];
    }
    if (map[k] !== undefined) return [null, `重复的键：${k}`];
    map[k] = v;
  }
  return [map, ""];
}

// ---------- 复制到剪贴板 ----------
async function copyText(text, label) {
  try {
    await navigator.clipboard.writeText(text);
    toast(`${label}已复制到剪贴板`);
  } catch (e) {
    // 降级方案：execCommand
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    try {
      document.execCommand("copy");
      toast(`${label}已复制到剪贴板`);
    } catch (err) {
      toast("复制失败，请手动复制");
    }
    document.body.removeChild(ta);
  }
}
function openHelp() { $("helpOverlay").classList.remove("hidden"); }
function closeHelp() { $("helpOverlay").classList.add("hidden"); }
function openAbout() { $("aboutOverlay").classList.remove("hidden"); }
function closeAbout() { $("aboutOverlay").classList.add("hidden"); }

// ---------- 通用确认框 ----------
function confirmDialog(msg, onOk) {
  $("confirmMsg").textContent = msg;
  $("confirmOverlay").classList.remove("hidden");
  $("btnConfirmOk").onclick = () => { $("confirmOverlay").classList.add("hidden"); onOk(); };
  $("btnConfirmCancel").onclick = () => $("confirmOverlay").classList.add("hidden");
}
