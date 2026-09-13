package headless

import (
	"os/exec"
)

func detachedCommand(exe string, args []string) *exec.Cmd {
	cmd := exec.Command(exe, args...)
	setDetached(cmd)
	return cmd
}
