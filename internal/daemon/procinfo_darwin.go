//go:build darwin

package daemon

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// inspectForegroundProc queries the PTY master fd for its foreground process
// group leader and reads its command name via sysctl kern.proc.pid (zero CGO,
// zero subprocesses, safe on Apple Silicon and corporate FUSE mounts).
// Working directory is populated from OSC 7 shell integration and initialCwd.
func inspectForegroundProc(ptmx *os.File, fallbackPID int) (fgCmd string, cwd string) {
	pid := fallbackPID
	if ptmx != nil {
		if pgrp, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPGRP); err == nil && pgrp > 0 {
			pid = pgrp
		}
	}
	if pid <= 0 {
		return "", ""
	}

	if kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid); err == nil && kp != nil {
		fgCmd = strings.TrimSpace(unix.ByteSliceToString(kp.Proc.P_comm[:]))
	}
	return fgCmd, ""
}
