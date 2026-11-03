//go:build windows

package doubaocdp

import (
	"os/exec"
	"syscall"
)

// detach 让子进程脱离父进程的作业/控制台，使 daemon 退出时不会连带杀掉豆包。
//
// Windows 上用 CREATE_NEW_PROCESS_GROUP + DETACHED_PROCESS：前者让它不接收父进程的
// Ctrl+C，后者让它不共享控制台（无控制台的 daemon 里启动 GUI 程序时需要）。
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008, // DETACHED_PROCESS
		HideWindow:    true,
	}
}
