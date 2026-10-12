//go:build unix

package skills

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// killGroupOnCancel puts cmd in its own process group and makes cancelling
// its context kill that group, so the transport helpers git started die with
// it instead of lingering against a remote that never answers.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
