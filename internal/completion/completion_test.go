package completion

import (
	"os/exec"
	"strings"
	"testing"
)

var expectedSubcommands = []string{
	"new", "attach", "switch", "list",
	"capture", "read", "logs", "send", "follow", "wait", "mcp",
	"rename", "kill", "detach",
	"save", "restore", "config", "completion",
	"update", "version", "help",
}

func TestGenerateSupportedShells(t *testing.T) {
	for _, shell := range []string{"fish", "zsh", "bash", "FISH", "Zsh", " Bash "} {
		t.Run(shell, func(t *testing.T) {
			out, err := Generate(shell)
			if err != nil {
				t.Fatalf("Generate(%q) error: %v", shell, err)
			}
			if strings.TrimSpace(out) == "" {
				t.Fatalf("Generate(%q) returned empty script", shell)
			}
			for _, sub := range expectedSubcommands {
				if !strings.Contains(out, sub) {
					t.Errorf("Generate(%q) missing subcommand %q", shell, sub)
				}
			}
			if !strings.Contains(out, "__complete-sessions") {
				t.Errorf("Generate(%q) missing dynamic __complete-sessions call", shell)
			}
		})
	}
}

func TestGenerateKeyFlagsPresent(t *testing.T) {
	// Fish defines long flags with `-l <name>`, zsh/bash with `--<name>`.
	flags := []string{"json", "socket", "key", "pick", "scrollback", "lines", "enter", "all", "file", "with-scrollback", "force", "path", "init"}
	for _, shell := range SupportedShells {
		out, err := Generate(shell)
		if err != nil {
			t.Fatalf("Generate(%q): %v", shell, err)
		}
		for _, fl := range flags {
			needle := "--" + fl
			if shell == "fish" {
				needle = "-l " + fl
			}
			if !strings.Contains(out, needle) {
				t.Errorf("shell %s missing flag %q", shell, needle)
			}
		}
	}
}

func TestGenerateUnsupportedShell(t *testing.T) {
	for _, bad := range []string{"", "tcsh", "powershell", "nu"} {
		if _, err := Generate(bad); err == nil {
			t.Errorf("Generate(%q) expected error, got nil", bad)
		}
	}
}

func TestGeneratedScriptsPassShellSyntaxCheck(t *testing.T) {
	checks := []struct {
		shell string
		bin   string
		args  []string
	}{
		{"bash", "bash", []string{"-n"}},
		{"zsh", "zsh", []string{"-n"}},
		{"fish", "fish", []string{"--no-execute"}},
	}

	for _, tc := range checks {
		t.Run(tc.shell, func(t *testing.T) {
			path, err := exec.LookPath(tc.bin)
			if err != nil {
				t.Skipf("%s not installed", tc.bin)
			}
			script, err := Generate(tc.shell)
			if err != nil {
				t.Fatalf("Generate(%q): %v", tc.shell, err)
			}
			cmd := exec.Command(path, tc.args...)
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s syntax check failed: %v\n%s", tc.shell, err, out)
			}
		})
	}
}
