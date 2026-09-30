//go:build !windows

package ai

import (
	"os/exec"
	"syscall"
)

// prepare makes cancelling a CLI stop the whole process group, not just the
// first process (npm-installed CLIs are often wrapper scripts).
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
