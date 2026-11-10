"use strict";
function renderAccounts() {
  const grid = $("acctList");
  const empty = $("acctEmpty");
  if (!state.accounts.length) {
    grid.innerHTML = "";
    empty.classList.remove("hidden");
    return;
  }
  empty.classList.add("hidden");

  grid.innerHTML = state.accounts.map((a) => {
    const noCheckin = noExplicitCheckin(a.group); // 无显式签到，签到按钮灰掉
    const noCheckinTitle = a.group === "workbuddyai"
      ? "无需手动签到：定时自动对话保活并领取日活奖励"
      : `${chLabel(a.group)} 不支持手动签到`;

    // 签到徽标三档（2026-11-10 起「无资格」独立成档）：
    //   - 无资格（该账号没有这个活动的资格，终态，当天不会再自动重试）⇒ warn 档，
    //     **不得显示成「签到成功」**——那会让用户以为领到了积分；
    //   - ok=true ⇒ 签到成功；ok=false ⇒ 签到失败。
    // 判据用 last_checkin_msg 的措辞（后端直说「无积分领取活动资格」），
    // 与 app-actions.js 的 CHECKIN_NO_ELIGIBILITY_RE 同一口径。
    const noElig = a.last_checkin_at && /无积分领取活动资格|无领取资格/.test(a.last_checkin_msg || "");
    const checkinTag = a.last_checkin_at
      ? (noElig
        ? `<span class="tag warn" title="${esc(a.last_checkin_msg || "该账号无积分领取活动资格")}">无资格</span>`
        : `<span class="tag ${a.last_checkin_ok ? "ok" : "bad"}">${a.last_checkin_ok ? "签到成功" : "签到失败"}</span>`)
      : (noCheckin ? `<span class="tag neutral" title="${esc(noCheckinTitle)}">${noCheckinText(a.group)}</span>` : '<span class="tag neutral">未签到</span>');

    const disabledClass = a.disabled ? " disabled" : "";
    const disableIcon = a.disabled ? "▶" : "⏸";
    const disableTitle = a.disabled ? "启用" : "停用";

    // 冷却标签与类别（TP-006：区分余额不足/频率超限/连续错误三档原因及操作建议）。
    let coolingKindLabel = "";
    let coolingKindAction = "";
    if (a.cooling) {
      switch (a.cool_kind) {
        case "hard_credit":
          coolingKindLabel = "余额不足";
          coolingKindAction = "建议充值或更换渠道，换模型无效";
          break;
        case "soft_rate":
          coolingKindLabel = "频率受限";
          coolingKindAction = "可稍后重试或切换其他模型";
          break;
        case "error_threshold":
          coolingKindLabel = "连续异常";
          coolingKindAction = "建议检查账号有效性或网络状况";
          break;
        case "low_balance":
          coolingKindLabel = "低额度防刷";
          coolingKindAction = "额度低于均值，进入保护冷却";
          break;
        default:
          coolingKindLabel = "冷却中";
          coolingKindAction = "原因未分类";
      }
    }
    const coolingTitle = a.cooling
      ? `${coolingKindLabel} · ${coolingKindAction}（${esc(a.reason || "")}）`
      : "";
    const coolingTag = a.cooling
      ? `<span class="tag warn cooling-kind-${a.cool_kind || "unknown"}" title="${esc(coolingTitle)}">${esc(coolingKindLabel)}至 ${esc(a.until || "-")}${a.until_ts ? `<span data-until-ts="${a.until_ts}">（${fmtLeft(a.until_ts)}）</span>` : ""}</span>`
      : "";
    const coolingClass = a.cooling ? " cooling" : "";

    // 模型级冷却标签（TP-006：上游 429 按模型限流，账号未整号冻结时展现）
    let modelCoolingTag = "";
    if (!a.cooling && a.model_cooling && Object.keys(a.model_cooling).length > 0) {
      const mcList = Object.entries(a.model_cooling);
      const mcTitle = mcList.map(([m, u]) => `${m} 限流至 ${u}`).join("&#10;");
      modelCoolingTag = `<span class="tag neutral model-cooling-tag" title="${esc(mcTitle)}">${mcList.length}模型限流中</span>`;
    }
    const checkinBtn = noCheckin
      ? `<span class="icon-op off" title="${esc(noCheckinTitle)}" onclick="return false">✓</span>`
      : `<span class="icon-op" title="签到" onclick="checkin('${a.uid}')">✓</span>`;

    // 无积分账号分两类，处理不同：
    // - oczen/cline（固定账号）：只保留「不可操作」的静态指示，
    //   不给刷新积分/停用/删除入口——后端也会硬拒，避免用户白点一次。
    // - 自定义渠道（R66）：无积分/无签到，但账号可在设置里停用/删除，
    //   故保留停用/删除/改名入口，只去掉签到/刷新积分两个无意义按钮。
    // 判定优先用后端下发的 credits_na（单一真相），前端名单只作旧载荷的兜底。
    const noCredits = isNoCreditAcct(a) || hasNoCredits(a.group);
    const fixedAcct = noCredits && !isCustomGroup(a.group);
    // 默认收起的「不适用」卡片：渲染时就带上 hidden（而不是等 applyAcctFilter），
    // 首帧不会先闪一排「不适用」再消失；applyAcctFilter 后续负责保持同步。
    const naHiddenClass = (!showNoCredit && isNoCreditAcct(a)) ? " hidden" : "";
    // 问号图标：hover 展示 oczen 通道说明（免费反代范围/私有 Key/代理三项）。
    // 放在锁头左侧；用独立的 help 样式（正常亮度 + help 光标），不可点击但 tooltip 可用。
    // title 内换行用 &#10;（HTML 属性实体），字面 \n 会被部分浏览器吞掉导致无 tooltip。
    const oczenHelp = `<span class="icon-op help" title="关于 OpenCodeZen 通道：&#10;1. 本工具仅反代其免费模型（绕过官方客户端限制）；付费账号可直接使用官方端点，无需经此通道&#10;2. 在 OpenCodeZen 获取的 API-Key 可在设置中配置，避免匿名账号共享限额超限&#10;3. 地域受限的免费模型（如 muse-spark 系）会经「备用上游通道」自动重发（设置里配置代理地址；未配置则原文透传 403）">?</span>`;
    // kilo 的说明与 oczen 不同：没有私有 Key 可配，也没有备用通道重放。
    const kiloHelp = `<span class="icon-op help" title="关于 Kilo 免费池：&#10;1. 上游是 api.kilo.ai 网关的免费模型池，**不需要任何凭据**（无账号、无 Key）&#10;2. 只反代免费池（模型清单里 isFree=true 的那些）；请求付费模型会返回 PAID_MODEL_AUTH_REQUIRED&#10;3. 无签到、无积分，故没有「刷新积分」入口">?</span>`;
    const channelHelp = a.group === "kilo" ? kiloHelp : oczenHelp;
    // 登录态失效（401/登录已失效）：徽标 + 一键重新登录（#134）。
    const sessionDeadTag = a.session_dead
      ? `<span class="tag bad" title="${esc(a.session_dead_reason || "登录态已失效，请重新登录")}">需重新登录</span>`
      : "";
    // 凭据缺 refresh_token（目前只有 OfficeAce，见后端 refreshTokenMissing）：
    // 性质与「登录态失效」不同 —— 不是被上游拒了，而是本地根本没这条凭据，
    // **不会自愈**，所以徽标要**持久**显示（原先只在导入那一刻弹一次提示，事后无从复查）。
    // 用 warn 色与 bad 区分：前者「还能对话，但积分/签到不可用」，后者「登录已失效」。
    const missingRT = Boolean(a.refresh_token_missing);
    const missingRTTag = missingRT
      ? `<span class="tag warn" title="${esc(a.refresh_token_missing_reason || "凭据缺 refresh_token：积分/签到不可用，需重新登录或重新导入")}">缺续期凭据</span>`
      : "";
    const needRelogin = Boolean(a.session_dead) || missingRT;
    const reloginTitle = missingRT
      ? "重新登录（本账号缺 refresh_token，积分/签到不可用；登录成功后自动补签）"
      : "重新登录（跳转登录页换新凭据，成功后自动补签）";
    // 一键重新登录：沿用既有内联 onclick 形态（本仓无 CSP 头，全文件已有 10 处同款写法）。
    // 渠道差异由 `startLogin` 内部**分派**：OfficeAce 是「华为云授权码 + 云端轮询捕获」的
    // 两步流程，必须走 startOfficeAceWebLogin（→ POST /api/login/officeace_auto），不能走
    // 一次性的 POST /api/login/start。2026-10-18 修正：先前 OfficeAce 的 ⟳ 直接打
    // /api/login/start，而后端 StartLoginFor 未登记 officeace ⇒ 点了只弹
    // `unknown login provider officeace`（死按钮，独立复核抓出）。
    const reloginBtn = needRelogin && RELOGIN_CHANNELS.has(a.group)
      ? `<span class="icon-op" title="${reloginTitle}" onclick="startLogin('${a.group}')">⟳</span>`
      : "";
    const ops = fixedAcct
      ? `${channelHelp}<span class="icon-op off" title="固定账号，不可停用/删除" onclick="return false">🔒</span>`
      : `${checkinBtn}
          ${reloginBtn}
          ${noCredits ? "" : `<span class="icon-op" title="刷新积分" onclick="refreshOne('${a.uid}')">↻</span>`}
          <span class="icon-op warn" title="${disableTitle}" onclick="toggleDisable('${a.uid}',${a.disabled})">${disableIcon}</span>
          <span class="icon-op danger" title="${isCustomGroup(a.group) ? esc("删除账号（自定义渠道将移除整个渠道配置）") : "删除账号"}" onclick="removeAcct('${a.uid}')">✕</span>`;
    // 显示名：点击直接弹出改名框（匿名渠道不可改，降级为普通文本）。
    // oczen 特例：渠道名已由 badge 承担，名字按凭证形态显示「匿名/私有Key」。
    const isOczen = a.group === "oczen";
    const oczenName = (state.oczen_api_key || "") ? "私有Key" : "匿名";
    const nameHtml = fixedAcct
      ? `<span class="acct-name">${esc(isOczen ? oczenName : (a.nickname || shortUid(a.uid)))}</span>`
      : `<span class="acct-name editable" title="点击修改显示名" onclick="openRename('${a.uid}','${esc(a.nickname || shortUid(a.uid)).replace(/'/g, "&#39;")}')">${esc(a.nickname || shortUid(a.uid))}</span>`;

    return `
    <div class="acct-card${disabledClass}${coolingClass}${naHiddenClass}" data-group="${esc(a.group || "workbuddy")}" data-na="${isNoCreditAcct(a) ? 1 : 0}">
      <div class="acct-top">
        <div class="acct-id-group">
          ${chBadge(a.group)}
          ${nameHtml}
          <span class="acct-uid">#${esc(shortUid(a.uid))}</span>
        </div>
        <div class="acct-ops">
          ${ops}
        </div>
      </div>
      <div class="acct-mid">
        <div class="acct-credits${noCredits ? "" : " clickable"}" data-uid="${a.uid}"${noCredits ? "" : ` onclick="showCreditDetail(event,'${a.uid}')" title="点击查看积分明细（含分页）" role="button" tabindex="0" onkeydown="if(event.key==='Enter'||event.key===' '){event.preventDefault();showCreditDetail(event,'${a.uid}')}"`}>${creditsText(a)}</div>
        <div class="acct-checkin">${coolingTag}${modelCoolingTag}${sessionDeadTag}${missingRTTag}${checkinTag}</div>
      </div>
    </div>`;
  }).join("");
  // R129：账号池变化会改变三场景的「可用账号数」，故一并刷新首页摘要（只读、无额外请求）。
  renderHomeScenes();
}

