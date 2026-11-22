//go:build !linux && !darwin

package daemon

import "os"

func inspectForegroundProc(_ *os.File, _ int) (fgCmd string, cwd string) {
	return "", ""
}
