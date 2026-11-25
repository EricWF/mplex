//go:build linux

package daemon

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// inspectForegroundProc queries the PTY master fd for its current foreground
// process group leader and reads its command name and working directory from /proc.
func inspectForegroundProc(ptmx *os.File, fallbackPID int) (fgCmd string, cwd string) {
	pid := fallbackPID
	if ptmx != nil {
		if raw, err := ptmx.SyscallConn(); err == nil {
			_ = raw.Control(func(fd uintptr) {
				if pgrp, err := unix.IoctlGetInt(int(fd), unix.TIOCGPGRP); err == nil && pgrp > 0 {
					pid = pgrp
				}
			})
		}
	}
	if pid <= 0 {
		return "", ""
	}

	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		fgCmd = strings.TrimSpace(string(data))
	}
	if link, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		cwd = link
	}
	return fgCmd, cwd
}