function renderTimes() {
  const box = $("timesBox");
  box.innerHTML = (state.checkin_times || []).map((t) =>
    `<span class="time-chip" title="点击删除" onclick="delTime('${t}')">${t} ✕</span>`).join("");
  $("nextCheckin").textContent = state.next_checkin || "-";
}

// channelCredits 汇总某渠道全部账号的可用额度（与积分汇总条同源 state.accounts）。
//
// 返回 { bal, unit }：unit 为 ""（积分）或 "token"（Token 计数，如 ZCode）。
// ⚠️ 调用方**必须**按 unit 选单位与格式化（token 用 fmtTokens、积分用 fmtNum）——
// 否则会把上亿 token 显示成「N 分」（2026-10-17 用户截图报告：费率页 ZCode 角标
// 显示「余额 8,000,000 分」，正是漏了这一步）。
// 无该渠道账号时返回 null（不渲染角标，避免显示误导性的 0）。
function channelCredits(kind) {
  if (!state || !state.accounts) return null;
  let sum = 0, has = false, unit = "";
  for (const a of state.accounts) {
    if ((a.group || "workbuddy") !== kind) continue;
    if (a.credits_na) return null; // 匿名渠道无额度概念
    has = true;
    if (a.credits_unit === "token") unit = "token";
    sum += a.credits || 0;
  }
  return has ? { bal: sum, unit } : null;
}

