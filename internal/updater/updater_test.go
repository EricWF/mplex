package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVersionParsingAndComparison(t *testing.T) {
	if !IsReleaseVersion("v0.1.0") {
		t.Fatalf("expected v0.1.0 to be a release version")
	}
	if !IsReleaseVersion("1.2.3") {
		t.Fatalf("expected 1.2.3 to be a release version")
	}
	for _, devVer := range []string{"dev", "(devel)", "", "v0.1.0-dirty", "v0.1.0-3-g6ad1397", "v0.1.0-3-g6ad1397-dirty", "6ad1397"} {
		if IsReleaseVersion(devVer) {
			t.Fatalf("expected %q NOT to be treated as a release version", devVer)
		}
	}

	cases := []struct {
		cur, lat string
		newer    bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.2.0", "v0.10.0", true},
		{"v0.1.0", "v0.1.1", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.3.0", "v0.2.0", false},
		{"v1.0.0-rc.1", "v1.0.0", true},
		{"v1.0.0", "v1.0.0-rc.1", false},
		{"dev", "v0.2.0", false},
		{"v0.1.0-3-g6ad1397", "v0.1.0", false},
	}
	for _, tc := range cases {
		if got := IsNewer(tc.cur, tc.lat); got != tc.newer {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.cur, tc.lat, got, tc.newer)
		}
	}
}

func makeTestTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o755,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func TestCheckCachePromptAndAtomicUpgrade(t *testing.T) {
	newBinContent := []byte("#!/bin/sh\necho upgraded-v0.2.0\n")
	assetName := ExpectedAssetName("linux", "amd64")
	tarball := makeTestTarGz(t, "mplex", newBinContent)
	sum := sha256.Sum256(tarball)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), assetName)

	var apiHits atomic.Int32
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/EricWF/mplex/releases/latest":
			apiHits.Add(1)
			_ = json.NewEncoder(w).Encode(githubRelease{
				TagName: "v0.2.0",
				HTMLURL: srvURL + "/releases/tag/v0.2.0",
				Assets: []githubAsset{
					{Name: assetName, BrowserDownloadURL: srvURL + "/dl/" + assetName},
					{Name: "checksums.txt", BrowserDownloadURL: srvURL + "/dl/checksums.txt"},
				},
			})
		case "/dl/" + assetName:
			_, _ = w.Write(tarball)
		case "/dl/checksums.txt":
			_, _ = w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	tmp := t.TempDir()
	cachePath := filepath.Join(tmp, "update-check.json")
	targetBin := filepath.Join(tmp, "mplex")
	if err := os.WriteFile(targetBin, []byte("old-v0.1.0"), 0o755); err != nil {
		t.Fatalf("write old bin: %v", err)
	}

	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	cfg := Config{
		CurrentVersion: "v0.1.0",
		APIBaseURL:     srv.URL,
		CachePath:      cachePath,
		TargetPath:     targetBin,
		GOOS:           "linux",
		GOARCH:         "amd64",
		HTTPClient:     srv.Client(),
		Now:            func() time.Time { return now },
	}

	// 1. User declines ("n") on first prompt -> hits API once, snoozes for 24h.
	var out bytes.Buffer
	pr, err := PromptIfAvailable(context.Background(), cfg, strings.NewReader("n\n"), &out)
	if err != nil {
		t.Fatalf("PromptIfAvailable (n): %v", err)
	}
	if !pr.Prompted || pr.Upgraded {
		t.Fatalf("expected Prompted=true, Upgraded=false; got %+v", pr)
	}
	if !strings.Contains(out.String(), "v0.2.0 is available") {
		t.Fatalf("expected version prompt in output, got: %s", out.String())
	}
	if apiHits.Load() != 1 {
		t.Fatalf("expected 1 API hit, got %d", apiHits.Load())
	}

	// 2. Immediate re-check within 24h -> served from cache AND snoozed (no prompt, no API hit).
	out.Reset()
	pr, err = PromptIfAvailable(context.Background(), cfg, strings.NewReader("y\n"), &out)
	if err != nil {
		t.Fatalf("PromptIfAvailable (snoozed): %v", err)
	}
	if pr.Prompted || pr.Upgraded {
		t.Fatalf("expected snoozed run not to prompt; got %+v", pr)
	}
	if apiHits.Load() != 1 {
		t.Fatalf("expected cache hit with still 1 API hit, got %d", apiHits.Load())
	}

	// 3. Advance clock 25h and answer "y" -> prompts again, downloads, verifies SHA-256, replaces binary!
	now = now.Add(25 * time.Hour)
	out.Reset()
	pr, err = PromptIfAvailable(context.Background(), cfg, strings.NewReader("y\n"), &out)
	if err != nil {
		t.Fatalf("PromptIfAvailable (y): %v", err)
	}
	if !pr.Prompted || !pr.Upgraded || pr.NewVersion != "v0.2.0" {
		t.Fatalf("expected successful upgrade, got %+v (out=%s)", pr, out.String())
	}
	gotBin, err := os.ReadFile(targetBin)
	if err != nil {
		t.Fatalf("read upgraded binary: %v", err)
	}
	if !bytes.Equal(gotBin, newBinContent) {
		t.Fatalf("upgraded binary mismatch: got %q, want %q", gotBin, newBinContent)
	}
}

