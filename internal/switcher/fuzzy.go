package switcher

import (
	"sort"
	"strings"
	"unicode"

	"github.com/EricWF/mplex/internal/protocol"
)

// FuzzyResult holds a scored fuzzy match and the matched rune indices in Session.Name.
type FuzzyResult struct {
	Session   protocol.SessionInfo
	Score     int
	NameMatch []int // rune indices in Session.Name that matched
}

// FilterSessions performs fzf-style multi-token fuzzy matching against sessions.
// Each whitespace-separated query token must match somewhere across the session's
// name, foreground command, cwd, or window title. Matches in Name score highest.
func FilterSessions(sessions []protocol.SessionInfo, query string) []FuzzyResult {
	query = strings.TrimSpace(query)
	if query == "" {
		out := make([]FuzzyResult, len(sessions))
		for i, s := range sessions {
			out[i] = FuzzyResult{Session: s}
		}
		return out
	}

	tokens := strings.Fields(query)
	var results []FuzzyResult

	for _, s := range sessions {
		totalScore := 0
		var namePositions []int
		matchedAll := true

		for _, tok := range tokens {
			bestTokScore := -1
			var bestNamePos []int

			// 1. Try matching session name (highest priority)
			if sc, pos, ok := fuzzyScore(s.Name, tok); ok {
				boosted := sc + 40
				if strings.EqualFold(s.Name, tok) {
					boosted += 100
				} else if strings.HasPrefix(strings.ToLower(s.Name), strings.ToLower(tok)) {
					boosted += 30
				}
				bestTokScore = boosted
				bestNamePos = pos
			}

			// 2. Try matching foreground command
			if s.FgCommand != "" {
				if sc, _, ok := fuzzyScore(s.FgCommand, tok); ok && sc+15 > bestTokScore {
					bestTokScore = sc + 15
					bestNamePos = nil
				}
			}

			// 3. Try matching CWD
			if s.Cwd != "" {
				if sc, _, ok := fuzzyScore(s.Cwd, tok); ok && sc > bestTokScore {
					bestTokScore = sc
					bestNamePos = nil
				}
			}

			// 4. Try matching window title
			if s.Title != "" {
				if sc, _, ok := fuzzyScore(s.Title, tok); ok && sc+5 > bestTokScore {
					bestTokScore = sc + 5
					bestNamePos = nil
				}
			}

			if bestTokScore < 0 {
				matchedAll = false
				break
			}
			totalScore += bestTokScore
			if len(bestNamePos) > 0 {
				namePositions = append(namePositions, bestNamePos...)
			}
		}

		if matchedAll {
			results = append(results, FuzzyResult{
				Session:   s,
				Score:     totalScore,
				NameMatch: dedupeSorted(namePositions),
			})
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Session.LastActiveAt.After(results[j].Session.LastActiveAt)
	})

	return results
}

// fuzzyScore computes a case-insensitive subsequence match score and matched rune
// positions, rewarding prefix, word-boundary, camelCase, and consecutive matches.
func fuzzyScore(target, pattern string) (int, []int, bool) {
	tRunes := []rune(target)
	pRunes := []rune(pattern)
	if len(pRunes) == 0 {
		return 0, nil, true
	}
	if len(pRunes) > len(tRunes) {
		return 0, nil, false
	}

	tLower := make([]rune, len(tRunes))
	for i, r := range tRunes {
		tLower[i] = unicode.ToLower(r)
	}
	pLower := make([]rune, len(pRunes))
	for i, r := range pRunes {
		pLower[i] = unicode.ToLower(r)
	}

	// Quick subsequence feasibility check (forward pass) + find tightest match.
	firstIdx := make([]int, len(pLower))
	pi := 0
	for ti := 0; ti < len(tLower) && pi < len(pLower); ti++ {
		if tLower[ti] == pLower[pi] {
			firstIdx[pi] = ti
			pi++
		}
	}
	if pi < len(pLower) {
		return 0, nil, false
	}

	// Backward pass from the last matched position to tighten gaps (fzf heuristic).
	positions := make([]int, len(pLower))
	lastEnd := firstIdx[len(pLower)-1]
	ti := lastEnd
	for pj := len(pLower) - 1; pj >= 0; pj-- {
		for ti >= 0 {
			if tLower[ti] == pLower[pj] {
				positions[pj] = ti
				ti--
				break
			}
			ti--
		}
	}

	// Score the tightened match (also compare against forward match if boundary-better).
	scoreBack := evalMatchScore(tRunes, pRunes, positions)
	scoreFwd := evalMatchScore(tRunes, pRunes, firstIdx)
	if scoreFwd > scoreBack {
		return scoreFwd, firstIdx, true
	}
	return scoreBack, positions, true
}

func evalMatchScore(target, pattern []rune, pos []int) int {
	score := 0
	consecutive := 0
	for i, idx := range pos {
		score += 10
		if target[idx] == pattern[i] {
			score += 1 // exact case bonus
		}
		if idx == 0 {
			score += 20 // start of string
		} else {
			prev := target[idx-1]
			if prev == '/' || prev == '-' || prev == '_' || prev == '.' || prev == ' ' || prev == ':' {
				score += 16 // word / path boundary
			} else if unicode.IsLower(prev) && unicode.IsUpper(target[idx]) {
				score += 14 // camelCase boundary
			}
		}
		if i > 0 {
			gap := idx - pos[i-1] - 1
			if gap == 0 {
				consecutive++
				score += 12 + consecutive*4
			} else {
				consecutive = 0
				score -= gap * 2
			}
		}
	}
	// Small preference for shorter target strings.
	score -= len(target) / 4
	return score
}

func dedupeSorted(in []int) []int {
	if len(in) <= 1 {
		return in
	}
	sort.Ints(in)
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
