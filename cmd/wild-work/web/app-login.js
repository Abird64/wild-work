"use strict";
// ---------- 登录 ----------
let pendingChannel = null;
// 待执行动作："login" = 打开浏览器登录，"import" = 从本机客户端导入。
// 小浣熊两种都支持，由弹窗里的两个按钮分别设定。
let pendingAction = "login";
// 无手动签到渠道的登录提示差异文案（国际版会自动领日活奖励）。
const NO_CHECKIN_LOGIN_HINT = {
  workbuddyai: "（无需手动签到，定时自动对话保活并领取日活奖励）",
  qwenwork: "（每日积分服务端 00:00 自动发放；若浏览器已登录千问办公则全自动完成，否则需扫码一次）",
  monkeycode: "（凭据来自本机已登录的 MonkeyCode 客户端；上游无续期接口，客户端重新登录后需再次导入）",
  raccoon: "（支持「网页版登录」与「从客户端导入」两种方式；access_token 约 1 小时，本工具会自动续期，上游刷新时会轮换 refresh_token）",
  loomy: "（凭据来自本机已登录的 Loomy 客户端；上游无续期接口，约 14 天后需重新登录并再次导入）",
  glm: "（登录后请按下方指引复制 refresh_token 粘贴回来）",
  cline: "（无需登录：在 app.cline.bot 注册账号，Account / API Keys → Create API Key 后粘贴即可；Key 不过期也不轮换）",
  officeace: "（凭据来自本机已登录的 OfficeAce 客户端；每日签到的临时凭据约 2 小时有效，本工具会自动续期）",
  minimax: "（设备码授权：点「浏览器登录」后在新页面确认授权，本工具会自动轮询直到完成；可重复添加多个账号）",
  lobsterai: "（本机回调授权：点「浏览器登录」后在有道页面完成登录，浏览器会跳回本机完成授权；支持多个账号）",
  zcode: "（设备授权：点「浏览器登录」后在智谱页面完成授权，本工具会轮询直到完成；⚠️ 本渠道的积分领取依赖网页验证码，本工具不自动领取）",
  gemini: "（本机回调授权：点「浏览器登录」后完成 Google 账号授权，浏览器会跳回本机；支持多个账号。⚠️ 额度以「配额窗口剩余百分比」呈现，不是积分数）",
  codearts: "（PKCE 授权：点「浏览器登录」后在华为云授权页完成授权，浏览器会跳回本机；支持多个账号。⚠️ 免费额度按 UTC+8 自然日重置）",
};
function promptLogin(channel) {
  pendingChannel = channel;
  const name = chLabel(channel);
  pendingAction = isImportLocal(channel) ? "import" : "login";
  // 「账号标识」输入行：只对**上游不提供稳定身份字段**的渠道显示（当前是 CodeArts）。
  // 为什么要把区分账号的责任交给用户：上游不回传任何身份信息，账号 ID 只能落到每次新签发的
  // 临时凭据上 ⇒ 既无法区分多个账号、重复登录又会被写成新条目（见 docs/渠道接入备忘.md）。
  const labelRow = $("lcLabelRow");
  if (labelRow) {
    const need = channel === "codearts";
    labelRow.classList.toggle("hidden", !need);
    if (need) {
      const inp = $("lcAccountLabel");
      if (inp) inp.value = "";
    }
  }
  // 次按钮默认隐藏，只在「两条路都通」的渠道（小浣熊）里显示。
  const altBtn = $("btnLoginAlt");
  altBtn.classList.add("hidden");
  altBtn.onclick = null;

  // OfficeAce（华为云果办）：主按钮走自助 OAuth（打开华为云授权页），
  // 次按钮回退到「从本机 OfficeAce 客户端导入」。
  if (channel === "officeace") {
    pendingAction = "login";
    $("lcTitle").textContent = "添加 OfficeAce 账号";
    $("lcMsg").innerHTML = `点击「浏览器登录」将打开<b>华为云授权页</b>，用你的华为账号登录后，`
      + `本工具会自动捕获凭据并添加账号（无需安装 OfficeAce 桌面端）。`
      + `也可以点「从客户端导入」直接读取本机已登录的 OfficeAce 桌面端凭据。`
      + `<div class="glm-warn" style="margin-top:10px">`
      + `<div class="glm-warn-title">提示</div>`
      + `对话走不过期的网关凭据；每日签到的临时凭据约 2 小时有效，本工具会自动续期。`
      + `若同一账号的 refresh_token 被本工具与桌面端共用可能互踢，需在本工具单独登录一次。`
      + `</div>`;
    $("btnLoginConfirm").textContent = "浏览器登录";
    altBtn.textContent = "从客户端导入";
    altBtn.classList.remove("hidden");
    altBtn.onclick = () => { pendingAction = "import"; confirmLogin(); };
    $("loginConfirmOverlay").classList.remove("hidden");
    return;
  }

  // 网页版登录渠道：主按钮走「拉起浏览器 + 自动捕获凭据」，次按钮回退到本机客户端导入。
  if (hasWebLogin(channel)) {
    pendingAction = "login";
    $("lcTitle").textContent = "添加 " + name + " 账号";
    if (channel === "loomy") {
      // Loomy 的机制与小浣熊不同：不拉浏览器、不捕获凭据，而是由本工具起一个本机登录引导页，
      // 用户在页面里填手机号 + 短信验证码（见 internal/login_loomy）。
      // 另外它**没有 refresh 端点**，所以不存在「互踢 / 自动续期」那一段说明。
      $("lcMsg").innerHTML = `点击「登录${name}」会打开一个<b>本机登录页</b>（地址在你的电脑上，不经过本工具的服务器），`
        + `请在其中输入手机号、接收并填写短信验证码，登录完成后本工具会自动保存账号。`
        + `<div class="glm-warn" style="margin-top:10px">`
        + `<div class="glm-warn-title">⚠️ 关于登录有效期</div>`
        + `Loomy 的登录态<b>约 14 天</b>到期，且上游<b>没有刷新接口</b> —— `
        + `到期后需要重新点「登录${name}」再登一次，或改用「从客户端导入」重新读取已登录客户端的凭据。`
        + `</div>`
        + `也可以改用「从客户端导入」：直接读取本机已登录客户端的凭据。`;
      $("btnLoginConfirm").textContent = "登录" + name;
      altBtn.textContent = "从客户端导入";
      altBtn.classList.remove("hidden");
      altBtn.onclick = () => { pendingAction = "import"; confirmLogin(); };
      $("loginConfirmOverlay").classList.remove("hidden");
      return;
    }
    $("lcMsg").innerHTML = `点击「登录${name}」会打开一个<b>独立的浏览器窗口</b>（不影响你日常浏览器的登录态），`
      + `请在其中正常登录（扫码 / 手机号 / 微信均可），登录完成后本工具会自动捕获凭据并保存账号。`
      + `<div class="glm-warn" style="margin-top:10px">`
      + `<div class="glm-warn-title">⚠️ 关于登录态互踢</div>`
      + `本工具会用捕获到的凭据<b>自动续期</b>（上游刷新时会轮换 refresh_token）。`
      + `若你把这个账号的凭据<b>同时</b>用在本工具和浏览器 / 官方客户端上，两边会争抢同一个 refresh_token，`
      + `可能出现<b>其中一侧被挤下线</b>的情况（上游可能对同一账号限制单会话）。`
      + `想两边同时用，请在本工具里点「登录${name}」<b>单独登录一次</b>（本流程新建独立会话），`
      + `而不是从浏览器复制凭据粘进来。`
      + `</div>`
      + `也可以改用「从客户端导入」：直接读取本机已登录客户端的凭据。`;
    $("btnLoginConfirm").textContent = "登录" + name;
    altBtn.textContent = "从客户端导入";
    altBtn.classList.remove("hidden");
    altBtn.onclick = () => { pendingAction = "import"; confirmLogin(); };
    $("loginConfirmOverlay").classList.remove("hidden");
    return;
  }

  // 粘贴凭据型渠道（Cline）：弹专用输入框，而不是「打开浏览器登录」。
  // Cline 的凭据是官方 API Key，创建动作发生在 app.cline.bot 网页里，本工具无法代劳，
  // 所以这里没有轮询/回调，只有一次「验证 + 落盘」（后端只调 /api-keys，零 token 消耗）。
  if (isPasteKey(channel)) {
    $("clineKeyInput").value = "";
    $("clineErr").textContent = "";
    $("clineWarn").classList.add("hidden");
    $("clineOverlay").classList.remove("hidden");
    setTimeout(() => $("clineKeyInput").focus(), 30);
    return;
  }
  // 纯导入型渠道：文案与动作都不同（读本机客户端凭据，而不是打开浏览器登录）。
  if (isImportLocal(channel)) {
    $("lcTitle").textContent = "导入 " + name + " 账号";
    $("lcMsg").textContent = `将读取本机已登录的${name}客户端凭据并保存到 wild-work（不会修改客户端本身）。`
      + `若提示未找到凭据，请先打开并登录${name}客户端后重试。${NO_CHECKIN_LOGIN_HINT[channel] || ""}`;
    $("btnLoginConfirm").textContent = "导入";
    $("loginConfirmOverlay").classList.remove("hidden");
    return;
  }
  // CodeArts：授权页必须避开日常浏览器里的华为云登录态（否则永远被授权成已登录的那个账号），
  // 后端会用**无痕窗口**打开；同时需要用户填「账号标识」来区分多个华为账号。
  if (channel === "codearts") {
    $("lcTitle").textContent = "添加 CodeArts 账号";
    $("lcMsg").innerHTML = `点击「登录 CodeArts」会打开一个<b>无痕浏览器窗口</b> —— `
      + `无痕会话不带日常浏览器的华为云登录态（否则你只会被授权成<b>已登录的那个</b>账号）。`
      + `请在其中登录<b>你想要添加的那个</b>华为账号。`
      + `<div class="glm-warn" style="margin-top:10px">`
      + `<div class="glm-warn-title">为什么要填「账号标识」</div>`
      + `华为云不回传账号身份信息，本工具无法自动分辨「同一账号重新登录」与「另一个账号」。`
      + `所以：<b>留空</b> = 覆盖已有的 CodeArts 账号；<b>填写</b>（例如"工作号"）= 作为独立账号并存。`
      + `以后重登同一个账号时填<b>同一个标识</b>，就会覆盖它而不是再新增一个。`
      + `</div>`;
    $("btnLoginConfirm").textContent = "登录 CodeArts";
    $("loginConfirmOverlay").classList.remove("hidden");
    return;
  }
  $("lcTitle").textContent = "添加 " + name + " 账号";
  // 此处只会收到内建渠道名（自定义渠道走设置弹层，不经 promptLogin），
  // 故直接查内建名单，不走动态判定的 noExplicitCheckin()。
  $("lcMsg").textContent = NO_EXPLICIT_CHECKIN.has(channel)
    ? `点击「登录${name}」将打开浏览器窗口，请按照指示正常登录${name}账号，登录成功后关闭浏览器窗口即可。${NO_CHECKIN_LOGIN_HINT[channel] || ""}`
    : `点击「登录${name}」将打开浏览器窗口，请按照指示正常登录${name}账号，登录成功后关闭浏览器窗口即可。`;
  $("btnLoginConfirm").textContent = "登录" + name;
  $("loginConfirmOverlay").classList.remove("hidden");
}
function confirmLogin() {
  $("loginConfirmOverlay").classList.add("hidden");
  if (!pendingChannel) return;
  // 按 pendingAction 分发：小浣熊两种动作都支持，不能只看渠道是否属于导入型。
  if (pendingAction === "import") importLocal(pendingChannel);
  else if (pendingChannel === "officeace") startOfficeAceWebLogin();
  else startLogin(pendingChannel, loginLabelValue());
}

