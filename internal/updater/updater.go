package updater

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultRepo            = "EricWF/mplex"
	DefaultAPIBaseURL      = "https://api.github.com"
	DefaultCheckInterval   = 24 * time.Hour
	DefaultErrorBackoff    = 1 * time.Hour
	DefaultPromptSnooze    = 24 * time.Hour
	DefaultCheckTimeout    = 2 * time.Second
	DefaultDownloadTimeout = 60 * time.Second

	maxChecksumBytes = 1 * 1024 * 1024
	maxArchiveBytes  = 100 * 1024 * 1024
	maxBinaryBytes   = 150 * 1024 * 1024
)

// Config controls how update checks and upgrades are performed.
type Config struct {
	CurrentVersion  string
	Repo            string
	APIBaseURL      string
	CachePath       string
	TargetPath      string // executable path to replace; defaults to current os.Executable()
	GOOS            string // defaults to runtime.GOOS
	GOARCH          string // defaults to runtime.GOARCH
	CheckInterval   time.Duration
	ErrorBackoff    time.Duration
	PromptSnooze    time.Duration
	CheckTimeout    time.Duration
	DownloadTimeout time.Duration
	HTTPClient      *http.Client
	Now             func() time.Time
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.Repo == "" {
		out.Repo = DefaultRepo
	}
	if out.APIBaseURL == "" {
		out.APIBaseURL = DefaultAPIBaseURL
	}
	out.APIBaseURL = strings.TrimRight(out.APIBaseURL, "/")
	if out.CachePath == "" {
		out.CachePath = DefaultCachePath()
	}
	if out.GOOS == "" {
		out.GOOS = runtime.GOOS
	}
	if out.GOARCH == "" {
		out.GOARCH = runtime.GOARCH
	}
	if out.CheckInterval <= 0 {
		out.CheckInterval = DefaultCheckInterval
	}
	if out.ErrorBackoff <= 0 {
		out.ErrorBackoff = DefaultErrorBackoff
	}
	if out.PromptSnooze <= 0 {
		out.PromptSnooze = DefaultPromptSnooze
	}
	if out.CheckTimeout <= 0 {
		out.CheckTimeout = DefaultCheckTimeout
	}
	if out.DownloadTimeout <= 0 {
		out.DownloadTimeout = DefaultDownloadTimeout
	}
	if out.Now == nil {
		out.Now = time.Now
	}
	return out
}

// DefaultCachePath returns the path to the local update check cache file.
func DefaultCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "mplex", "update-check.json")
}

// Release describes the latest GitHub release and matching platform archive.
type Release struct {
	Version     string `json:"version"`
	ReleaseURL  string `json:"release_url,omitempty"`
	AssetName   string `json:"asset_name,omitempty"`
	AssetURL    string `json:"asset_url,omitempty"`
	ChecksumURL string `json:"checksum_url,omitempty"`
}

// CheckCache is persisted on disk to throttle GitHub API checks and remember user choices.
type CheckCache struct {
	CheckedAt      time.Time `json:"checked_at,omitempty"`
	FailedAt       time.Time `json:"failed_at,omitempty"`
	LatestVersion  string    `json:"latest_version,omitempty"`
	ReleaseURL     string    `json:"release_url,omitempty"`
	AssetName      string    `json:"asset_name,omitempty"`
	AssetURL       string    `json:"asset_url,omitempty"`
	ChecksumURL    string    `json:"checksum_url,omitempty"`
	LastPromptedAt time.Time `json:"last_prompted_at,omitempty"`
	SkippedVersion string    `json:"skipped_version,omitempty"`
}

// CheckResult reports the outcome of checking GitHub for a newer release.
type CheckResult struct {
	CurrentVersion  string
	LatestVersion   string
	Release         Release
	UpdateAvailable bool
	FromCache       bool
	ShouldPrompt    bool
}

// PromptResult reports what happened during an interactive startup update prompt.
type PromptResult struct {
	Prompted   bool
	Upgraded   bool
	Skipped    bool
	NewVersion string
	TargetPath string
}

