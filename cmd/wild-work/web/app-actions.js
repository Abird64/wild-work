"use strict";
// ---------- 账号操作 ----------
// 签到失败信息友好化：登录态失效形态（401 等）追加可操作指引（#134）。
function friendlyCheckinMsg(m) {
  return /401|unauthorized|登录已失效|登录已过期/i.test(m || "")
    ? m + "（该账号登录已过期，点账号卡片右上角 ⟳ 重新登录）" : m;
}
// checkinToast 签到后的提示文案。
// 为什么不能一律写「签到成功」：Qoder 双区的 campaigns 路径有三个**都是 ok=true** 的结果——
// 真领到（msg「签到成功 +100」）/ 今日已领（msg「今日已领取 +100」）/ 上游还没发可领活动
// （no_campaign，msg 是诊断摘要）。旧文案把第三种也说成「签到成功：上游未下发可领取活动…」，
// 读起来像领到了，是 10-17「签到没实现」误判的来源之一。这里按 msg 区分，绝不越权声称。
//
// 第三档再分「失败」与「暂未领到」：后端在 ok=true 下也可能给出失败形态的 msg
// （如「领取失败 http 400: …」「claim parse: …」），那是**真失败**，说成「暂未领到」
// 等于换了说法的假成功。判据放在兜底分支之前，不给「先被 +N 命中」的机会。
const CHECKIN_FAIL_RE = /失败|http \d{3}|error|无法解析|parse|未知状态|401|403|500/i;
// 第五档「无领取资格」（2026-10-18 用户实测定性）：该活动由**上游按账号授予资格**，
// 不是每个号都有；没资格的号在官方客户端里同样看不到入口。后端会直说「该账号无积分
// 领取活动资格……不是本工具的问题，重试无意义」。
// 为什么必须单独成档：这条 msg 既不含「失败」也不含错误码，会落到兜底分支被说成
// 「暂未领到」，再叠加下面那句「窗口内会自动重试」——对一个永远不会有结果的账号，
// 等于让用户一直等一个不会发生的事。这正是「以为工具没实现」的最后一处来源。
const CHECKIN_NO_ELIGIBILITY_RE = /无积分领取活动资格|无领取资格/;
function checkinToast(r) {
  const msg = r.msg || "";
  // 无资格判据放在最前：2026-11-10 起它是**终态**（ok=true / retryable=false），
  // 但终态 ≠ 领到积分，所以绝不能落到「签到成功」那一档；反之若某渠道给出
  // ok=false 的无资格（历史数据），也不该被兜底说成「暂未领到」。
  if (CHECKIN_NO_ELIGIBILITY_RE.test(msg)) return `无领取资格：${msg}`;
  if (!r.ok) return `签到：${friendlyCheckinMsg(msg)}`;
  if (/^今日已领取/.test(msg)) return `今日已领过：${msg}（剩余 ${r.remain}）`;
  // 注意「领取失败 http 400」里也可能带数字，故失败判据必须先于「含 +N」判据。
  if (CHECKIN_FAIL_RE.test(msg)) return `签到失败：${friendlyCheckinMsg(msg)}`;
  if (/^签到成功/.test(msg) || /\+-?\d+/.test(msg)) return `签到成功：${msg}（剩余 ${r.remain}）`;
  return `暂未领到：${msg}（剩余 ${r.remain}）`;
}
async function checkin(uid) {
  try {
    const r = await api("/api/account/checkin", { uid });
    // 无资格优先于「可重试」：该状态 2026-11-10 起是**终态**（retryable=false，
    // 当天不再自动重试），但对用户仍要明说「这不是工具故障、也不会自动补发」。
    if (CHECKIN_NO_ELIGIBILITY_RE.test(r.msg || "")) {
      toast(`无领取资格：${r.msg}`);
    } else if (!r.ok && r.retryable) {
      // 未达成且可重试（活动尚未创建/上游瞬时故障）：说明会在签到窗口内自动重试，
      // 避免用户看到「无可用签到活动」误以为失败。
      toast(`暂未领到：${r.msg}（10:00–12:00 窗口内会自动重试）`);
    } else {
      toast(checkinToast(r));
    }
    delete detailCache[uid]; // 签到改变余额，旧明细缓存会与新顶层打架
    loadState();
  } catch (e) { toast(e.message); }
}

