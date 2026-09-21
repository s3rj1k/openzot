package tools

import (
	"os/exec"
	"syscall"
)

// setProcessGroup runs the command in its own process group and makes cancellation kill the whole group. A shell
// command starts children, and killing only the shell leaves `go test ./...` binaries or anything backgrounded running,
// so addressing the group is what makes "stop this command" mean everything it started.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}

		// a negative pid addresses the group rather than the single process
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
