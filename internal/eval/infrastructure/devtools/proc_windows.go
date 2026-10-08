//go:build windows

package devtools

import (
	"os/exec"
	"strconv"
	"syscall"
)

// isolate starts the server in a process group of its own, so that the
// Ctrl-C/Ctrl-Break which interrupts the evaluator does not also reach the
// browser in the middle of a call. The evaluator closes it in an orderly way
// instead.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killTree ends the server and everything it started, such as Chrome. Windows
// has no call that ends a process and its descendants the way a Unix process
// group does, so this asks taskkill to walk the process tree the OS already
// tracks by parent pid and end every one of them.
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	if err := exec.Command("taskkill", "/T", "/F", "/PID", pid).Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
