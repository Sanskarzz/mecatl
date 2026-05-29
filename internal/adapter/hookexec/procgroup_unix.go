//go:build unix

package hookexec

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the hook in its own process group and, on context
// cancellation, kills the entire group rather than just the leader. A shell
// hook can spawn children (e.g. `sh -c "sleep 5"`); killing only the shell can
// leave a child holding the stdout/stderr pipes, which makes (*exec.Cmd).Wait
// block until that child exits — defeating the per-run timeout. Signalling the
// negative PID delivers SIGKILL to every process in the group, so the pipes
// close promptly and Run honours its deadline.
func configureProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		// Negative PID → the whole process group created via Setpgid. ESRCH
		// (group already gone) is benign; exec only consults this on the
		// cancellation path.
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}
