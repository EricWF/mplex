package client

import "testing"

func TestFormatWindowTitle(t *testing.T) {
	tests := []struct {
		name       string
		session    string
		innerTitle string
		want       string
	}{
		{
			name:       "empty inner title",
			session:    "api",
			innerTitle: "",
			want:       "[api]",
		},
		{
			name:       "whitespace inner title",
			session:    "api",
			innerTitle: "   ",
			want:       "[api]",
		},
		{
			name:       "non-empty inner title",
			session:    "api",
			innerTitle: "vim main.go",
			want:       "[api] vim main.go",
		},
		{
			name:       "already prefixed exact",
			session:    "api",
			innerTitle: "[api]",
			want:       "[api]",
		},
		{
			name:       "already prefixed with inner text",
			session:    "api",
			innerTitle: "[api] vim main.go",
			want:       "[api] vim main.go",
		},
		{
			name:       "already prefixed without space",
			session:    "api",
			innerTitle: "[api]zsh",
			want:       "[api]zsh",
		},
		{
			name:       "different bracketed name does not suppress prefix",
			session:    "api",
			innerTitle: "[api-2] vim",
			want:       "[api] [api-2] vim",
		},
		{
			name:       "trims surrounding spaces",
			session:    "  work  ",
			innerTitle: "  ~/code  ",
			want:       "[work] ~/code",
		},
		{
			name:       "empty session name returns inner title",
			session:    "",
			innerTitle: "bash",
			want:       "bash",
		},
		{
			name:       "both empty",
			session:    "",
			innerTitle: "",
			want:       "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatWindowTitle(tc.session, tc.innerTitle); got != tc.want {
				t.Fatalf("formatWindowTitle(%q, %q) = %q, want %q", tc.session, tc.innerTitle, got, tc.want)
			}
		})
	}
}

func TestFormatWindowTitleOSC(t *testing.T) {
	if got := string(formatWindowTitleOSC("api", "")); got != "\x1b]0;[api]\x07" {
		t.Fatalf("got %q, want %q", got, "\x1b]0;[api]\x07")
	}
	if got := string(formatWindowTitleOSC("api", "nvim")); got != "\x1b]0;[api] nvim\x07" {
		t.Fatalf("got %q, want %q", got, "\x1b]0;[api] nvim\x07")
	}
	if got := formatWindowTitleOSC("", ""); got != nil {
		t.Fatalf("expected nil OSC for empty inputs, got %q", got)
	}
}
