package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateInstallsOnlyVerifiedNewerRelease(t *testing.T) {
	if installScheduled {
		t.Skip("Windows replacement finishes after this process exits")
	}
	for _, tc := range []struct {
		name, current string
		check         bool
		badChecksum   bool
		missingAsset  bool
		interrupted   bool
		chunked       bool
		unknownSize   bool
		wantChanged   bool
		wantError     string
	}{
		{name: "install", current: "v1.2.3", wantChanged: true},
		{name: "check only", current: "v1.2.3", check: true},
		{name: "same version", current: "v1.3.0"},
		{name: "older release", current: "v1.4.0"},
		{name: "bad checksum", current: "v1.2.3", badChecksum: true, wantError: "SHA-256"},
		{name: "missing binary", current: "v1.2.3", missingAsset: true, wantError: "missing asset"},
		{name: "interrupted download", current: "v1.2.3", interrupted: true, wantError: "download release binary"},
		{name: "metadata size fallback", current: "v1.2.3", chunked: true, wantChanged: true},
		{name: "unknown download size", current: "v1.2.3", chunked: true, unknownSize: true, wantChanged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldBinary := []byte("old executable")
			newBinary := []byte("new executable")
			path := filepath.Join(t.TempDir(), "pg-pull")
			if err := os.WriteFile(path, oldBinary, 0755); err != nil {
				t.Fatal(err)
			}
			name, _ := assetName("linux", "amd64")
			digest := sha256.Sum256(newBinary)
			// Release sidecars contain the hexadecimal digest.
			checksum := hex.EncodeToString(digest[:]) + "\n"
			if tc.badChecksum {
				checksum = fmt.Sprintf("%064d\n", 0)
			}
			assets := []asset{{ID: 11, Name: name, Size: int64(len(newBinary))}, {ID: 12, Name: name + ".sha256"}}
			if tc.unknownSize {
				assets[0].Size = 0
			}
			if tc.missingAsset {
				assets = assets[1:]
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "" {
					t.Errorf("unexpected authorization header = %q", got)
				}
				if r.URL.Path == repoAPI+"/releases/latest" && r.Header.Get("Accept") != "application/vnd.github+json" {
					t.Errorf("unexpected Accept header = %q", r.Header.Get("Accept"))
				}
				switch r.URL.Path {
				case repoAPI + "/releases/latest":
					_ = json.NewEncoder(w).Encode(release{TagName: "v1.3.0", Assets: assets})
				case repoAPI + "/raw/11":
					if tc.interrupted {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_, _ = fmt.Fprintf(connection, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\nshort", len(newBinary))
						connection.Close()
						return
					}
					if tc.chunked {
						w.(http.Flusher).Flush() // Force chunked transfer without Content-Length.
					}
					_, _ = w.Write(newBinary)
				case repoAPI + "/raw/12":
					_, _ = io.WriteString(w, checksum)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			for i := range assets {
				assets[i].BrowserDownloadURL = fmt.Sprintf("%s%s/raw/%d", server.URL, repoAPI, assets[i].ID)
			}
			manager := &Manager{BaseURL: server.URL, Client: server.Client(), Executable: path, GOOS: "linux", GOARCH: "amd64"}
			var progress [][2]int64
			manager.OnDownloadProgress = func(done, total int64) {
				progress = append(progress, [2]int64{done, total})
			}
			result, err := manager.Run(context.Background(), tc.current, tc.check)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("Run error = %v, want %q", err, tc.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if result.Updated != tc.wantChanged {
				t.Fatalf("updated = %t, want %t", result.Updated, tc.wantChanged)
			}
			shouldDownload := tc.wantChanged || tc.badChecksum || tc.interrupted
			if shouldDownload {
				if len(progress) < 2 || progress[0][0] != 0 {
					t.Fatalf("download progress = %v", progress)
				}
				wantTotal := int64(len(newBinary))
				if tc.unknownSize {
					wantTotal = 0
				}
				for i, sample := range progress {
					if sample[1] != wantTotal || i > 0 && sample[0] < progress[i-1][0] {
						t.Fatalf("download progress = %v", progress)
					}
				}
				if !tc.interrupted && progress[len(progress)-1][0] != int64(len(newBinary)) {
					t.Fatalf("final download progress = %v", progress)
				}
			} else if len(progress) != 0 {
				t.Fatalf("unexpected download progress = %v", progress)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := oldBinary
			if tc.wantChanged {
				want = newBinary
			}
			if string(got) != string(want) {
				t.Fatalf("installed executable = %q, want %q", got, want)
			}
		})
	}
}

func TestFailedInstallPreservesExecutable(t *testing.T) {
	if installScheduled {
		t.Skip("Windows replacement finishes after this process exits")
	}
	path := filepath.Join(t.TempDir(), "pg-pull")
	if err := os.WriteFile(path, []byte("old executable"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := install(path, filepath.Join(t.TempDir(), "missing candidate")); err == nil {
		t.Fatal("install unexpectedly succeeded")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "old executable" {
		t.Fatalf("previous executable = %q, %v", got, err)
	}
}

func TestReleaseVersionAndPlatformSelection(t *testing.T) {
	for _, tc := range []struct {
		current, latest string
		newer           bool
	}{
		{"v1.2.9", "v1.3.0", true},
		{"v1.3.0-rc.2", "v1.3.0", true},
		{"v1.3.0", "v1.3.0", false},
		{"v2.0.0", "v1.99.99", false},
		{"v1.3.0-rc.10", "v1.3.0-rc.2", false},
	} {
		current, err := parseVersion(tc.current)
		if err != nil {
			t.Fatal(err)
		}
		latest, err := parseVersion(tc.latest)
		if err != nil {
			t.Fatal(err)
		}
		if got := compareVersion(latest, current) > 0; got != tc.newer {
			t.Fatalf("%s newer than %s = %t", tc.latest, tc.current, got)
		}
	}
	for _, bad := range []string{"dev", "v1.2", "v01.2.3", "v1.2.3-", "v1.2.3-rc..1"} {
		if _, err := parseVersion(bad); err == nil {
			t.Fatalf("accepted invalid version %q", bad)
		}
	}
	for _, tc := range []struct{ os, arch, name string }{
		{"linux", "amd64", "pg-pull-linux-x86_64"},
		{"linux", "arm64", "pg-pull-linux-aarch64"},
		{"darwin", "amd64", "pg-pull-macos-x86_64"},
		{"darwin", "arm64", "pg-pull-macos-aarch64"},
		{"windows", "amd64", "pg-pull-windows-x86_64.exe"},
		{"windows", "arm64", "pg-pull-windows-aarch64.exe"},
	} {
		name, err := assetName(tc.os, tc.arch)
		if err != nil || name != tc.name {
			t.Fatalf("assetName(%s, %s) = %q, %v", tc.os, tc.arch, name, err)
		}
	}
}

func TestPublicAssetRedirect(t *testing.T) {
	var requested bool
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
		_, _ = w.Write([]byte("asset"))
	}))
	defer storage.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storage.URL+"/asset", http.StatusFound)
	}))
	defer server.Close()
	manager := &Manager{BaseURL: server.URL, Client: server.Client()}
	data, err := manager.getBytes(context.Background(), "/redirect", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !requested || string(data) != "asset" {
		t.Fatalf("redirect response = %q, requested = %t", data, requested)
	}
}

func TestPublicReleaseHTTPError(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusNotFound, "release or asset not found"},
		{http.StatusForbidden, "limited this request"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			manager := &Manager{BaseURL: server.URL, Client: server.Client(), GOOS: "linux", GOARCH: "amd64"}
			_, err := manager.Run(context.Background(), "v1.0.0", true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run error = %v, want %q", err, tc.want)
			}
		})
	}
}
