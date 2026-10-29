package stats

import (
	"fmt"
	"testing"
	"time"
)

// 回归组：R130「最近在用」列表（modelLastUsed）。
//
// 这一组守四条不变量，每一条都对应一个会静默出错的实现：
//  1. **跨天保留** —— s.models 跨天清零，若 lastUsed 跟着清，首页每天早上变空；
//  2. **跨重启保留** —— 只存内存不落盘，重启后列表消失（用户以为数据丢了）；
//  3. **失败不推进** —— 时间戳必须与 reqs/tokens 同口径（只在记账成功时更新），
//     否则一个实际没跑成的模型会被顶到「最近在用」最前面；
//  4. **顺序稳定** —— 同一时刻两个模型不能每次刷新互换（map 遍历随机）。

func TestRecentModelsSurviveDayRoll(t *testing.T) {
	s := newTestStats(t)
	day1 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	s.AddUsage("workbuddy/m1", 10, 5, 0, 100, 1, 0, false, day1)

	if got := len(s.Snapshot(day1).RecentModels); got != 1 {
		t.Fatalf("当日应有 1 条最近在用, got %d", got)
	}
	// 跨天：今日模型清零，但「最近在用」必须还在（这是本条决议的核心）。
	day2 := day1.Add(24 * time.Hour)
	s.ApplyPoll([]Account{{UID: "a", Group: "workbuddy", Credits: 100}}, day2)
	snap := s.Snapshot(day2)
	if n := len(snap.Models); n != 0 {
		t.Fatalf("跨天后今日模型应清零, got %d", n)
	}
	if n := len(snap.RecentModels); n != 1 {
		t.Fatalf("跨天后最近在用被清空了（正是本字段要避免的）, got %d", n)
	}
	// 今日没跑 ⇒ reqs/tokens 为 0（不能把昨天的量算进今天）
	r := snap.RecentModels[0]
	if r.Model != "workbuddy/m1" || r.Reqs != 0 || r.Tokens != 0 {
		t.Fatalf("跨天后今日量应为 0 而不是沿用昨天: %+v", r)
	}
	if r.LastTS != day1.Unix() {
		t.Fatalf("LastTS 应保留原时刻 %d, got %d", day1.Unix(), r.LastTS)
	}
}

func TestRecentModelsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now()
	s.AddUsage("traework/m2", 10, 5, 0, 100, 1, 0, false, now)
	s.FlushNow()

	s2, err := New(dir, nil)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	snap := s2.Snapshot(now)
	if len(snap.RecentModels) != 1 {
		t.Fatalf("重启后最近在用丢了（未落盘）, got %d", len(snap.RecentModels))
	}
	if snap.RecentModels[0].Model != "traework/m2" || snap.RecentModels[0].LastTS != now.Unix() {
		t.Fatalf("重启后记录不一致: %+v", snap.RecentModels[0])
	}
}

// 失败请求不经过 AddUsage（记账成功才调），所以这里守的是「AddUsage 之外没有任何
// 地方写 modelLastUsed」——用一次未记账的 ApplyPoll + Snapshot 验证列表不变。
func TestRecentModelsNotTouchedByPoll(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.AddUsage("workbuddy/m1", 10, 5, 0, 100, 1, 0, false, now)
	before := s.Snapshot(now).RecentModels
	s.ApplyPoll([]Account{{UID: "a", Group: "workbuddy", Credits: 500}}, now.Add(time.Minute))
	after := s.Snapshot(now.Add(time.Minute)).RecentModels
	if len(before) != 1 || len(after) != 1 || after[0].LastTS != before[0].LastTS {
		t.Fatalf("积分轮询不应改动最近使用时刻: before=%+v after=%+v", before, after)
	}
}

// 同上表时刻的排序必须稳定（否则列表每次刷新抖动）。
func TestRecentModelsOrderStableOnTie(t *testing.T) {
	s := newTestStats(t)
	ts := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	for _, m := range []string{"z/m", "a/m", "m/m"} {
		s.AddUsage(m, 1, 1, 0, 10, 1, 0, false, ts)
	}
	first := s.Snapshot(ts).RecentModels
	if len(first) != 3 {
		t.Fatalf("应有 3 条, got %d", len(first))
	}
	// 同刻按模型名升序，且必须可重复
	want := []string{"a/m", "m/m", "z/m"}
	for i, w := range want {
		if first[i].Model != w {
			t.Fatalf("同刻排序应为模型名升序: got %v, want %v", modelsOf(first), want)
		}
	}
	for i := 0; i < 20; i++ {
		got := s.Snapshot(ts).RecentModels
		for j := range want {
			if got[j].Model != want[j] {
				t.Fatalf("第 %d 次快照顺序抖动: got %v", i, modelsOf(got))
			}
		}
	}
}

func TestRecentModelsLimitAndOrder(t *testing.T) {
	s := newTestStats(t)
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	for i := 0; i < recentModelsLimit+3; i++ {
		s.AddUsage(fmt.Sprintf("ch/m%d", i), 1, 1, 0, 10, 1, 0, false, base.Add(time.Duration(i)*time.Minute))
	}
	got := s.Snapshot(base.Add(time.Hour)).RecentModels
	if len(got) != recentModelsLimit {
		t.Fatalf("应截断到 %d 条, got %d", recentModelsLimit, len(got))
	}
	// 最近的在前：最后写入的 m7 应排第一
	if want := fmt.Sprintf("ch/m%d", recentModelsLimit+2); got[0].Model != want {
		t.Fatalf("首条应为最近使用的 %s, got %s", want, got[0].Model)
	}
}

// modelLastUsed 不设上限就是只增不减的内存+落盘泄漏。
func TestModelLastUsedCapped(t *testing.T) {
	s := newTestStats(t)
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	for i := 0; i < modelLastUsedCap+50; i++ {
		s.AddUsage(fmt.Sprintf("ch/m%d", i), 1, 1, 0, 10, 1, 0, false, base.Add(time.Duration(i)*time.Second))
	}
	if n := len(s.modelLastUsed); n > modelLastUsedCap {
		t.Fatalf("modelLastUsed 未封顶: %d > %d", n, modelLastUsedCap)
	}
	// 淘汰的是最旧的：最新那条必须还在
	if _, ok := s.modelLastUsed[fmt.Sprintf("ch/m%d", modelLastUsedCap+49)]; !ok {
		t.Fatal("淘汰策略删掉了最新记录（应删最旧）")
	}
}

func modelsOf(rs []ModelRecent) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Model
	}
	return out
}
