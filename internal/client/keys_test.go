package client

import (
	"bytes"
	"testing"
)

func TestParseSendKey(t *testing.T) {
	tests := []struct {
		spec string
		want []byte
	}{
		{"enter", []byte{'\r'}},
		{"Return", []byte{'\r'}},
		{"lf", []byte{'\n'}},
		{"tab", []byte{'\t'}},
		{"esc", []byte{0x1b}},
		{"space", []byte{' '}},
		{"backspace", []byte{0x7f}},
		{"up", []byte("\x1b[A")},
		{"down", []byte("\x1b[B")},
		{"right", []byte("\x1b[C")},
		{"left", []byte("\x1b[D")},
		{"home", []byte("\x1b[H")},
		{"end", []byte("\x1b[F")},
		{"pgup", []byte("\x1b[5~")},
		{"pgdn", []byte("\x1b[6~")},
		{"del", []byte("\x1b[3~")},
		{"ctrl-c", []byte{0x03}},
		{"Ctrl+D", []byte{0x04}},
		{"^c", []byte{0x03}},
		{"c-z", []byte{0x1a}},
		{"ctrl-\\", []byte{0x1c}},
		{"ctrl-space", []byte{0x00}},
		{"alt-x", []byte{0x1b, 'x'}},
		{"M-b", []byte{0x1b, 'b'}},
	}
	for _, tc := range tests {
		got, err := ParseSendKey(tc.spec)
		if err != nil {
			t.Errorf("ParseSendKey(%q) unexpected error: %v", tc.spec, err)
			continue
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("ParseSendKey(%q) = %x, want %x", tc.spec, got, tc.want)
		}
	}

	invalid := []string{"", "ctrl-123", "alt-enter", "unknown-key"}
	for _, spec := range invalid {
		if got, err := ParseSendKey(spec); err == nil {
			t.Errorf("ParseSendKey(%q) = %x, expected error", spec, got)
		}
	}
}
