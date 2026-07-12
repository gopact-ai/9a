//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package executable

import (
	"os/exec"

	"github.com/gopact-ai/9a/internal/processgroup"
)

func configureProcessGroup(cmd *exec.Cmd) {
	processgroup.Configure(cmd)
}

func killProcessGroup(cmd *exec.Cmd) {
	_ = processgroup.Kill(cmd)
}
