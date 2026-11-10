package app

import "testing"

// TestIsNoEligibilityMsg 无资格文案判定（R164）：
//
// 终态「无资格」在结构化路径里 OK=true（结构化路径 OK = !Retryable），
// 所以后端**只能靠文案**把它从「签到成功」里摘出来。判据必须与前端
// CHECKIN_NO_ELIGIBILITY_RE 同口径，否则前后端会给出互相矛盾的计数。
func TestIsNoEligibilityMsg(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"该账号无积分领取活动资格：上游本次没有下发任何领积分活动（…）⇒ 不是本工具的问题，重试无意义", true},
		{"无领取资格", true},
		{"签到成功 +100", false},
		{"今日已领取 +100", false},
		{"签到失败", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isNoEligibilityMsg(c.msg); got != c.want {
			t.Errorf("isNoEligibilityMsg(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}
