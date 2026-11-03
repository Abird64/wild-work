#!/usr/bin/env node
// check-percent-quota.mjs 百分比配额渠道的源码级护栏（2026-10-30 用户实测驱动）。
//
// 为什么用源码断言而不是单测：豆包工作的 UserResourceDetail 全部逻辑都在
// CDP 页面上下文里跑（`conn.Evaluate`），没有可注入的纯函数，单测拿不到它。
// 而这里的缺陷恰恰是**一行累加**（`remain += left`）——源码里看得见、运行时才现形，
// 故按项目既有做法（ui-static-check 同款）在提交前静态检查。
import { readFileSync } from "node:fs";

const DOUBAO = "internal/doubaowork/client.go";
const src = readFileSync(new URL("../" + DOUBAO, import.meta.url), "utf8");

const checks = [];
const check = (name, pass, extra = "") => checks.push([name, pass, extra]);

// ① 三个窗口的剩余百分比**不得**相加成 remain。
// 现场：曾返回 265（100+100+65），面板当积分显示，凭空多出一个「265 积分」。
check(
  "豆包工作不得把窗口百分比累加进 remain",
  !/remain\s*\+=\s*left/.test(src),
  "出现 remain += left：三个窗口口径不同、不可相加"
);

// ② ControllerResponse 通道：窗口必须是 Total=100 形态（percent 提取依赖它）。
check(
  "窗口条目必须是 Total=100 / Remain=剩余百分比",
  /Total:\s*100,\s*Used:\s*int64\(used\),\s*Remain:\s*left/.test(src),
  "条目形态变了，quotaWindowsFrom 的 Total==100 判据会失效"
);

// ③ 两个豆包渠道都必须标 percent（否则面板又会显示「0 可用积分」）。
const OPS = "internal/app/accounts_ops.go";
const ops = readFileSync(new URL("../" + OPS, import.meta.url), "utf8");
check(
  "豆包两条渠道都标 percent 口径",
  /provider\.Doubao\.String\(\)\s*==\s*group\s*==\s*provider\.Doubaowork\.String\(\)/.test(ops.replace(/\s+/g, " "))
  || /group == provider\.Doubao\.String\(\) \|\| group == provider\.Doubaowork\.String\(\)/.test(ops),
  "accounts_ops.go 的 creditsUnit 少了豆包分支"
);

let failed = 0;
for (const [name, pass, extra] of checks) {
  console.log(`  ${pass ? "✅" : "❌"} ${name}${!pass && extra ? `  ← ${extra}` : ""}`);
  if (!pass) failed++;
}
console.log(failed === 0 ? "\n百分比配额口径检查通过" : `\n${failed} 项失败`);
process.exit(failed === 0 ? 0 : 1);
