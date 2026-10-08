//go:build !unix && !windows

package devtools

import "os/exec"

// isolate and killTree have no real implementation on a platform that is
// neither Unix nor Windows (such as js/wasm or plan9): the evaluator is not
// built for those, so this is a harmless fallback, not a supported target.
func isolate(*exec.Cmd) {}

func killTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
