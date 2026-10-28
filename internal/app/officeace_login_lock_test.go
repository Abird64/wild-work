package app

import (
	"testing"
	"time"

	"wild-work/internal/config"
	"wild-work/internal/officeace"
)

// TestStartOfficeAceLoginReleasesLock 守住「发起成功后 muOALogin 必须已释放」。
//
// 历史缺陷（2026-10-27 实测）：StartOfficeAceLogin 成功路径漏了 Unlock，于是**发起过一次
// 登录之后** muOALogin 被永久占用 —— 面板前端每 2s 调一次 officeace_auto_status 全都阻塞在
// Lock 上（curl 直接超时），用户侧看到的就是「点了没反应 / 永远转圈」。错误路径反而是对的
// （它 Unlock 了），所以这个洞只有在**成功发起**后才暴露，走查代码很容易漏。
//
// 用零值 *officeace.LoginSession 当替身：它满足调用所需的最小表面（Status/URL 均可用），
// 又不需要真打华为云。断言用超时而不是顺序读取 —— 死锁时顺序读取会让测试**挂住**而不是
// 失败，那样的测试在 CI 里表现为整包超时，定位成本极高。
func TestStartOfficeAceLoginReleasesLock(t *testing.T) {
	prev := officeAceStartLogin
	t.Cleanup(func() { officeAceStartLogin = prev })
	officeAceStartLogin = func(string, func(officeace.LoginResult)) (*officeace.LoginSession, error) {
		return &officeace.LoginSession{}, nil
	}

	a := &App{cfg: &config.Config{AuthDir: t.TempDir()}}
	if _, err := a.StartOfficeAceLogin(); err != nil {
		t.Fatalf("发起登录失败：%v", err)
	}

	// 关键断言：发起成功后立刻查状态，必须在 2s 内返回（死锁则超时）
	done := make(chan struct{})
	go func() {
		_ = a.GetOfficeAceLoginStatus()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GetOfficeAceLoginStatus 超时：StartOfficeAceLogin 成功后未释放 muOALogin")
	}
}

// TestStartOfficeAceLoginSerializesConcurrentCalls 并发发起只允许一条流程真正开始。
//
// 原实现靠「持锁跑完网络」顺带挡住并发双击（锁内会看到 oaLogin 已是 pending）。把网络
// 移出锁后，必须有一个显式占位（oaStarting）来保留这个保护，否则双击会起两条 OAuth 流程、
// 后一条覆盖前一条的会话，用户在浏览器里完成的是被丢弃的那条。
func TestStartOfficeAceLoginSerializesConcurrentCalls(t *testing.T) {
	prev := officeAceStartLogin
	t.Cleanup(func() { officeAceStartLogin = prev })

	started := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	officeAceStartLogin = func(string, func(officeace.LoginResult)) (*officeace.LoginSession, error) {
		calls++
		close(started)
		<-release // 把第一个请求卡在「发起中」，让第二个请求撞进来
		return &officeace.LoginSession{}, nil
	}

	a := &App{cfg: &config.Config{AuthDir: t.TempDir()}}
	go func() { _, _ = a.StartOfficeAceLogin() }()
	<-started

	if _, err := a.StartOfficeAceLogin(); err == nil {
		t.Fatal("并发发起应被拒绝（否则会起两条 OAuth 流程互相覆盖）")
	}
	close(release)
	time.Sleep(100 * time.Millisecond)
	if calls != 1 {
		t.Fatalf("真实发起次数 = %d，want 1", calls)
	}
}