// loginLabelValue 读取登录弹窗里的「账号标识」：只对需要它的渠道（CodeArts）有意义，
// 其余渠道那一行是隐藏的，恒返回空串。
function loginLabelValue() {
  const row = $("lcLabelRow");
  if (!row || row.classList.contains("hidden")) return "";
  const inp = $("lcAccountLabel");
  return inp ? inp.value.trim() : "";
}

// importLocal 从本机已登录的官方客户端导入凭据（MonkeyCode / 小浣熊 / Loomy）。
// note 只在有降级时非空（如某个凭据文件没读到）—— 必须显示出来，
// 否则用户只看到"已导入成功"，等积分不显示时才发现。
async function importLocal(channel) {
  try {
    const r = await api("/api/account/import_local", { channel });
    const head = `已导入 ${chLabel(channel)} 账号 ${r.uid || ""}`;
    if (r.note) toast(`${head}。${r.note}`, 8000);
    else toast(head);
    await loadState();
  } catch (e) {
    toast(e.message);
  }
}

async function startLogin(channel, accountLabel) {
  // 渠道分派：OfficeAce 是**两步**流程（华为云授权码 + 云端轮询捕获），专属入口是
  // startOfficeAceWebLogin（→ POST /api/login/officeace_auto）。它与下面
  // `POST /api/login/start` 的形态不同——后者一次调用就返回 auth_url，且后端
  // StartLoginFor 不会为 officeace 拉起轮询。这里收口成一个入口，卡片上的 ⟳
  // 与「添加账号」都走同一条路，避免再出现「前端能点、后端不认」的死按钮。
  // （2026-10-18：卡片 ⟳ 曾直接 startLogin('officeace') 而只弹 unknown login provider。）
  if (channel === "officeace") return startOfficeAceWebLogin();
  try {
    const r = await api("/api/login/start", { channel, account_label: accountLabel || "" });
    const url = r.auth_url;
    if (!url) { toast("无法获取登录链接"); return; }
    $("loginTitle").textContent = `添加 ${chLabel(channel)} 账号`;
    // 三种情形（顺序即优先级）：
    //  ① 后端已用**无痕窗口**拉起（CodeArts）—— 绝不能 window.open：那会在日常浏览器里再开一个
    //     带登录态的标签页，用户又会被授权成老账号，无痕那一步就白做了；
    //  ② 网页版登录渠道（glm / 小浣熊）—— 后端用独立 profile + CDP 拉起，同理不能 open
    //     （那个标签页没有调试端口，捕获不到）；
    //  ③ 其余渠道 —— 由前端 window.open 打开（它们不需要隔离登录态）。
    if (r.backend_opened) {
      $("loginMsg").textContent = `已为你打开一个**无痕窗口**，请在其中完成 ${chLabel(channel)} 登录；`
        + `登录完成后无需任何操作，本工具会自动添加账号。`;
    } else if (channel === "loomy") {
      // Loomy 与 ② 相反：后端**不**拉浏览器，返回的是本机登录引导页地址，
      // 必须由前端 window.open 打开（见 internal/login_loomy）。它也不「捕获凭据」——
      // 用户在引导页里填手机号 + 短信验证码，后端据此换 session。
      $("loginMsg").textContent = `已为你打开 Loomy 登录页，请在其中输入手机号并填写短信验证码；`
        + `登录完成后本工具会自动添加账号。登录态约 14 天后到期，届时需重新登录。`;
      window.open(url, "_blank", "noopener,noreferrer");
    } else if (hasWebLogin(channel)) {
      $("loginMsg").textContent = `已为你打开一个独立的浏览器窗口，请在其中完成${chLabel(channel)}登录；`
        + `登录完成后无需任何操作，本工具会自动捕获凭据并添加账号。`;
    } else {
      $("loginMsg").textContent = "请在浏览器新窗口中完成登录…";
      window.open(url, "_blank", "noopener,noreferrer");
    }
    $("loginOverlay").classList.remove("hidden");
    $("btnCopyUrl").dataset.url = url;
    // 2026-10-18（H9）：后端告知这次登录是否需要轮询。
    // 改造前这里**无条件** startLoginPoll()，而 GLM 属于「只给链接、不跑流程」的渠道
    // （后端不置 loginBusy、不起 pollLogin）⇒ 轮询看到 login_busy=false 便提示
    // 「登录完成，正在同步账号…」，实际什么都没发生 —— 假成功。
    // 现在只有真发起登录流程的渠道才轮询；只给链接的渠道引导用户改用粘贴入口。
    if (r.needs_poll === false) {
      $("loginMsg").textContent = "已为你打开登录页。请按页面提示登录后，"
        + "从浏览器 Cookie 中取出 refresh_token 粘贴到下方，再点「提交」。";
      window.open(url, "_blank", "noopener,noreferrer");
      return;
    }
    // 手动回调兜底（LobsterAI / CodeArts / TraeWork，R96③/R97/#98）。
    // LobsterAI 的上游 portal **只认** http://127.0.0.1:<port>/auth/callback
    // （hostname 严格等于 127.0.0.1，否则它丢弃该地址、改发 lobsterai:// deep-link，
    // 见 docs/决议项.md R97）；TraeWork 的回调写死 127.0.0.1:<随机端口> 且监听只有
    // 5 分钟寿命（issue #98）。所以浏览器与网关不同机（或监听超时）时，浏览器会停在
    // 一条打不开的 127.0.0.1 页面上 —— 那条 URL 里就有凭据：把它粘回来即可
    // （后端解析出 code/token 自行换凭据）。
    // 想全自动：把该端口固定（config.login.lobsterai_callback_port）并在浏览器所在机器上
    // 建一条 `ssh -L 127.0.0.1:<port>:127.0.0.1:<port> <网关>` 转发，之后无需粘贴。
    if (r.manual_callback) {
      $("lcCallbackHint").textContent =
        "登录完成后若浏览器停在一条打不开的 127.0.0.1 地址上（上游只允许回调到 127.0.0.1，"
        + "从别的机器打开面板时必然如此）：把那条完整 URL（含 ? 之后的全部参数）复制到下面提交，网关会立刻换取凭据。"
        + "浏览器与网关同一台机器时无需理会此项；若已在浏览器所在机器建好端口转发，也会自动完成。";
      $("lcCallbackRow").classList.remove("hidden");
    } else {
      $("lcCallbackRow").classList.add("hidden");
      $("lcCallbackUrl").value = "";
    }
    startLoginPoll();
  } catch (e) {
    toast(e.message);
  }
}

