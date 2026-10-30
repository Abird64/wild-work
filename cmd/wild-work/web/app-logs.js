"use strict";
// ---------- 降级记录（原模型无可用账号切备胎的事件流；无事件整块隐藏） ----------
// fmtFbDur 秒数 → 紧凑文案（"42 秒" / "3 分 12 秒" / "1 时 02 分"）
function fmtFbDur(sec) {
  sec = Math.max(0, Math.floor(sec));
  if (sec < 60) return sec + " 秒";
  const m = Math.floor(sec / 60), s = sec % 60;
  if (m < 60) return m + " 分 " + s + " 秒";
  return Math.floor(m / 60) + " 时 " + String(m % 60).padStart(2, "0") + " 分";
}

function renderFallbacks(evs) {
  const block = $("stFbBlock");
  block.hidden = false; // 常显：让用户随时知道有这个区块；无事件显示占位行
  const $t = $("stFbs");
  if (!evs.length) {
    $t.innerHTML = `<tr><td class="st-empty">暂无降级记录——原模型无可用账号切备胎时这里会出现事件（降级/恢复时刻与持续时长）</td></tr>`;
    return;
  }
  let h = `<tr><th>#</th><th>降级时间</th><th>原模型</th><th></th><th>备胎</th><th>恢复</th><th>持续</th></tr>`;
  evs.forEach((e, i) => {
    h += `<tr class="${e.active ? "fb-active" : ""}">` +
      `<td>${i + 1}</td>` +
      `<td class="st-mono">${esc(e.at)}</td>` +
      `<td class="st-mono" title="${esc(e.from)}">${esc(e.from)}</td>` +
      `<td class="fb-arrow">→</td>` +
      `<td class="st-mono" title="${esc(e.to)}">${esc(e.to)}</td>` +
      (e.active
        ? `<td><span class="fb-live">● 降级中</span></td><td><span class="fb-live" data-fb-epoch="${e.at_unix}"></span></td>`
        : `<td class="st-mono">${esc(e.recovered || "—")}</td><td>${fmtFbDur(e.dur_sec)}</td>`) +
      `</tr>`;
  });
  $("stFbs").innerHTML = h;
}

// 进行中事件的「持续」每秒重算（只改文本不重渲染；元素不存在时空转，开销可忽略）
setInterval(() => {
  document.querySelectorAll("[data-fb-epoch]").forEach((el) => {
    el.textContent = "已 " + fmtFbDur((Date.now() - Number(el.dataset.fbEpoch) * 1000) / 1000);
  });
}, 1000);

// 冷却卡片的「剩 X 时 X 分」每 30 秒重算：卡片只在 loadState 事件时重渲染，
// 不定时刷新会让倒计时停在渲染时刻的值，静止几小时后反而误导。
setInterval(() => {
  document.querySelectorAll("[data-until-ts]").forEach((el) => {
    el.textContent = `（${fmtLeft(Number(el.dataset.untilTs))}）`;
  });
}, 30000);

// 异常账号（三级：disabled 已停用 / cool 整号冷却 / err 错误累计；无异常整块隐藏）
const ABNORMAL_META = {
  disabled: { cls: "st-ab-disabled", tag: "已停用", tip: "账号被上游停用（如登录态失效），需人工处理" },
  cool: { cls: "st-ab-cool", tag: "整号冷却", tip: "账号整体进入冷却（限流/余额不足/账号故障），解冻前不可用" },
  err: { cls: "st-ab-err", tag: "错误累计", tip: "连续错误累计中，达阈值将触发冷却" },
};

