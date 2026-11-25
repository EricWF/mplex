package client

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/EricWF/mplex/internal/protocol"
)

const (
	// DefaultWaitTimeout is the default timeout for Wait when none is specified.
	DefaultWaitTimeout      = 30 * time.Second
	defaultWaitPollInterval = 50 * time.Millisecond
)

// ErrWaitTimeout is returned by Wait when the timeout expires before any wait
// condition is satisfied.
var ErrWaitTimeout = errors.New("wait timed out")

// WaitClient is the subset of RPCConn methods required by Wait.
type WaitClient interface {
	List() ([]protocol.SessionInfo, error)
	Capture(req protocol.CaptureReq) (protocol.CaptureResp, error)
}

// WaitOptions configures a Wait operation against a running session.
type WaitOptions struct {
	Target            string
	Match             string        // regular expression to match against captured text
	Idle              time.Duration // wait until session has no new activity for this duration
	Exit              bool          // wait until session exits (no longer present in List())
	IncludeScrollback bool          // include scrollback when checking Match
	Timeout           time.Duration // max duration to wait (defaults to DefaultWaitTimeout)
	PollInterval      time.Duration // polling interval (defaults to 50ms)
}

// WaitResult describes the outcome of a Wait call.
type WaitResult struct {
	Matched   bool                  `json:"matched"`
	Reason    string                `json:"reason"`
	Session   *protocol.SessionInfo `json:"session,omitempty"`
	MatchLine string                `json:"match_line,omitempty"`
	Text      string                `json:"text,omitempty"`
}

// Wait polls cli every PollInterval (default 50ms) until one of the requested
// conditions (--match, --idle, --exit) is satisfied or the timeout/context expires.
func Wait(ctx context.Context, cli WaitClient, opts WaitOptions) (WaitResult, error) {
	if strings.TrimSpace(opts.Target) == "" {
		return WaitResult{}, errors.New("target session is required")
	}
	if opts.Idle < 0 {
		return WaitResult{}, errors.New("idle duration must be positive")
	}
	if opts.Timeout < 0 {
		return WaitResult{}, errors.New("timeout duration must be positive")
	}
	if opts.Match == "" && opts.Idle == 0 && !opts.Exit {
		return WaitResult{}, errors.New("at least one wait condition (--match, --idle, or --exit) is required")
	}

	var re *regexp.Regexp
	if opts.Match != "" {
		var err error
		re, err = regexp.Compile(opts.Match)
		if err != nil {
			return WaitResult{}, fmt.Errorf("invalid match regex %q: %w", opts.Match, err)
		}
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultWaitTimeout
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultWaitPollInterval
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		lastSess *protocol.SessionInfo
		lastText string
		seenOnce bool
	)

	check := func() (WaitResult, bool, error) {
		if opts.Exit || opts.Idle > 0 {
			sessions, err := cli.List()
			if err != nil {
				return WaitResult{}, true, err
			}
			info, found := findSessionInfo(sessions, opts.Target)
			if !found {
				if opts.Exit {
					return WaitResult{
						Matched: true,
						Reason:  "exit",
						Session: lastSess,
						Text:    lastText,
					}, true, nil
				}
				if seenOnce {
					return WaitResult{
						Matched: false,
						Reason:  "exit",
						Session: lastSess,
						Text:    lastText,
					}, true, fmt.Errorf("session %q exited before condition was met", opts.Target)
				}
				return WaitResult{}, true, fmt.Errorf("session %q not found", opts.Target)
			}
			seenOnce = true
			infoCopy := info
			lastSess = &infoCopy
		}

		if re != nil {
			capResp, err := cli.Capture(protocol.CaptureReq{
				Target:            opts.Target,
				IncludeScrollback: opts.IncludeScrollback,
			})
			if err != nil {
				if isNotFoundErr(err) {
					if opts.Exit {
						return WaitResult{
							Matched: true,
							Reason:  "exit",
							Session: lastSess,
							Text:    lastText,
						}, true, nil
					}
					if seenOnce {
						return WaitResult{
							Matched: false,
							Reason:  "exit",
							Session: lastSess,
							Text:    lastText,
						}, true, fmt.Errorf("session %q exited before condition was met", opts.Target)
					}
				}
				return WaitResult{}, true, err
			}
			seenOnce = true
			sessCopy := capResp.Session
			lastSess = &sessCopy
			lastText = capResp.Text

			for _, line := range capResp.Lines {
				if re.MatchString(line) {
					return WaitResult{
						Matched:   true,
						Reason:    "match",
						Session:   &sessCopy,
						MatchLine: line,
						Text:      capResp.Text,
					}, true, nil
				}
			}
			if re.MatchString(capResp.Text) {
				return WaitResult{
					Matched:   true,
					Reason:    "match",
					Session:   &sessCopy,
					MatchLine: re.FindString(capResp.Text),
					Text:      capResp.Text,
				}, true, nil
			}
		}

		if opts.Idle > 0 && lastSess != nil && !lastSess.LastActiveAt.IsZero() {
			if time.Since(lastSess.LastActiveAt) >= opts.Idle {
				if re == nil {
					if capResp, err := cli.Capture(protocol.CaptureReq{
						Target:            opts.Target,
						IncludeScrollback: opts.IncludeScrollback,
					}); err == nil {
						sessCopy := capResp.Session
						lastSess = &sessCopy
						lastText = capResp.Text
						// Verify no new activity occurred between List and Capture.
						if !sessCopy.LastActiveAt.IsZero() && time.Since(sessCopy.LastActiveAt) < opts.Idle {
							return WaitResult{}, false, nil
						}
					}
				}
				return WaitResult{
					Matched: true,
					Reason:  "idle",
					Session: lastSess,
					Text:    lastText,
				}, true, nil
			}
		}

		return WaitResult{}, false, nil
	}

	if err := waitCtx.Err(); err != nil {
		return timeoutResult(ctx, lastSess, lastText)
	}
	if res, done, err := check(); done {
		return res, err
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return timeoutResult(ctx, lastSess, lastText)
		case <-ticker.C:
			if res, done, err := check(); done {
				return res, err
			}
		}
	}
}

func timeoutResult(parentCtx context.Context, lastSess *protocol.SessionInfo, lastText string) (WaitResult, error) {
	if parentCtx.Err() != nil {
		return WaitResult{
			Matched: false,
			Reason:  "canceled",
			Session: lastSess,
			Text:    lastText,
		}, parentCtx.Err()
	}
	return WaitResult{
		Matched: false,
		Reason:  "timeout",
		Session: lastSess,
		Text:    lastText,
	}, ErrWaitTimeout
}

func findSessionInfo(sessions []protocol.SessionInfo, target string) (protocol.SessionInfo, bool) {
	for _, s := range sessions {
		if s.ID == target {
			return s, true
		}
	}
	for _, s := range sessions {
		if s.Name == target {
			return s, true
		}
	}
	return protocol.SessionInfo{}, false
}

func isNotFoundErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}
