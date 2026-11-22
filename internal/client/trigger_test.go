package client

import (
	"bytes"
	"testing"
)

func TestTriggerKeyMatching(t *testing.T) {
	tk, err := ParseTriggerKey("ctrl-\\")
	if err != nil {
		t.Fatalf("ParseTriggerKey: %v", err)
	}

	// 1. Legacy raw byte 0x1c surrounded by normal input
	before, matched, after, ok := tk.Match([]byte("abc\x1cdef"))
	if !ok || string(before) != "abc" || !bytes.Equal(matched, []byte{0x1c}) || string(after) != "def" {
		t.Fatalf("legacy match failed: ok=%v before=%q matched=%x after=%q", ok, before, matched, after)
	}

	// 2. Kitty keyboard protocol CSI u (press & repeat)
	for _, seq := range []string{"\x1b[92;5u", "\x1b[92;5:1u", "\x1b[92;5:2u", "\x1b[92;69u"} {
		_, m, _, ok := tk.Match([]byte(seq))
		if !ok || string(m) != seq {
			t.Fatalf("kitty sequence %q failed to match: ok=%v m=%q", seq, ok, m)
		}
	}

	// 3. Kitty key release (:3) MUST NOT trigger!
	if _, _, _, ok := tk.Match([]byte("\x1b[92;5:3u")); ok {
		t.Fatalf("kitty key release must not trigger switcher")
	}

	// 4. xterm modifyOtherKeys
	if _, m, _, ok := tk.Match([]byte("\x1b[27;5;92~")); !ok || string(m) != "\x1b[27;5;92~" {
		t.Fatalf("modifyOtherKeys failed to match: ok=%v m=%q", ok, m)
	}

	// 5. Bare Escape, arrows, and SGR mouse MUST pass straight through!
	for _, pass := range []string{"\x1b", "\x1b[A", "\x1b[<0;12;34M", "\x1b[97;5u", "normal text"} {
		if _, _, _, ok := tk.Match([]byte(pass)); ok {
			t.Fatalf("sequence %q should not have triggered", pass)
		}
	}

	// 6. Disabled ("none")
	none, err := ParseTriggerKey("none")
	if err != nil || !none.Disabled {
		t.Fatalf("expected disabled trigger key, got %+v err=%v", none, err)
	}
	if _, _, _, ok := none.Match([]byte{0x1c}); ok {
		t.Fatalf("disabled trigger key must never match")
	}
}
