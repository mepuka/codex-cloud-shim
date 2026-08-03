//go:build unix

package cloud

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the codex subprocess in its own process group and makes
// context cancellation kill the whole group, so children codex spawns (node
// shims, git) cannot outlive it holding our pipes. SIGKILL, not SIGTERM: the
// cancel paths here are timeout and shim-cancel, where Multica's own 2 s
// hard-exit follows — there is no window for a graceful codex shutdown, and
// the upstream cloud run is server-side and unaffected either way.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