async function cancelLogin() {
  try {
    // 仅在 OfficeAce 流程进行中才调它的取消端点（否则对其它渠道是无谓的 400 噪音）。
    if (oaPoll) { stopOfficeAcePoll(); cancelOfficeAceLogin(); }
    await api("/api/login/cancel", {});
    stopLoginPoll();
    $("loginOverlay").classList.add("hidden");
    toast("登录已取消");
  } catch (e) { toast(e.message); }
}

// ---- OfficeAce 自助登录（华为云 OAuth 授权码 + PKCE）----
let oaPoll = null;
async function startOfficeAceWebLogin() {
  try {
    const r = await api("/api/login/officeace_auto", {});
    const url = r.auth_url;
    if (!url) { toast("无法获取登录链接"); return; }
    $("loginTitle").textContent = "添加 OfficeAce 账号";
    $("loginMsg").textContent = "已在本设备新标签页打开华为云授权页，请在其中登录并授权；"
      + "完成后本工具会自动捕获凭据（捕获走云端轮询，与在哪台设备打开无关）。"
      + "若浏览器拦截了弹窗未打开，可点「复制链接」手动打开。";
    $("loginOverlay").classList.remove("hidden");
    $("btnCopyUrl").dataset.url = url;
    if (r.warning) toast(r.warning, 6000);
    // 在用户当前设备打开授权页（与其它渠道一致）。
    // 注意：带 noopener 时 window.open 的返回值**恒为 null**（MDN windowFeatures），
    // 不能拿它判断是否被弹窗拦截（否则每次登录都误报「被拦截」）。故：先开、后断链——
    // 不带 feature 打开才能拿到窗口句柄，随后手动置 opener=null 兼顾安全。
    const w = window.open(url, "_blank");
    if (w) {
      try { w.opener = null; } catch (e) { /* 跨域时可能被拒，忽略 */ }
    } else {
      toast("浏览器拦截了弹窗，请点「复制链接」手动打开", 6000);
    }
    startOfficeAcePoll();
  } catch (e) {
    toast(e.message);
  }
}

