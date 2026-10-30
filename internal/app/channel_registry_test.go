package app

// channel_registry_test.go 把「新增渠道必须同步的清单」变成会红的断言（R99 的同款思路）。
//
// 背景（实测于 2026-10-29 的审计）：同一个「系统支持哪些渠道」概念被写了至少 8 遍，
// 而其中两张表**已经漂移**——channelOrder 只登记 15/23、feeChannelOrder 20/23。
// 漂移不报错、不崩溃，只表现为「面板顺序不对」或「某个渠道 tab 永远不出现」，
// 只能靠人猜。本文件把这 4 条不变量钉成测试：漏登记就变红，并直接点出缺哪个渠道。
//
// 注意：这些断言刻意**不要求**两张表与 BuiltinKinds 完全等集。
//   - channelOrder 允许多渠道"未登记"（落到末尾），但**已登记的键必须是真实渠道**
//     （防止改名后留下死键，那会让面板顺序静默错位）；
//   - feeChannelOrder 同理。两者都不接受「表里写了不存在的渠道」。
//
// 真正的等集要求（BuiltinKinds ⊇ 前端保留集）已由 scripts/ui-static-check.mjs 覆盖。

import (
	"sort"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// TestChannelOrderKeysAreRealKinds 两张顺序表里不得出现「不存在的渠道」。
//
// 现实危害：把 provider.Xxx 改名或删掉后，表里的键会变成零值 Kind("")——
// 编译期不报错（map 字面量用的是常量，删掉的常量会编译失败，但**改名**不会），
// 于是面板顺序表里混入一个空键，排序结果错位且无人发现。
// 本断言对 Key 逐个核对 provider.BuiltinKinds()，并按名字指出。
func TestChannelOrderKeysAreRealKinds(t *testing.T) {
	known := provider.BuiltinKinds()

	for k := range channelOrder {
		if !known[k.String()] {
			t.Errorf("channelOrder 登记了未知渠道 %q（改名/删除渠道后必须同步本表）", k.String())
		}
	}
	for _, k := range feeChannelOrder {
		if !known[k.String()] {
			t.Errorf("feeChannelOrder 登记了未知渠道 %q（改名/删除渠道后必须同步本表）", k.String())
		}
	}
}

// TestChannelOrderIsAFunction 同一渠道在 channelOrder 里只能有一个序号。
//
// map 字面量里重复键是编译错误，所以这条真正防的是**语义重复**：
// 两个不同 Kind 映射到同一序号会让排序退化成插入序（本仓已踩过 —— 旧表里
// Oczen=0 与 WorkBuddy=1 相邻写在一行，曾有人复制粘贴把两个都写成 0）。
func TestChannelOrderIsAFunction(t *testing.T) {
	seen := map[int]provider.Kind{}
	for k, r := range channelOrder {
		if prev, dup := seen[r]; dup {
			t.Errorf("channelOrder 序号 %d 被 %q 与 %q 重复占用（排序会退化成插入序）",
				r, prev.String(), k.String())
		}
		seen[r] = k
	}
}

// TestFeeChannelOrderHasNoDuplicates 费率表是切片，重复项会让同一渠道出现两个 tab。
func TestFeeChannelOrderHasNoDuplicates(t *testing.T) {
	seen := map[provider.Kind]bool{}
	for _, k := range feeChannelOrder {
		if seen[k] {
			t.Errorf("feeChannelOrder 重复登记 %q（面板会出现两个同名 tab）", k.String())
		}
		seen[k] = true
	}
}

// TestFeeChannelOrderCoversAllCheckinChannels 有签到的渠道必须有费率 tab。
//
// 这条来自 R107/issue 的真实形态：某渠道被加进 pool 与 scheduler，却漏进 feeChannelOrder，
// 面板上它的模型/费率/接入点永久不可见（buildFeesChannels 只遍历 order）。
// 判据不用「全部 23 个」——Cline/豆包等确有可能没有模型目录；只用「有签到 = 有模型面」
// 这个更弱的必要条件，避免把合法的空渠道误判成缺陷。
func TestFeeChannelOrderCoversAllCheckinChannels(t *testing.T) {
	inOrder := map[provider.Kind]bool{}
	for _, k := range feeChannelOrder {
		inOrder[k] = true
	}
	var missing []string
	for k := range channelOrder {
		if noExplicitCheckin(k) {
			continue
		}
		if !inOrder[k] {
			missing = append(missing, k.String())
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("以下渠道有签到活动但不在 feeChannelOrder（面板将永远不显示其模型 tab）：%s",
			strings.Join(missing, ", "))
	}
}

// TestChannelLessOrdersUnregisteredByASCII 未登记渠道必须排在已登记之后、且彼此按名升序。
//
// 这是对 channelOrder 注释「未出现在表中的渠道排最后（按字母序）」的可执行版本：
// 旧实现让未登记渠道的 rank 全部打平，sort.SliceStable 会保持插入顺序，
// 与注释声称的字母序不符。channelLess 修正后本断言锁住该行为。
func TestChannelLessOrdersUnregisteredByASCII(t *testing.T) {
	var registered, unregistered []provider.Kind
	for _, k := range []provider.Kind{
		provider.Oczen, provider.WorkBuddy, provider.CodeArts, // 已登记
		provider.Cline, provider.Raccoon, provider.MonkeyCode, // 未登记（实测不在表内）
	} {
		if _, ok := channelOrder[k]; ok {
			registered = append(registered, k)
		} else {
			unregistered = append(unregistered, k)
		}
	}
	if len(unregistered) < 2 {
		t.Skipf("channelOrder 已登记全部渠道，本断言失去意义（未登记=%d）", len(unregistered))
	}

	// 已登记 vs 未登记：已登记必须更小。
	for _, r := range registered {
		for _, u := range unregistered {
			if channelLess(u, r) {
				t.Errorf("未登记渠道 %q 排在已登记渠道 %q 之前（未登记必须垫底）", u.String(), r.String())
			}
		}
	}
	// 未登记之间必须按名字升序。
	for i := 0; i < len(unregistered); i++ {
		for j := i + 1; j < len(unregistered); j++ {
			a, b := unregistered[i], unregistered[j]
			if a == b {
				continue
			}
			want := a.String() < b.String()
			if got := channelLess(a, b); got != want {
				t.Errorf("未登记渠道 %q vs %q：channelLess=%v，按 ASCII 期望 %v（注释声称按字母序）",
					a.String(), b.String(), got, want)
			}
		}
	}
}

// TestAllKindsIsSingleSource BuiltinKinds 必须与 AllKinds 同集，且 AllKinds 无重复。
//
// 这是 B10 的收口断言：provider.AllKinds() 是「有哪些渠道」的唯一来源，
// BuiltinKinds() 由它派生（外加 ModelNamespaces 的上游命名空间）。
// 若有第三个地方又手抄一份渠道清单，应改为引用 AllKinds —— 本测试保证转换过程无损。
func TestAllKindsIsSingleSource(t *testing.T) {
	alls := provider.AllKinds()
	if len(alls) == 0 {
		t.Fatal("AllKinds() 为空")
	}
	seen := map[provider.Kind]bool{}
	for _, k := range alls {
		if seen[k] {
			t.Errorf("AllKinds() 重复登记 %q", k.String())
		}
		seen[k] = true
	}
	builtin := provider.BuiltinKinds()
	for _, k := range alls {
		if !builtin[k.String()] {
			t.Errorf("AllKinds 里的 %q 未出现在 BuiltinKinds（BuiltinKinds 必须由 AllKinds 派生）", k.String())
		}
	}
	// 反向：BuiltinKinds 里除 ModelNamespaces 之外不得有 AllKinds 之外的值。
	for name := range builtin {
		if _, isNS := provider.ModelNamespaces[name]; isNS {
			continue
		}
		if !seen[provider.Kind(name)] {
			t.Errorf("BuiltinKinds 里的 %q 不在 AllKinds（渠道清单出现了第二来源）", name)
		}
	}
}
