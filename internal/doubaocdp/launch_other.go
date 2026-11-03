//go:build !windows

package doubaocdp

import "os/exec"

// detach 非 Windows 平台不做特殊处理（本渠道本身依赖 Windows 客户端）。
func detach(cmd *exec.Cmd) {}
