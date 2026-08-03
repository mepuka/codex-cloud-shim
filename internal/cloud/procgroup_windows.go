//go:build windows

package cloud

import "os/exec"

// setProcGroup is a no-op on Windows: there are no Unix process groups, and
// exec.CommandContext's default leader-kill plus WaitDelay is the available
// containment. Multica's daemon owns any further process-tree cleanup.
func setProcGroup(_ *exec.Cmd) {}