function renderAbnormal(rows) {
  if (!rows.length) { $("stAbnormal").innerHTML = ""; $("stAbnormal").className = "st-abnormal"; return; }
  const counts = { disabled: 0, cool: 0, err: 0 };
  rows.forEach((r) => { if (counts[r.level] != null) counts[r.level]++; });
  // 摘要徽章只显示存在的级别（顺序固定：停用→整号冷却→错误累计）
  let sumHtml = "";
  ["disabled", "cool", "err"].forEach((lv) => {
    if (counts[lv] > 0) {
      sumHtml += `<span class="st-ab-sum ${ABNORMAL_META[lv].cls}" title="${esc(ABNORMAL_META[lv].tip)}">` +
        `${ABNORMAL_META[lv].tag} ${counts[lv]}</span>`;
    }
  });
  let html = `<div class="st-block-title">异常账号<span class="st-hint">限流 / 冻结 / 亚健康 · 共 ${rows.length} 个</span>` +
    `<span class="st-ab-sums">${sumHtml}</span></div>` +
    `<div class="st-scroll st-ab-scroll">` +
    `<div class="st-ab-row st-ab-head">` +
    `<span class="st-ab-idx">#</span>` +
    `<span class="st-ab-tag-slot">状态</span>` +
    `<span class="st-ab-plat-slot">平台</span>` +
    `<span class="st-ab-name">账号</span>` +
    `<span class="st-ab-reason">原因</span>` +
    `<span class="st-ab-until">解冻</span>` +
    `<span class="st-ab-credit">余额</span>` +
    `</div>`;
  rows.forEach((r, i) => {
    const meta = ABNORMAL_META[r.level] || ABNORMAL_META.err;
    let reason = r.reason || meta.tag;
    if (r.level === "err" && r.err_count) reason = `连续错误 ${fmtCredits(r.err_count)} 次`;
    html += `<div class="st-ab-row ${meta.cls}">` +
      `<span class="st-ab-idx">${i + 1}</span>` +
      `<span class="st-ab-tag-slot"><span class="st-ab-tag" title="${esc(meta.tip)}">${meta.tag}</span></span>` +
      `<span class="st-ab-plat-slot">${chBadge(r.group)}</span>` +
      `<span class="st-ab-name" title="${esc(r.uid)}">${esc(r.nickname || r.uid)}</span>` +
      `<span class="st-ab-reason" title="${esc(reason)}">${esc(reason)}</span>` +
      `<span class="st-ab-until">${r.until ? esc(r.until) : "—"}</span>` +
      `<span class="st-ab-credit">${r.credits > 0 ? fmtCredits(r.credits) + "分" : "—"}</span>` +
      `</div>`;
  });
  html += `</div>`;
  $("stAbnormal").innerHTML = html;
  $("stAbnormal").className = "st-abnormal st-block";
}

// 运行日志（app.log 尾部）：级别判定顺序有讲究——
// ① code=9074/ok=false 先标红（「签到完成」话术里装的业务失败，先判会被误放行为 info）；
// ② 已签到 是良性重复签到，不标红；③ 汇总成功行（含 failed=N）仍按成功放行。
function appLogLevel(text) {
  if (/code=9074|ok=false/.test(text)) return "err";
  if (/完成|成功|ok=true|已签到/.test(text)) return "info";
  if (/PANIC|FATAL|失败|错误|无效|Error|failed/.test(text)) return "err";
  if (/warn|WARN|警告/.test(text)) return "warn";
  return "info";
}

let applogFirst = true;
function renderAppLog(lines) {
  // 用户上翻阅读时不打扰；贴底（或首帧）才自动吸底
  const box = $("stApplog");
  const pinned = applogFirst || box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  applogFirst = false;
  let html = "";
  const start = Math.max(0, lines.length - APPLOG_MAX_LINES);
  for (let i = start; i < lines.length; i++) {
    const line = lines[i];
    if (!line) continue;
    // Go log 前缀 "2026/09/22 15:04:05.123456 " → 展示 MM-DD HH:MM:SS
    const m = line.match(/^(\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?)\s(.*)$/);
    const cls = `st-logline st-log-${appLogLevel(line)}`;
    if (m) {
      html += `<div class="${cls}"><span class="st-log-time">${esc(m[1].slice(5, 19))}</span>${esc(m[2])}</div>`;
    } else {
      html += `<div class="${cls}">${esc(line)}</div>`;
    }
  }
  box.innerHTML = html || `<div class="st-empty">暂无日志</div>`;
  if (pinned) box.scrollTop = box.scrollHeight;
}

// 里程碑 toast：浏览器通知优先，降级面板 toast（复用上游 #toast）；localStorage 按 id 去重
function statsToastSeen() {
  try { return JSON.parse(localStorage.getItem(TOAST_SEEN_KEY) || "[]"); } catch (e) { return []; }
}
function statsToastMark(id) {
  const ids = statsToastSeen();
  ids.push(id);
  try { localStorage.setItem(TOAST_SEEN_KEY, JSON.stringify(ids.slice(-200))); } catch (e) { /* 忽略 */ }
}
function handleToasts(list) {
  let seen = {};
  try { statsToastSeen().forEach((x) => { seen[x] = 1; }); } catch (e) { seen = {}; }
  for (const t of list) {
    if (seen[t.id]) continue;
    showStatsNotification(t.title, t.body);
    statsToastMark(t.id);
  }
}
function showStatsNotification(title, body) {
  try {
    if ("Notification" in window) {
      if (Notification.permission === "granted") {
        new Notification(title, { body });
        return;
      }
      if (Notification.permission === "default") Notification.requestPermission();
    }
  } catch (e) { /* 降级面板提示 */ }
  toast(`${title}：${body}`);
}
