//go:build !unix

package hookexec

import "os/exec"

// configureProcessGroup is a no-op on platforms without POSIX process groups.
// There the timeout is enforced by exec's default process kill plus the
// Cmd.WaitDelay backstop set in Run.
func configureProcessGroup(_ *exec.Cmd) {}