// tabBalText 渠道 tab 余额角标文案（按计量单位切换：token 与积分分开显示）。
function tabBalText(v) {
  if (!v) return "";
  return v.unit === "token"
    ? `余额 ${fmtTokens(v.bal)} tokens`
    : `余额 ${fmtNum(v.bal)} 分`;
}

// tabBalTitle 角标 tooltip：明示单位与统计口径（token 不计入积分合计）。
function tabBalTitle(v) {
  return v && v.unit === "token"
    ? "该渠道全部账号当前可用 Token 数（与积分分开统计，不计入积分合计）"
    : "该渠道全部账号当前可用积分合计";
}

// fmtNum 千分位整数（与汇总条的 fmt 同口径，独立命名避免作用域冲突）。
function fmtNum(n) {
  return n.toLocaleString("zh-CN");
}

// refreshTabBalances 就地更新费率渠道 tab 的积分余额角标。
// 账号余额刷新远频于费率表（state 每次 loadState 都更新，fees 只在加载/手动刷新时变），
// 若只在 renderFees 里写余额，账号刷新后角标会停在旧值。这里在每次 render() 时
// 只改角标文本（不重建 DOM），既保持最新又不打断用户浏览/滚动。
function refreshTabBalances() {
  const box = $("feesBox");
  if (!box) return;
  box.querySelectorAll(".fees-tab[data-feech]").forEach((tab) => {
    const bal = channelCredits(tab.dataset.feech);
    const el = tab.querySelector(".fees-tab-bal");
    if (bal === null) {
      if (el) el.remove();
      return;
    }
    const txt = tabBalText(bal);
    const tip = tabBalTitle(bal);
    if (el) {
      if (el.textContent !== txt) el.textContent = txt;
      if (el.title !== tip) el.title = tip;
    }
    else { const s = document.createElement("span"); s.className = "fees-tab-bal"; s.textContent = txt; s.title = tip; tab.appendChild(s); }
  });
}