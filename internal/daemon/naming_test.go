package daemon

import (
	"fmt"
	"testing"
)

func TestAllocateUniqueName(t *testing.T) {
	t.Setenv("HOME", "/home/tester")

	addDummy := func(d *Daemon, name string) {
		id := fmt.Sprintf("s%d", len(d.sessions)+1)
		d.sessions[id] = &Session{id: id, name: name}
	}

	t.Run("cwd mode derives basename and deduplicates", func(t *testing.T) {
		d := New("/tmp/unused.sock")

		got1 := d.allocateUniqueName("", "/home/tester/work/api", "")
		if got1 != "api" {
			t.Fatalf("1st name = %q, want %q", got1, "api")
		}
		addDummy(d, got1)

		got2 := d.allocateUniqueName("", "/home/tester/work/api/", "cwd")
		if got2 != "api-2" {
			t.Fatalf("2nd name = %q, want %q", got2, "api-2")
		}
		addDummy(d, got2)

		got3 := d.allocateUniqueName("", "/other/path/api", "cwd")
		if got3 != "api-3" {
			t.Fatalf("3rd name = %q, want %q", got3, "api-3")
		}
	})

	t.Run("cwd mode sanitizes whitespace and control chars", func(t *testing.T) {
		d := New("/tmp/unused.sock")

		got1 := d.allocateUniqueName("", "/tmp/my   cool\tproject", "cwd")
		if got1 != "my-cool-project" {
			t.Fatalf("got %q, want %q", got1, "my-cool-project")
		}
		addDummy(d, got1)

		got2 := d.allocateUniqueName("", "/tmp/foo\x07bar", "cwd")
		if got2 != "foo-bar" {
			t.Fatalf("got %q, want %q", got2, "foo-bar")
		}
		addDummy(d, got2)

		got3 := d.allocateUniqueName("", "/tmp/   ", "cwd")
		if got3 != "main" {
			t.Fatalf("whitespace-only basename = %q, want %q", got3, "main")
		}
	})

	t.Run("home dir, root dir, and empty cwd fall back to main", func(t *testing.T) {
		d := New("/tmp/unused.sock")

		if got := d.allocateUniqueName("", "/home/tester", "cwd"); got != "main" {
			t.Fatalf("HOME dir = %q, want %q", got, "main")
		}
		addDummy(d, "main")

		if got := d.allocateUniqueName("", "/home/tester/", "cwd"); got != "main-2" {
			t.Fatalf("HOME trailing slash dedup = %q, want %q", got, "main-2")
		}
		addDummy(d, "main-2")

		if got := d.allocateUniqueName("", "/", "cwd"); got != "main-3" {
			t.Fatalf("root dir = %q, want %q", got, "main-3")
		}
		addDummy(d, "main-3")

		if got := d.allocateUniqueName("", "", "cwd"); got != "main-4" {
			t.Fatalf("empty cwd = %q, want %q", got, "main-4")
		}
	})

	t.Run("explicit desired name overrides cwd and deduplicates", func(t *testing.T) {
		d := New("/tmp/unused.sock")

		got1 := d.allocateUniqueName("deploy", "/home/tester/work/api", "cwd")
		if got1 != "deploy" {
			t.Fatalf("1st desired = %q, want %q", got1, "deploy")
		}
		addDummy(d, got1)

		got2 := d.allocateUniqueName("deploy", "/home/tester/work/api", "numeric")
		if got2 != "deploy-2" {
			t.Fatalf("2nd desired = %q, want %q", got2, "deploy-2")
		}
	})

	t.Run("numeric mode preserves legacy main, 1, 2 naming", func(t *testing.T) {
		d := New("/tmp/unused.sock")

		want := []string{"main", "1", "2", "3"}
		for i, w := range want {
			got := d.allocateUniqueName("", "/home/tester/work/api", "numeric")
			if got != w {
				t.Fatalf("numeric[%d] = %q, want %q", i, got, w)
			}
			addDummy(d, got)
		}
	})
}