// IsDisabledByEnv reports whether auto-update checks are disabled via environment variables.
func IsDisabledByEnv() bool {
	if isTruthyEnv(os.Getenv("MPLEX_SKIP_UPDATE_ONCE")) {
		return true
	}
	if isTruthyEnv(os.Getenv("MPLEX_NO_UPDATE_CHECK")) {
		return true
	}
	return false
}

// IsForcedByEnv reports whether startup checks are explicitly forced (even for dev builds).
func IsForcedByEnv() bool {
	return isTruthyEnv(os.Getenv("MPLEX_FORCE_UPDATE_CHECK"))
}

func isTruthyEnv(v string) bool {
	s := strings.TrimSpace(strings.ToLower(v))
	return s != "" && s != "0" && s != "false" && s != "no" && s != "off"
}

type semver struct {
	major int
	minor int
	patch int
	pre   string
}

// IsReleaseVersion reports whether v looks like an official tagged semver release
// (excluding "dev", "(devel)", "-dirty", or git-describe commit suffixes like "-3-gabc1234").
func IsReleaseVersion(v string) bool {
	sv, ok := parseSemver(v)
	if !ok {
		return false
	}
	if sv.pre != "" {
		if strings.Contains(sv.pre, "dirty") || isGitDescribeSuffix(sv.pre) {
			return false
		}
	}
	return true
}