async function refreshOne(uid) {
  try {
    const r = await api("/api/account/refresh", { uid });
    // 单位随账号切换：token 计量渠道（如 ZCode）不叫「积分」（否则上亿 token 被读成积分）。
    const acc = (state.accounts || []).find((x) => x.uid === uid);
    toast(acc && acc.credits_unit === "token"
      ? `刷新成功：剩余 ${fmtTokens(r.remain)} tokens`
      : `刷新成功：剩余积分 ${r.remain}`);
    delete detailCache[uid]; // 顶层已更新，旧明细缓存须作废，否则「新顶层+旧弹层」
    loadState();
  } catch (e) { toast(e.message); }
}

async function toggleDisable(uid, currentlyDisabled) {
  const action = currentlyDisabled ? "启用" : "停用";
  confirmDialog(`确定${action}该账号？${currentlyDisabled ? "" : "停用后路由不会分配给该账号。"}`, async () => {
    try {
      await api("/api/account/disable", { uid, disabled: !currentlyDisabled });
      toast(`账号已${action}`);
      loadState();
    } catch (e) { toast(e.message); }
  });
}

function removeAcct(uid) {
  confirmDialog("确定删除该账号？删除后需重新登录。", async () => {
    try {
      await api("/api/account/remove", { uid });
      toast("账号已删除");
      loadState();
    } catch (e) { toast(e.message); }
  });
}

// ---------- 批量操作 ----------
async function checkinAll() {
  $("btnCheckinAll").disabled = true;
  try {
    const r = await api("/api/account/checkin_all", {});
    detailCache = {}; // 批量签到改变余额，全部明细缓存作废
    const results = r.results || [];
    const ok = results.filter((x) => x.ok).length;
    // 「无资格」是终态（2026-11-10 起 ok=true / retryable=false）：既不是「签到成功」，
    // 也不会再自动重试 ⇒ 必须从「成功」里剔出来单独报，否则等于说它领到了。
    const noElig = results.filter((x) => CHECKIN_NO_ELIGIBILITY_RE.test(x.msg || ""));
    const okReal = ok - noElig.length;
    const pending = results.filter((x) => !x.ok && x.retryable).length;
    const skip = (state.accounts || []).filter((a) => noExplicitCheckin(a.group)).length;
    const total = results.length + skip;
    const parts = [skip ? `成功 ${okReal} / ${total}（${skip} 个无签到活动跳过）` : `成功 ${okReal} / 共 ${total}`];
    if (noElig.length) parts.push(`${noElig.length} 个无领取资格（该账号无此活动资格，不会自动重试）`);
    if (pending) parts.push(`${pending} 个未领到将在窗口内自动重试`);
    toast(`批量签到完成：${parts.map(friendlyCheckinMsg).join("；")}`);
    loadState();
  } catch (e) { toast(e.message); } finally {
    $("btnCheckinAll").disabled = false;
  }
}

async function refreshAll() {
  $("btnRefreshAll").disabled = true;
  try {
    const r = await api("/api/account/refresh_all", {});
    detailCache = {}; // 顶层已全部更新，全部明细缓存作废
    // 无积分概念的渠道（oczen/cline/自定义）后端不参与统计，无需前端再补偿数
    toast(r.busy ? "已有刷新任务进行中"
      : r.failed > 0 ? `积分刷新完成：成功 ${r.ok} / 失败 ${r.failed}`
      : `积分刷新完成：全部成功（${r.ok} 个）`);
    loadState();
  } catch (e) { toast(e.message); } finally {
    $("btnRefreshAll").disabled = false;
  }
}
