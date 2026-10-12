//go:build !unix

package skills

import "os/exec"

// killGroupOnCancel is the unix process-group kill; elsewhere cancelling
// kills git alone and WaitDelay is what unblocks the loader.
func killGroupOnCancel(*exec.Cmd) {}
