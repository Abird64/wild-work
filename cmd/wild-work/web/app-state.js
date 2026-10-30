"use strict";
// ---------- 全局状态 ----------
let state = null;

// ---------- 数据加载 ----------
async function loadState() {
  state = await api("/api/state");
  // 账号数据已变（刷新/签到/增删），明细缓存随之失效，避免 tooltip 展示旧余额。
  detailCache = {};
  render();
}

async function loadFees() {
  try {
    const fees = await api("/api/fees");
    lastFees = fees; // 供模型查找面板取全渠道模型（纯前端，不改后端）
    renderFees(fees);
  } catch (e) { /* 费率接口失败不阻塞 */ }
}

async function refreshFees() {
  $("btnRefreshFees").disabled = true;
  try {
    await api("/api/fees/refresh", {});
    const fees = await api("/api/fees");
    lastFees = fees;
    renderFees(fees);
    toast("模型列表和费率已刷新");
  } catch (e) { toast(e.message); } finally {
    $("btnRefreshFees").disabled = false;
  }
}

// ---------- 模型查找（纯前端：从最近一次 /api/fees 结果里按关键字过滤） ----------
// lastFees 最近一次费率接口响应（loadFees/refreshFees 填充）；查找面板读它取全渠道模型。
let lastFees = null;

// lastStats 最近一次 /api/stats 响应（loadStats 填充）。
// 为什么留一份而不是每次现拉：三场景推荐排序（rankCandidates）需要 models/platforms 活值，
// 而 loadStats 已经在轮询；复用同一份就不会出现「面板显示旧统计、推荐用另一份更新」的两套口径。
let lastStats = null;
