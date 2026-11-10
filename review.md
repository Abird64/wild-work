# review.md — wild-work 审查判据

> 给**独立审核 Agent**（未参与本轮实现）。只回答「这个仓里什么算真问题、要什么证据」。
> 本仓 `AGENTS.md` 已有 R1–R167 决议 + §2 关键不变量 25 条，**本文件不复述它**，只做审核索引：它告诉你"不能改坏什么"，这里告诉你"审的时候盯哪几类"。
> 跨仓通用纪律见 vault `01-Projects/AI任务系统/治理/模板/review模板.md`。

## 一、本仓形态（命中通用模板哪几行）

**HTTP 服务 + 兼容层网关 + 桌面托盘 + 内嵌 Web 面板**，Go 单二进制，Windows 为主平台。所以取证有四路，**任一路都不能冒充另一路**：`go test` 全绿 ≠ 面板没歪 ≠ exe 是新的 ≠ 上游真的收了我们的请求。

## 二、失败长什么样（本仓历史高频根因，按决议号定点读）

| 类别 | 具体形态 | 锚点 |
|---|---|---|
| **上游错误未类型化** | ≥400 用裸 `fmt.Errorf` 返回 ⇒ 自愈链 `errors.As` 恒 false ⇒ 401 永不刷新、积分恒 0、明细恒空。**新增任何上游调用必须回 `&provider.Error{Kind: Classify(...)}`** | §2-19、R79 |
| **「表里少一行」的接线缺陷** | 渠道自己的单测全绿，因为被测的是**没被调用**的那个函数；缺陷在装配层（`loginFlows[...]` 漏 `clientFactory` ⇒ 代理白配）。**审核必须看 app 层接线断言有没有，不看渠道包** | R133 |
| **惩罚/冷却误用** | 对 `SingleAccount` 或池内单号渠道施加冷却、`errCount`、`Disable` ⇒ 整条渠道下线；429 也不冷却（无号可轮换，透传让客户端按 `Retry-After` 退避）。`ErrHardCredit`(402) 是**模型级**冷却，账号不动。**严禁**把 401/403 归 `ErrSessionDead` | §2-22、R16/R60/R113 |
| **「未上报」被显示成 0** | 本仓最高频的一类真缺陷：缓存命中率、配额百分比、到期时间、积分余额——上游不提供字段时必须显示「—/待刷新/隐藏整列」，**显示 0 就是编造数据**，且会污染路由分母 | §2-24、R134/R156/R157、§2-18 |
| **凭据与脱敏** | 日志/面板/消息框零 token；自定义渠道 api-key 明文只活在**那一次交互**（点复制进剪贴板、点眼睛进输入框，关窗即清），**绝不随 `/api/state`、`/api/fees` 30s 轮询批量下发**；脱敏格式改时必须 Go 与 JS 两侧同步（`isMaskedKey` 认三种形态），前端不得二次脱敏；渠道改名必须显式 `renames:[{new,old}]`，**历史流水不改写**（洗名＝伪造账本） | §2-2、R119/R121/R122 |
| **流式与管道** | devdump 落盘必须 tee，`io.ReadAll(resp.Body)` 会让 SSE 正文整段突然出现；`gateway.call()` 走 `io.Pipe`，调用方读完必须 `res.Close()`；`pipeRW.Flush()` 空操作是刻意的，勿改 | §2-13/14、§5 |
| **图片两维** | token 维：工具结果里的图片**绝不得** `json.Marshal` 成 base64 文本（1.6MB 截图 ≈146 万 token vs ~1K）；字节维：`imageslim` 只动 `data:` URL，`maxInnerBody` 必须是**函数**（常量会把上限快照死） | §2-25、R113/R114 |
| **兼容层语义** | `PrepareBody` 四改写勿动；`/v1/chat/completions` 与 `/v1/models` 由内层直服，gateway 只接管三接口；429 分类必须**先于** hardMarkers | §2-1/12/15 |
| **假绿陷阱（审自己工具时最易中招）** | ① 文件名含 `_windows` 被 Go 当 **GOOS 约束**，测试被静默排除、`no tests to run` 却**退出码 0**——判据是 `go list -f '{{.IgnoredGoFiles}}'`，不是看 PASS；② 变异必须确认**真的植入了**（替换文本写错 → 变异没生效 ≠ 有保护）；③ `ui-static-check` 里 `check()` 只 push 不打印 → 报"全部通过"是假绿，断言本身也要被断言 | R130/R132/R156 |
| **产物与状态文件** | 改代码后必须重建 `dist/wild-work.exe`（CI 不产它，不重建＝跑的是旧二进制）；版本核对用字节计数而非 `strings`；`data/state-*.json` 只增不减字段；`config.example.json` 与 `config.Default()` 同步；`docs/` 白名单制——新文档必须同时 `.gitignore` 反选 + §6 表格加行，否则死链 | §2-0/5/7、§4、§6.1 |
| **时区** | 到期/重置一律按 **UTC+8 墙钟**解析（`softRateResetLoc`），用 `time.Local` 会在非 UTC+8 机器上算错一天 | §2-18 |

## 三、验证命令（数字一律当场实测，本文件故意不写基线）

> 写死基线数字必然陈旧——`obsidian-personal-workbench/AGENTS.md` 刚刚因此被抓（写 17/84，实测 13/102）。

```bash
go build ./... && go vet ./... && go test ./...          # 全绿才算改动成立
go list -f '{{.IgnoredGoFiles}}' ./... | grep -v '^$'     # 有没有测试被 GOOS 约束静默吃掉
node scripts/ui-static-check.mjs                          # 面板静态断言（含场景 4.x / 9 等）
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-H windowsgui" -o dist/wild-work.exe ./cmd/wild-work
python -c "b=open('dist/wild-work.exe','rb').read();print('new:',b.count(b'<新版本>'),'old:',b.count(b'<旧版本>'))"  # 期望 new≥1 且 old==0
```

**改动的证据包**：`go test` 输出 + **变异验证记录（几处植入、几处变红）** + 面板改动的宽度×页码验证矩阵与截图 + exe 重建与版本字节核对；涉及真实上游行为的，附 `devdump` 离线取证或单变量对照（**能离线取证就不要打上游**）。

## 四、不报（噪音）

`docs/渠道接入备忘.md` 里各渠道协议细节的对错（属调研层，不在本包范围）· 注释措辞与命名 · `dist/`、`build/`、`temp/` 产物 · 未入库文档（`ref/`、`HANDOFF.md`、逆向类本地文档）——**别把未入库文档当缺失来报** · 已知限制清单里已登记的项（R131 网络出口、R158 哨兵未获真签名、`/v1/chat/completions` 不瘦身等）。

## 五、待仓库负责人校正

1. 20 条渠道里，哪些属于「改坏了会真金白银亏额度」的高风险区（签到 / 积分路由 / 记账分母），值得审核时提高档？
2. 面板 UI 的验证矩阵档位（现沿用 6 档宽度 × 页数）是否还是你要的口径，哪些页可以省？
3. `devdump` 快照含敏感头——审核时能否读它的落盘文件，还是只看汇总行？
