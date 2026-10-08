//go:build unix

package devtools

import (
	"os/exec"
	"syscall"
)

// isolate puts the server in a process group of its own, so that the Ctrl-C
// which interrupts the evaluator does not also kill the browser in the middle
// of a call. The evaluator closes it in an orderly way instead.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree ends the server and everything it started, such as Chrome.
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
