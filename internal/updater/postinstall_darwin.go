//go:build darwin

package updater

import "os/exec"

func postInstallBinary(targetPath string) {
	// Best-effort clear macOS Gatekeeper quarantine attribute if inherited/present.
	_ = exec.Command("xattr", "-d", "com.apple.quarantine", targetPath).Run()
}