func isGitDescribeSuffix(pre string) bool {
	// Matches "<commits>-g<hex>" (with optional "-dirty"), e.g., "3-g6ad1397".
	parts := strings.Split(pre, "-")
	if len(parts) < 2 {
		return false
	}
	if _, err := strconv.Atoi(parts[0]); err != nil {
		return false
	}
	if len(parts[1]) < 2 || parts[1][0] != 'g' {
		return false
	}
	for _, c := range parts[1][1:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func parseSemver(v string) (semver, bool) {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return semver{}, false
	}
	if idx := strings.IndexByte(s, '+'); idx >= 0 {
		s = s[:idx]
	}
	var pre string
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		pre = s[idx+1:]
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	pat, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || maj < 0 || min < 0 || pat < 0 {
		return semver{}, false
	}
	return semver{major: maj, minor: min, patch: pat, pre: pre}, true
}

// CompareVersions compares two semver strings, returning -1 if a < b, 0 if a == b, 1 if a > b.
func CompareVersions(a, b string) (int, bool) {
	sa, okA := parseSemver(a)
	sb, okB := parseSemver(b)
	if !okA || !okB {
		return 0, false
	}
	if sa.major != sb.major {
		return cmpInt(sa.major, sb.major), true
	}
	if sa.minor != sb.minor {
		return cmpInt(sa.minor, sb.minor), true
	}
	if sa.patch != sb.patch {
		return cmpInt(sa.patch, sb.patch), true
	}
	if sa.pre == sb.pre {
		return 0, true
	}
	if sa.pre == "" {
		return 1, true
	}
	if sb.pre == "" {
		return -1, true
	}
	ap := strings.Split(sa.pre, ".")
	bp := strings.Split(sb.pre, ".")
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if ap[i] == bp[i] {
			continue
		}
		an, aErr := strconv.Atoi(ap[i])
		bn, bErr := strconv.Atoi(bp[i])
		if aErr == nil && bErr == nil {
			return cmpInt(an, bn), true
		}
		if aErr == nil {
			return -1, true
		}
		if bErr == nil {
			return 1, true
		}
		if ap[i] < bp[i] {
			return -1, true
		}
		return 1, true
	}
	return cmpInt(len(ap), len(bp)), true
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// IsNewer reports whether latest is strictly newer than current.
// Non-release/dev current versions return false so local dev builds are not auto-prompted.
func IsNewer(current, latest string) bool {
	if !IsReleaseVersion(current) || !IsReleaseVersion(latest) {
		return false
	}
	cmp, ok := CompareVersions(current, latest)
	return ok && cmp < 0
}

// ExpectedAssetName returns the GitHub release archive name for GOOS/GOARCH.
func ExpectedAssetName(goos, goarch string) string {
	return fmt.Sprintf("mplex-%s-%s.tar.gz", goos, goarch)
}

type githubRelease struct {
	TagName    string        `json:"tag_name"`
	HTMLURL    string        `json:"html_url"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	Assets     []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// FetchLatest queries the GitHub Releases API for the latest non-draft release.
func FetchLatest(ctx context.Context, cfg Config) (Release, error) {
	c := cfg.withDefaults()
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.APIBaseURL, c.Repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	uaVer := c.CurrentVersion
	if uaVer == "" {
		uaVer = "dev"
	}
	req.Header.Set("User-Agent", "mplex/"+uaVer)

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: c.CheckTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("github api returned HTTP %d", resp.StatusCode)
	}

	var gr githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&gr); err != nil {
		return Release{}, fmt.Errorf("decode github release: %w", err)
	}
	if gr.TagName == "" {
		return Release{}, errors.New("github release missing tag_name")
	}

	wantAsset := ExpectedAssetName(c.GOOS, c.GOARCH)
	rel := Release{
		Version:    gr.TagName,
		ReleaseURL: gr.HTMLURL,
	}
	for _, a := range gr.Assets {
		switch a.Name {
		case wantAsset:
			rel.AssetName = a.Name
			rel.AssetURL = a.BrowserDownloadURL
		case "checksums.txt":
			rel.ChecksumURL = a.BrowserDownloadURL
		}
	}
	return rel, nil
}

// LoadCache reads the local update check cache file, returning an empty cache if missing/corrupt.
func LoadCache(path string) CheckCache {
	if path == "" {
		return CheckCache{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return CheckCache{}
	}
	var c CheckCache
	if err := json.Unmarshal(data, &c); err != nil {
		return CheckCache{}
	}
	return c
}

// SaveCache atomically writes the update check cache file.
func SaveCache(path string, cache CheckCache) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".update-check-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""
	return nil
}

// Check inspects GitHub (or the local cache when fresh) to determine if a newer release exists.
func Check(ctx context.Context, cfg Config, forceNetwork bool) (CheckResult, CheckCache, error) {
	c := cfg.withDefaults()
	now := c.Now()
	cache := LoadCache(c.CachePath)

	var rel Release
	fromCache := false

	cacheValid := !forceNetwork && !cache.CheckedAt.IsZero() && now.Sub(cache.CheckedAt) < c.CheckInterval
	inErrorBackoff := !forceNetwork && !cache.FailedAt.IsZero() && now.Sub(cache.FailedAt) < c.ErrorBackoff

	if cacheValid {
		fromCache = true
		rel = Release{
			Version:     cache.LatestVersion,
			ReleaseURL:  cache.ReleaseURL,
			AssetName:   cache.AssetName,
			AssetURL:    cache.AssetURL,
			ChecksumURL: cache.ChecksumURL,
		}
	} else if inErrorBackoff {
		return CheckResult{CurrentVersion: c.CurrentVersion, FromCache: true}, cache, nil
	} else {
		checkCtx, cancel := context.WithTimeout(ctx, c.CheckTimeout)
		fetched, err := FetchLatest(checkCtx, c)
		cancel()
		if err != nil {
			cache.FailedAt = now
			_ = SaveCache(c.CachePath, cache)
			return CheckResult{CurrentVersion: c.CurrentVersion}, cache, err
		}
		rel = fetched
		cache.CheckedAt = now
		cache.FailedAt = time.Time{}
		cache.LatestVersion = rel.Version
		cache.ReleaseURL = rel.ReleaseURL
		cache.AssetName = rel.AssetName
		cache.AssetURL = rel.AssetURL
		cache.ChecksumURL = rel.ChecksumURL
		_ = SaveCache(c.CachePath, cache)
	}

	newer := IsNewer(c.CurrentVersion, rel.Version)
	if !newer && !IsReleaseVersion(c.CurrentVersion) && IsForcedByEnv() && IsReleaseVersion(rel.Version) {
		newer = true
	}
	available := newer && rel.AssetURL != ""
	shouldPrompt := available &&
		cache.SkippedVersion != rel.Version &&
		(cache.LastPromptedAt.IsZero() || now.Sub(cache.LastPromptedAt) >= c.PromptSnooze)

	return CheckResult{
		CurrentVersion:  c.CurrentVersion,
		LatestVersion:   rel.Version,
		Release:         rel,
		UpdateAvailable: available,
		FromCache:       fromCache,
		ShouldPrompt:    shouldPrompt,
	}, cache, nil
}

// PromptIfAvailable checks for a newer GitHub release (respecting cache/env settings)
// and prompts the user interactively on out/in before upgrading.
func PromptIfAvailable(ctx context.Context, cfg Config, in io.Reader, out io.Writer) (PromptResult, error) {
	c := cfg.withDefaults()
	if IsDisabledByEnv() {
		return PromptResult{}, nil
	}
	if !IsReleaseVersion(c.CurrentVersion) && !IsForcedByEnv() {
		return PromptResult{}, nil
	}

	res, cache, err := Check(ctx, c, false)
	if err != nil || !res.ShouldPrompt {
		return PromptResult{}, nil
	}

	fmt.Fprintf(out, "\nmplex %s is available (current: %s).\n", res.LatestVersion, c.CurrentVersion)
	if res.Release.ReleaseURL != "" {
		fmt.Fprintf(out, "Release notes: %s\n", res.Release.ReleaseURL)
	}
	fmt.Fprintf(out, "Upgrade now? [y/N/s(kip version)] ")

	reader := bufio.NewReader(in)
	line, readErr := reader.ReadString('\n')
	if readErr != nil && len(line) == 0 {
		// Treat EOF / aborted read as "No" and snooze prompt.
		cache.LastPromptedAt = c.Now()
		_ = SaveCache(c.CachePath, cache)
		fmt.Fprintln(out)
		return PromptResult{Prompted: true}, nil
	}

	ans := strings.TrimSpace(strings.ToLower(line))
	switch ans {
	case "y", "yes":
		fmt.Fprintf(out, "Downloading mplex %s...\n", res.LatestVersion)
		target, upErr := Upgrade(ctx, c, res.Release)
		if upErr != nil {
			cache.LastPromptedAt = c.Now()
			_ = SaveCache(c.CachePath, cache)
			fmt.Fprintf(out, "mplex: upgrade failed: %v\nContinuing with %s...\n\n", upErr, c.CurrentVersion)
			return PromptResult{Prompted: true}, nil
		}
		cache.SkippedVersion = ""
		cache.LastPromptedAt = time.Time{}
		cache.CheckedAt = c.Now()
		_ = SaveCache(c.CachePath, cache)
		fmt.Fprintf(out, "Upgraded mplex to %s (%s)\n", res.LatestVersion, target)
		if hint := MacOSQuarantineHint(c.GOOS, target); hint != "" {
			fmt.Fprintln(out, hint)
		}
		return PromptResult{
			Prompted:   true,
			Upgraded:   true,
			NewVersion: res.LatestVersion,
			TargetPath: target,
		}, nil

	case "s", "skip":
		cache.SkippedVersion = res.LatestVersion
		cache.LastPromptedAt = c.Now()
		_ = SaveCache(c.CachePath, cache)
		fmt.Fprintf(out, "Skipped %s (you can still upgrade anytime with 'mplex update').\n\n", res.LatestVersion)
		return PromptResult{Prompted: true, Skipped: true}, nil

	default:
		cache.LastPromptedAt = c.Now()
		_ = SaveCache(c.CachePath, cache)
		return PromptResult{Prompted: true}, nil
	}
}

// Upgrade downloads the release tarball, verifies its SHA-256 checksum against
// checksums.txt (when present in the release), extracts the mplex binary, and
// atomically replaces the running executable.
func Upgrade(ctx context.Context, cfg Config, rel Release) (string, error) {
	c := cfg.withDefaults()
	if rel.AssetURL == "" {
		return "", fmt.Errorf("no prebuilt release archive found for %s/%s (try: go install github.com/%s/cmd/mplex@latest)", c.GOOS, c.GOARCH, c.Repo)
	}

	targetPath := c.TargetPath
	if targetPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate current executable: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		targetPath = exe
	}

	dlCtx, cancel := context.WithTimeout(ctx, c.DownloadTimeout)
	defer cancel()

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: c.DownloadTimeout}
	}

	// 1. Download checksums.txt if published with the release.
	var expectedHash string
	if rel.ChecksumURL != "" {
		csData, err := downloadBytes(dlCtx, hc, rel.ChecksumURL, c.CurrentVersion, maxChecksumBytes)
		if err != nil {
			return "", fmt.Errorf("download checksums.txt: %w", err)
		}
		hash, ok := lookupChecksum(csData, rel.AssetName)
		if !ok {
			return "", fmt.Errorf("checksums.txt does not contain an entry for %s", rel.AssetName)
		}
		expectedHash = hash
	}

	// 2. Download the release archive.
	archiveData, err := downloadBytes(dlCtx, hc, rel.AssetURL, c.CurrentVersion, maxArchiveBytes)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", rel.AssetName, err)
	}

	// 3. Verify SHA-256 digest when checksums.txt was provided.
	if expectedHash != "" {
		sum := sha256.Sum256(archiveData)
		actualHash := hex.EncodeToString(sum[:])
		if !strings.EqualFold(actualHash, expectedHash) {
			return "", fmt.Errorf("checksum mismatch for %s: expected %s, got %s", rel.AssetName, expectedHash, actualHash)
		}
	}

	// 4. Extract the mplex binary from the tar.gz archive.
	binData, err := extractBinaryFromTarGz(archiveData, "mplex")
	if err != nil {
		return "", fmt.Errorf("extract binary: %w", err)
	}

	// 5. Atomically write and rename over targetPath.
	if err := replaceExecutableAtomically(targetPath, binData); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return "", fmt.Errorf("cannot write to %s: %w (try: sudo mplex update, or go install github.com/%s/cmd/mplex@latest)", targetPath, err, c.Repo)
		}
		return "", err
	}
	postInstallBinary(targetPath)

	return targetPath, nil
}

// MacOSQuarantineHint returns a copy-pasteable command for clearing macOS
// Gatekeeper quarantine on targetPath when running on darwin, or "" otherwise.
func MacOSQuarantineHint(goos, targetPath string) string {
	if goos != "darwin" || targetPath == "" {
		return ""
	}
	return fmt.Sprintf("If macOS Gatekeeper blocks the upgraded binary, allow it with:\n  xattr -d com.apple.quarantine %s", shellQuote(targetPath))
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	needsQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '/' || c == '.' || c == '_' || c == '-' || c == ':' || c == '@' || c == '+') {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func downloadBytes(ctx context.Context, hc *http.Client, url, uaVer string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if uaVer == "" {
		uaVer = "dev"
	}
	req.Header.Set("User-Agent", "mplex/"+uaVer)

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("response exceeded max allowed size (%d bytes)", maxBytes)
	}
	return data, nil
}

func lookupChecksum(checksumsData []byte, assetName string) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(checksumsData))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		name = filepath.Base(name)
		if name == assetName && len(fields[0]) == 64 {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

func extractBinaryFromTarGz(archiveData []byte, binaryName string) ([]byte, error) {
	gzr, err := gzip.NewReader(bytes.NewReader(archiveData))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(hdr.Name) == binaryName {
			data, err := io.ReadAll(io.LimitReader(tr, maxBinaryBytes+1))
			if err != nil {
				return nil, err
			}
			if int64(len(data)) > maxBinaryBytes {
				return nil, fmt.Errorf("extracted binary exceeded max allowed size")
			}
			if len(data) == 0 {
				return nil, errors.New("extracted binary is empty")
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("archive did not contain %q", binaryName)
}

func replaceExecutableAtomically(targetPath string, binData []byte) error {
	mode := os.FileMode(0o755)
	if st, err := os.Stat(targetPath); err == nil {
		mode = st.Mode().Perm() | 0o111
	}

	dir := filepath.Dir(targetPath)
	tmp, err := os.CreateTemp(dir, ".mplex-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(binData); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return err
	}
	tmpPath = ""
	return nil
}
