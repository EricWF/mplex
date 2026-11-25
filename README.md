# mplex

A terminal multiplexer engineered to **act identically to a bare, native terminal** — preserving your terminal emulator's native scrollback, mouse selection, trackpad scrolling, copy/paste, OSC 52 clipboard, Kitty keyboard protocol, hyperlinks, and truecolor — while adding persistent sessions and a fast, fzf-style interactive session switcher.

## How It Works

Unlike `tmux` and `screen` (which run inside the alternate screen buffer and rewrite terminal output through a virtual pane grid), `mplex` uses a **Transparent Passthrough + Shadow VT** architecture:

1. **Live data path (while attached):** Your outer terminal stays on its **main screen buffer**. Raw bytes flow straight between your outer terminal and the active session's PTY — no `TERM` translation, no hijacked mouse, no broken scrollback.
2. **Shadow VT state tracker (in the background daemon):** Every session's PTY output is passively mirrored into a full VT500/xterm/Kitty state machine tracking scrollback (with soft-wrap reflow), main & alternate screens, SGR/truecolor/undercurls, OSC 8 hyperlinks, window titles, mouse modes, bracketed paste, cursor style, and Kitty keyboard stacks.
3. **Atomic session switching:** When you switch sessions, `mplex` emits a single synchronized-output frame (`\x1b[?2026h`…`\x1b[?2026l`) that cleanly resets outer modes, clears outer scrollback (`\x1b[3J`), **repopulates your outer terminal's native scrollback buffer** with the target session's scrollback, paints the main screen (and alt screen on top if running `vim`/`htop`/etc.), and restores exact active modes.
4. **Detached query responder:** When a session has no client attached, the daemon answers terminal queries (`DSR`, `DA1/DA2`, `DECRQM`, Kitty keyboard query, snooped `OSC 10/11`) so backgrounded apps never stall.

## Installing & Building

Works on **Linux** and **macOS (Apple Silicon `darwin/arm64` & Intel `darwin/amd64`)** with zero CGO dependencies:

```bash
# Install directly with Go (recommended — no Gatekeeper quarantine on macOS)
go install github.com/EricWF/mplex/cmd/mplex@latest

# Or build from a local clone
make build        # builds ./mplex
make install      # installs to $GOPATH/bin/mplex
make release-local # cross-compiles darwin/{arm64,amd64} & linux/{amd64,arm64}
```

Prebuilt tarballs for `darwin/arm64`, `darwin/amd64`, `linux/amd64`, and `linux/arm64` are attached to [GitHub Releases](https://github.com/EricWF/mplex/releases). For example, on an Apple Silicon Mac:

```bash
mkdir -p ~/.local/bin
curl -sSL https://github.com/EricWF/mplex/releases/latest/download/mplex-darwin-arm64.tar.gz | tar -xz -C ~/.local/bin
```

> **Note for macOS:** `mplex` tracks each session's live foreground process via native `sysctl` (`kern.proc.pid`) and live working directory via OSC 7 (automatically emitted by Ghostty, Kitty, WezTerm, iTerm2, and macOS Terminal/zsh).

> **Note on unsigned macOS binaries:** If you download a prebuilt binary from a browser on macOS, Gatekeeper adds a quarantine attribute (`xattr -d com.apple.quarantine ./mplex` clears it; binaries built locally with `go install` or `go build` don't have quarantine attributes).

## Usage

```bash
# Start a session (or resume if detached).
# When another terminal window is already running an mplex session,
# opens the session creation/picker screen instead of stealing it!
./mplex

# Attach to (or create) a named session
./mplex api

# Open the fzf-style session switcher immediately
./mplex --pick

# Manage sessions from the CLI
./mplex list
./mplex new work -c ~/projects/work
./mplex new build -d -- make watch
./mplex rename work backend
./mplex kill build
./mplex detach

# Check GitHub Releases and upgrade in place (also prompts automatically when a new release drops)
./mplex update
./mplex update --check
```

> **Auto-update checks:** On interactive startup, tagged release builds check GitHub Releases at most once every 24 hours (cached locally) and prompt before downloading, SHA-256-verifying, and replacing the binary. Set `MPLEX_NO_UPDATE_CHECK=1` to disable automatic checks.

## Triggering the Session Switcher

### Option 1: In-band hotkey (default: `Ctrl+\`)
Press `Ctrl+\` at any time to drop into the fzf-style switcher.
- Works across legacy encoding, **Kitty keyboard protocol (`CSI u`)**, and **xterm `modifyOtherKeys`** with zero Escape-key lag.
- Press `Ctrl+\` a second time inside the switcher to send a literal `Ctrl+\` to your session.
- Customize with `--key` or `MPLEX_KEY` (e.g., `ctrl-space`, `ctrl-]`, `alt-s`, `ctrl-a`).

### Option 2: Zero-byte interception (out-of-band terminal keybinding)
Run `mplex` with `--key=none` (or `export MPLEX_KEY=none`) so **literally zero keyboard bytes** are ever intercepted, and bind a native shortcut in your terminal emulator to run `mplex switch` (or simply run `mplex` from inside a session).

For example, in **Ghostty** (`~/.config/ghostty/config`) or **Kitty** / **WezTerm**, bind `Cmd+K` or `Ctrl+Shift+P` to invoke `mplex switch` in the background.

## Switcher Keybindings

| Key | Action |
|---|---|
| Type anything | Fuzzy-filter by name, command, directory, or title |
| `Enter` | Switch to selected session (or create if `+ Create new session` is selected) |
| `Tab` | Quick-toggle to previously active session |
| `Alt+Enter` / `Ctrl+O` | Force-create a new session (using typed query as name) |
| `Up`/`Down` / `Ctrl+P`/`Ctrl+N` / `Ctrl+K`/`Ctrl+J` | Move selection (mouse wheel & click also work) |
| `Ctrl+R` | Rename selected session inline |
| `Ctrl+X` / `Ctrl+D` | Kill selected session (with confirmation) |
| `Ctrl+Q` / `Alt+D` | Detach cleanly (sessions keep running in background) |
| `Esc` / `Ctrl+C` | Cancel and return seamlessly to current session (or quit when opened unattached) |
| Trigger key (2nd tap) | Send literal trigger key to current session |

## Automation & LLM-Friendly CLI

Every session-management command works non-interactively and supports `--json` output, so AI assistants and scripts can inspect, drive, and switch your terminal sessions without stealing your window:

```bash
# Structured session inventory (includes id, pid, fg_command, cwd, title, scrollback, attached)
mplex list --json

# Create detached sessions with structured metadata output
mplex new api -d -c ~/projects/api --json -- npm run dev

# Read live screen or scrollback output from any session (soft-wrapped lines joined by default)
mplex capture api --scrollback --lines 100
mplex capture api --json

# Send commands or special keys to a session without detaching/stealing the user's window
mplex send api "npm test" --enter
mplex send api --key ctrl-c --json

# Remotely switch the active terminal window to a target session
mplex switch api --json

# Rename or kill sessions with JSON responses
mplex rename api backend --json
mplex kill backend --json
```