function startOfficeAcePoll() {
  stopOfficeAcePoll();
  oaPoll = setInterval(async () => {
    try {
      const st = await api("/api/login/officeace_auto_status");
      if (st.status === "done") {
        stopOfficeAcePoll();
        $("loginOverlay").classList.add("hidden");
        toast(`OfficeAce 账号已添加${st.nickname ? "：" + st.nickname : ""}`);
        await loadState();
        refreshFees();
      } else if (st.status === "failed" || st.status === "expired") {
        stopOfficeAcePoll();
        toast(st.error || "OfficeAce 登录失败", 8000);
      } else if (st.status === "canceled" || st.status === "idle") {
        stopOfficeAcePoll();
      }
    } catch (e) { /* 忽略瞬时错误 */ }
  }, 2000);
}

function stopOfficeAcePoll() {
  if (oaPoll) { clearInterval(oaPoll); oaPoll = null; }
}

function cancelOfficeAceLogin() {
  stopOfficeAcePoll();
  api("/api/login/officeace_auto_cancel", {}).catch(() => {});
}

function copyUrl() {
  const url = $("btnCopyUrl").dataset.url;
  if (!url) { toast("暂无链接"); return; }
  // 必须走 copyText：局域网 http:// 是非安全上下文，navigator.clipboard 被浏览器
  // 直接禁用，需降级 execCommand（与顶栏 API-Key 复制同一 helper）。
  copyText(url, "链接");
}

