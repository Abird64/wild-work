package backuplink

import "testing"

// TestRawReportsEffectiveAddress 断言 Raw() 回的是「当前实际生效」的地址原文。
//
// 现场依据（2026-10-30）：用户按提示配了代理，但填的端口没在监听（1080），
// 机器上真正可用的是 7890；旧文案只报「网络不可达」，用户对着「● 已生效」
// 的绿点无从判断自己填错了什么。Raw() 就是为了让错误文案能点名出口。
func TestRawReportsEffectiveAddress(t *testing.T) {
	t.Cleanup(func() { _ = Set("") })

	_ = Set("")
	if got := Raw(); got != "" {
		t.Fatalf("未配置时应为空串，得 %q", got)
	}

	if err := Set("socks5://127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	if got := Raw(); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("应回生效地址原文，得 %q", got)
	}

	// 换一个地址必须立刻反映（快照语义，热更新无缓存）
	if err := Set("http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	if got := Raw(); got != "http://127.0.0.1:7890" {
		t.Fatalf("改地址后应立刻反映，得 %q", got)
	}
}

// TestRawStripsUserinfo 带凭据的代理地址不得把密码写进用户可见文案。
func TestRawStripsUserinfo(t *testing.T) {
	t.Cleanup(func() { _ = Set("") })
	if err := Set("socks5://user:secret@127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	got := Raw()
	if got == "" {
		t.Fatal("应回非空")
	}
	if containsUserinfo(got) {
		t.Fatalf("不得回显凭据：%q", got)
	}
	if got != "socks5://127.0.0.1:1080" {
		t.Fatalf("应只剔除 userinfo，得 %q", got)
	}
}

func containsUserinfo(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '@' {
			return true
		}
	}
	return false
}