func TestPromptSkipVersionSuppressesUntilNewerRelease(t *testing.T) {
	assetName := ExpectedAssetName("darwin", "arm64")
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(githubRelease{
			TagName: "v0.2.0",
			HTMLURL: srvURL + "/releases/tag/v0.2.0",
			Assets: []githubAsset{
				{Name: assetName, BrowserDownloadURL: srvURL + "/dl/" + assetName},
			},
		})
	}))
	defer srv.Close()
	srvURL = srv.URL

	tmp := t.TempDir()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	cfg := Config{
		CurrentVersion: "v0.1.0",
		APIBaseURL:     srv.URL,
		CachePath:      filepath.Join(tmp, "update-check.json"),
		GOOS:           "darwin",
		GOARCH:         "arm64",
		HTTPClient:     srv.Client(),
		Now:            func() time.Time { return now },
	}

	var out bytes.Buffer
	pr, err := PromptIfAvailable(context.Background(), cfg, strings.NewReader("s\n"), &out)
	if err != nil {
		t.Fatalf("PromptIfAvailable (s): %v", err)
	}
	if !pr.Prompted || !pr.Skipped {
		t.Fatalf("expected Skipped=true, got %+v", pr)
	}

	// Even 48 hours later, v0.2.0 remains skipped!
	now = now.Add(48 * time.Hour)
	out.Reset()
	pr, err = PromptIfAvailable(context.Background(), cfg, strings.NewReader("y\n"), &out)
	if err != nil {
		t.Fatalf("PromptIfAvailable after skip: %v", err)
	}
	if pr.Prompted || pr.Upgraded {
		t.Fatalf("expected skipped version not to prompt again, got %+v", pr)
	}
}

func TestUpgradeRejectsBadChecksum(t *testing.T) {
	assetName := ExpectedAssetName("linux", "amd64")
	tarball := makeTestTarGz(t, "mplex", []byte("legit-binary"))
	badChecksums := fmt.Sprintf("%s  %s\n", strings.Repeat("0", 64), assetName)

	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/" + assetName:
			_, _ = w.Write(tarball)
		case "/dl/checksums.txt":
			_, _ = w.Write([]byte(badChecksums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	tmp := t.TempDir()
	targetBin := filepath.Join(tmp, "mplex")
	if err := os.WriteFile(targetBin, []byte("original"), 0o755); err != nil {
		t.Fatalf("write original: %v", err)
	}

	_, err := Upgrade(context.Background(), Config{
		CurrentVersion: "v0.1.0",
		TargetPath:     targetBin,
		GOOS:           "linux",
		GOARCH:         "amd64",
		HTTPClient:     srv.Client(),
	}, Release{
		Version:     "v0.2.0",
		AssetName:   assetName,
		AssetURL:    srvURL + "/dl/" + assetName,
		ChecksumURL: srvURL + "/dl/checksums.txt",
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got: %v", err)
	}

	// Original binary must be untouched.
	got, _ := os.ReadFile(targetBin)
	if string(got) != "original" {
		t.Fatalf("original binary was modified on checksum failure: %q", got)
	}
}
