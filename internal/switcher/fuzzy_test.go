package switcher

import (
	"testing"
	"time"

	"github.com/EricWF/mplex/internal/protocol"
)

func TestFilterSessionsRankingAndTokens(t *testing.T) {
	now := time.Now()
	sessions := []protocol.SessionInfo{
		{ID: "s1", Name: "backend-api", FgCommand: "go", Cwd: "/home/eric/work/api", LastActiveAt: now.Add(-1 * time.Minute)},
		{ID: "s2", Name: "api", FgCommand: "nvim", Cwd: "/home/eric/work/api", LastActiveAt: now.Add(-5 * time.Minute)},
		{ID: "s3", Name: "frontend", FgCommand: "npm", Cwd: "/home/eric/work/web", LastActiveAt: now},
		{ID: "s4", Name: "scratch-pad", FgCommand: "bash", Cwd: "/tmp", LastActiveAt: now.Add(-2 * time.Minute)},
	}

	// Exact name match should outrank substring match!
	res := FilterSessions(sessions, "api")
	if len(res) != 2 || res[0].Session.Name != "api" || res[1].Session.Name != "backend-api" {
		t.Fatalf("unexpected ranking for 'api': %+v", res)
	}
	if len(res[0].NameMatch) != 3 {
		t.Fatalf("expected 3 matched indices for 'api', got %v", res[0].NameMatch)
	}

	// Multi-token AND match across name + fg command
	res2 := FilterSessions(sessions, "api nvim")
	if len(res2) != 1 || res2[0].Session.ID != "s2" {
		t.Fatalf("multi-token filter failed: %+v", res2)
	}

	// Subsequence match ("bapi" -> "backend-api")
	res3 := FilterSessions(sessions, "bapi")
	if len(res3) != 1 || res3[0].Session.ID != "s1" {
		t.Fatalf("subsequence filter failed: %+v", res3)
	}
}
