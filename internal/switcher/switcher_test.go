package switcher

import (
	"bytes"
	"testing"

	"github.com/EricWF/mplex/internal/protocol"
)

func TestSwitcherPreferCreateAndExitOnEsc(t *testing.T) {
	sessions := []protocol.SessionInfo{
		{ID: "s1", Name: "main", Attached: true},
		{ID: "s2", Name: "api", Attached: false},
	}

	// 1. PreferCreate=true: pressing Enter immediately creates a new session
	// instead of selecting/stealing "main".
	inCh := make(chan []byte, 4)
	inCh <- []byte{'\r'}
	var out bytes.Buffer
	res := Run(Config{
		PreferCreate: true,
		ExitOnEsc:    true,
		Rows:         24,
		Cols:         100,
		InputCh:      inCh,
		Out:          &out,
		Callbacks: Callbacks{
			ListSessions: func() ([]protocol.SessionInfo, error) { return sessions, nil },
		},
	})
	if res.Action != ActionCreate || res.Target != "" {
		t.Fatalf("expected ActionCreate with empty target, got %+v", res)
	}

	// 2. PreferCreate=false when CurrentSessionID=="": prefers detached "api"
	// over attached "main".
	inCh2 := make(chan []byte, 4)
	inCh2 <- []byte{'\r'}
	out.Reset()
	res2 := Run(Config{
		PreferCreate: false,
		ExitOnEsc:    true,
		Rows:         24,
		Cols:         100,
		InputCh:      inCh2,
		Out:          &out,
		Callbacks: Callbacks{
			ListSessions: func() ([]protocol.SessionInfo, error) { return sessions, nil },
		},
	})
	if res2.Action != ActionSelect || res2.Target != "s2" {
		t.Fatalf("expected detached session s2 to be pre-selected, got %+v", res2)
	}

	// 3. ExitOnEsc=true: first Esc clears query, second Esc exits cleanly (ActionDetach).
	inCh3 := make(chan []byte, 4)
	inCh3 <- []byte("foo")
	inCh3 <- []byte{0x1b}
	inCh3 <- []byte{0x1b}
	out.Reset()
	res3 := Run(Config{
		PreferCreate: true,
		ExitOnEsc:    true,
		Rows:         24,
		Cols:         100,
		InputCh:      inCh3,
		Out:          &out,
		Callbacks: Callbacks{
			ListSessions: func() ([]protocol.SessionInfo, error) { return sessions, nil },
		},
	})
	if res3.Action != ActionDetach {
		t.Fatalf("expected ActionDetach on Esc with empty query, got %+v", res3)
	}
}
