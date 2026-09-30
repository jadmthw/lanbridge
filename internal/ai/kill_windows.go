//go:build windows

package ai

import (
	"os/exec"
	"strconv"
)

// prepare makes cancelling a CLI stop the whole process tree. On Windows,
// codex/grok are .cmd launchers that start the real program as a child,
// and killing only the launcher leaves the child running.
func prepare(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