// 手动提交授权回调地址（局域网/远程兜底，只对 manual_callback=true 的渠道显示，
// 当前是 CodeArts、LobsterAI 与 TraeWork）。
// 语义：只把 code 交给网关换凭据，用户不需要交出任何 token。
async function submitCallback() {
  const url = ($("lcCallbackUrl").value || "").trim();
  if (!url) { toast("请先粘贴浏览器地址栏里的完整 URL"); return; }
  try {
    await api("/api/login/callback", { url });
    $("lcCallbackUrl").value = "";
    $("lcCallbackHint").textContent = "已提交，正在换取凭据…";
  } catch (e) {
    // 保留弹层与已填内容：端口粘错、缺 code 这类错误下登录流程往往仍在进行，
    // 用户需要就地改正后重试（关掉弹层等于把输入丢了）。
    // 真正的终态失败（授权被拒/超时）由 2s 状态轮询统一收尾，避免重复提示。
    $("lcCallbackHint").textContent = "提交失败：" + e.message;
  }
}

// 登录轮询
let loginPoll = null;
function startLoginPoll() {
  stopLoginPoll();
  loginPoll = setInterval(async () => {
    try {
      const st = await api("/api/state");
      if (!st.login_busy) {
        stopLoginPoll();
        $("loginOverlay").classList.add("hidden");
        // 后端在 login_error 里回传终态失败（超时/回调不可用/兑换失败）；
        // 拿不到就是真的成功了。
        if (st.login_error) {
          toast(st.login_error, 8000);
          return;
        }
        toast("登录完成，正在同步账号…");
        await loadState();
        refreshFees();
      }
    } catch (e) { /* 忽略 */ }
  }, 3000);
}
function stopLoginPoll() {
  if (loginPoll) { clearInterval(loginPoll); loginPoll = null; }
}

// ---------- 智谱清言登录（自动捕获 Cookie，手工粘贴为兜底） ----------
// 自动路径：wild-work 拉起独立 profile 的浏览器 → 用户正常登录 → CDP 捕获 Cookie。
// 独立 profile 天然隔离，多账号逐个添加互不干扰（无需手动开无痕）。
let glmPoll = null;

function startGLMLogin() {
  $("glmErr").textContent = "";
  $("glmStatus").textContent = "正在启动浏览器…";
  $("glmManual").classList.add("hidden");
  $("glmAuto").classList.remove("hidden");
  $("glmOverlay").classList.remove("hidden");
  api("/api/login/glm_auto", {})
    .then(() => {
      $("glmStatus").textContent = "浏览器已打开，请在其中登录智谱清言…";
      startGLMPoll();
    })
    .catch((e) => {
      $("glmStatus").textContent = "";
      $("glmErr").textContent = (e.message || "启动失败") + "（可展开下方手工方式）";
      $("glmManual").classList.remove("hidden");
    });
}

function startGLMPoll() {
  stopGLMPoll();
  glmPoll = setInterval(async () => {
    try {
      const st = await api("/api/login/glm_auto_status");
      if (st.status === "success") {
        stopGLMPoll();
        $("glmStatus").textContent = `✅ 已添加账号：${st.nickname || st.uid}`;
        toast("智谱清言账号已添加");
        await loadState();
        refreshFees();
        setTimeout(() => $("glmOverlay").classList.add("hidden"), 1200);
      } else if (st.status === "failed") {
        stopGLMPoll();
        $("glmStatus").textContent = "";
        $("glmErr").textContent = (st.error || "登录失败") + "（可展开下方手工方式）";
        $("glmManual").classList.remove("hidden");
      } else if (st.status === "cancelled" || st.status === "idle") {
        stopGLMPoll();
      }
    } catch (e) { /* 忽略瞬时错误 */ }
  }, 2000);
}

function stopGLMPoll() {
  if (glmPoll) { clearInterval(glmPoll); glmPoll = null; }
}

function cancelGLMLogin() {
  stopGLMPoll();
  api("/api/login/glm_auto_cancel", {}).catch(() => {});
  $("glmOverlay").classList.add("hidden");
}

// submitClineKey 提交用户粘贴的 Cline 官方 API Key。
// 后端只做「GET /api/v1/api-keys 验证 + 落盘」，零 token 消耗；
// 返回的 warning 非空表示「这把 Key 与已有账号同属一个 Cline 账号」——
// 官方配额按账号计（…UsageCostUSDPerUser），同账号多 Key 不会扩额度，必须显式告知。
async function submitClineKey() {
  const key = ($("clineKeyInput").value || "").trim();
  if (!key) { $("clineErr").textContent = "请粘贴 API Key（sk_ 开头）"; return; }
  const btn = $("btnClineSubmit");
  btn.disabled = true;
  $("clineErr").textContent = "验证中…";
  try {
    const r = await api("/api/login/cline_key", { api_key: key });
    $("clineOverlay").classList.add("hidden");
    $("clineKeyInput").value = "";
    if (r && r.warning) toast(r.warning);
    toast("Cline 账号已添加");
    await loadState();
    refreshFees();
  } catch (e) {
    $("clineErr").textContent = e.message || "验证失败";
  } finally {
    btn.disabled = false;
  }
}

// 手工兜底：粘贴 refresh_token
async function submitGLMToken() {
  const token = ($("glmTokenInput").value || "").trim();
  if (!token) { $("glmErr").textContent = "请粘贴 refresh_token"; return; }
  const btn = $("btnGLMSubmit");
  btn.disabled = true;
  $("glmErr").textContent = "验证中…";
  try {
    await api("/api/login/glm_token", { refresh_token: token });
    stopGLMPoll();
    $("glmOverlay").classList.add("hidden");
    toast("智谱清言账号已添加");
    await loadState();
    refreshFees();
  } catch (e) {
    $("glmErr").textContent = e.message || "验证失败";
  } finally {
    btn.disabled = false;
  }
}

// ---------- 签到时间（设置弹层内编辑；单次变更立即保存） ----------
function delTime(t) {
  const times = (state.checkin_times || []).filter((x) => x !== t);
  saveTimes(times);
}

function addTime() {
  // 点「＋」不再**直接**塞入当前时刻（旧行为：想加 21:30 只能先加「现在」再删，
  // 用户 2026-10-21 报「签到时间添加没法手动选」）。改为就地展开一个原生
  // <input type="time">，选好再点「确定」；取消 / Esc / 点别处即收起，且不发请求。
  const box = $("timeEditor");
  if (!box.hidden) { closeTimeEditor(); return; }
  const now = new Date();
  $("timePicker").value = `${String(now.getHours()).padStart(2, "0")}:${String(now.getMinutes()).padStart(2, "0")}`;
  box.hidden = false;
  $("timePicker").focus();
}

function closeTimeEditor() {
  $("timeEditor").hidden = true;
}

function commitTime() {
  const v = ($("timePicker").value || "").trim();
  if (!v) { toast("请先选择时间"); return; }
  const times = (state.checkin_times || []).slice();
  if (times.includes(v)) { toast(`${v} 已在列表中`); closeTimeEditor(); return; }
  times.push(v);
  closeTimeEditor();
  saveTimes(times.sort());
}

async function saveTimes(times) {
  try {
    await api("/api/config/checkin_times", { times });
    toast("签到时间已更新");
    loadState();
  } catch (e) { toast(e.message); }
}

// ---------- 开机自启（设置弹层内，切换立即保存） ----------
// ---------- 请求体摘要（诊断开关，设置弹层内，切换立即保存） ----------
async function toggleBodyLog() {
  try {
    await api("/api/config/body_log", { on: $("chkBodyLog").checked });
    toast("设置已保存");
  } catch (e) { toast(e.message); loadState(); }
}

async function toggleAutostart() {
  try {
    await api("/api/config/autostart", { on: $("chkAutostart").checked });
    toast("设置已保存");
  } catch (e) { toast(e.message); loadState(); }
}
