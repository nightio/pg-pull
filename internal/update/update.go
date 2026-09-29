package update

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.github.com"
	repoAPI        = "/repos/nightio/pg-pull"
	releasesPage   = "https://github.com/nightio/pg-pull/releases"
	maxMetadata    = 1 << 20
	maxChecksum    = 4096
	maxBinary      = 200 << 20
)

// Manager allows tests to use a local release server and an isolated executable.
type Manager struct {
	BaseURL            string
	Client             *http.Client
	OnDownloadProgress func(downloaded, total int64)
	Executable         string
	GOOS               string
	GOARCH             string
}

type Result struct {
	Current   string
	Latest    string
	Available bool
	Updated   bool
	Scheduled bool
}

type asset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}

func New() *Manager {
	return &Manager{
		BaseURL: defaultBaseURL,
		Client:  &http.Client{Timeout: 5 * time.Minute},
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
	}
}

func (m *Manager) Run(ctx context.Context, current string, checkOnly bool) (Result, error) {
	result := Result{Current: current}
	currentVersion, err := parseVersion(current)
	if err != nil {
		return result, fmt.Errorf("self-update requires a versioned release binary (current version %q); download one from %s", current, releasesPage)
	}
	name, err := assetName(m.GOOS, m.GOARCH)
	if err != nil {
		return result, err
	}
	var latest release
	if err := m.getJSON(ctx, repoAPI+"/releases/latest", &latest); err != nil {
		return result, fmt.Errorf("check latest release: %w", err)
	}
	latestVersion, err := parseVersion(latest.TagName)
	if err != nil || len(latestVersion.pre) != 0 {
		return result, fmt.Errorf("latest release has an invalid stable version tag %q", latest.TagName)
	}
	result.Latest = latest.TagName
	result.Available = compareVersion(latestVersion, currentVersion) > 0
	if !result.Available || checkOnly {
		return result, nil
	}
	binary, err := findAsset(latest.Assets, name)
	if err != nil {
		return result, err
	}
	checksum, err := findAsset(latest.Assets, name+".sha256")
	if err != nil {
		return result, err
	}
	if binary.Size > maxBinary {
		return result, fmt.Errorf("release binary exceeds the 200 MiB size limit")
	}
	checksumData, err := m.getAssetBytes(ctx, checksum, maxChecksum)
	if err != nil {
		return result, fmt.Errorf("download checksum: %w", err)
	}
	want, err := parseChecksum(checksumData, name)
	if err != nil {
		return result, fmt.Errorf("invalid release checksum: %w", err)
	}
	path, err := m.executable()
	if err != nil {
		return result, err
	}
	stage, err := os.CreateTemp(filepath.Dir(path), ".pg-pull-update-*")
	if err != nil {
		return result, fmt.Errorf("create update beside %s: %w", path, err)
	}
	stagePath := stage.Name()
	keepStage := false
	defer func() {
		if !keepStage {
			os.Remove(stagePath)
		}
	}()
	if err := m.download(ctx, binary, stage, want); err != nil {
		stage.Close()
		return result, err
	}
	info, err := os.Stat(path)
	if err != nil {
		stage.Close()
		return result, fmt.Errorf("inspect installed executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		stage.Close()
		return result, fmt.Errorf("installed executable is not a regular file: %s", path)
	}
	if err := stage.Chmod(info.Mode().Perm()); err != nil {
		stage.Close()
		return result, fmt.Errorf("set executable permissions: %w", err)
	}
	if err := stage.Sync(); err != nil {
		stage.Close()
		return result, fmt.Errorf("sync downloaded executable: %w", err)
	}
	if err := stage.Close(); err != nil {
		return result, fmt.Errorf("close downloaded executable: %w", err)
	}
	if err := install(path, stagePath); err != nil {
		return result, err
	}
	result.Updated = true
	result.Scheduled = installScheduled
	keepStage = installScheduled
	return result, nil
}

func (m *Manager) endpoint(path string) (*url.URL, error) {
	base := m.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("invalid release server URL")
	}
	return u.Parse(path)
}

func (m *Manager) client() *http.Client {
	base := m.Client
	if base == nil {
		base = &http.Client{Timeout: 5 * time.Minute}
	}
	copy := *base
	copy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many release download redirects")
		}
		if len(via) != 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("release download redirected to an insecure URL")
		}
		if base.CheckRedirect != nil {
			if err := base.CheckRedirect(req, via); err != nil {
				return err
			}
		}
		return nil
	}
	return &copy
}

func (m *Manager) get(ctx context.Context, path string) (*http.Response, error) {
	u, err := m.endpoint(path)
	if err != nil {
		return nil, err
	}
	return m.getURL(ctx, u)
}

func (m *Manager) getAsset(ctx context.Context, item asset) (*http.Response, error) {
	u, err := url.Parse(item.BrowserDownloadURL)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("invalid download URL for asset %s", item.Name)
	}
	base, err := m.endpoint(repoAPI)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && !(base.Scheme == "http" && u.Scheme == "http" && u.Host == base.Host) {
		return nil, fmt.Errorf("insecure download URL for asset %s", item.Name)
	}
	return m.getURL(ctx, u)
}

func (m *Manager) getURL(ctx context.Context, u *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := m.client().Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			// Asset URLs can contain signed query parameters. Do not include
			// those URLs in user-facing network errors.
			return nil, fmt.Errorf("request release server: %w", urlErr.Err)
		}
		return nil, fmt.Errorf("request release server: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusForbidden, http.StatusTooManyRequests:
			return nil, errors.New("GitHub limited this request; try again later")
		case http.StatusNotFound:
			return nil, errors.New("GitHub release or asset not found; check that a release with binaries has been published")
		default:
			return nil, fmt.Errorf("GitHub returned HTTP %d", resp.StatusCode)
		}
	}
	return resp, nil
}

func (m *Manager) getBytes(ctx context.Context, path string, limit int64) ([]byte, error) {
	resp, err := m.get(ctx, path)
	if err != nil {
		return nil, err
	}
	return readBytes(resp, limit)
}

func (m *Manager) getAssetBytes(ctx context.Context, item asset, limit int64) ([]byte, error) {
	resp, err := m.getAsset(ctx, item)
	if err != nil {
		return nil, err
	}
	return readBytes(resp, limit)
}

func readBytes(resp *http.Response, limit int64) ([]byte, error) {
	defer resp.Body.Close()
	if resp.ContentLength > limit {
		return nil, errors.New("release response exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("release response exceeds size limit")
	}
	return data, nil
}

func (m *Manager) getJSON(ctx context.Context, path string, target any) error {
	data, err := m.getBytes(ctx, path, maxMetadata)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode release metadata: %w", err)
	}
	return nil
}

func (m *Manager) download(ctx context.Context, item asset, out *os.File, want []byte) error {
	resp, err := m.getAsset(ctx, item)
	if err != nil {
		return fmt.Errorf("download release binary: %w", err)
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxBinary {
		return errors.New("release binary exceeds the 200 MiB size limit")
	}
	total := resp.ContentLength
	if total <= 0 {
		total = item.Size
	}
	if total < 0 || total > maxBinary {
		total = 0
	}
	if m.OnDownloadProgress != nil {
		m.OnDownloadProgress(0, total)
	}
	hash := sha256.New()
	writer := &downloadProgressWriter{Writer: io.MultiWriter(out, hash), total: total, report: m.OnDownloadProgress}
	n, err := io.Copy(writer, io.LimitReader(resp.Body, maxBinary+1))
	if err != nil {
		return fmt.Errorf("download release binary: %w", err)
	}
	if n > maxBinary {
		return errors.New("release binary exceeds the 200 MiB size limit")
	}
	if subtle.ConstantTimeCompare(hash.Sum(nil), want) != 1 {
		return errors.New("release binary SHA-256 does not match its checksum")
	}
	return nil
}

type downloadProgressWriter struct {
	io.Writer
	total      int64
	downloaded int64
	report     func(downloaded, total int64)
}

func (w *downloadProgressWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	w.downloaded += int64(n)
	if n > 0 && w.report != nil {
		w.report(w.downloaded, w.total)
	}
	return n, err
}

func (m *Manager) executable() (string, error) {
	path := m.Executable
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("find installed executable: %w", err)
		}
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve installed executable: %w", err)
	}
	return filepath.Abs(path)
}

func findAsset(assets []asset, name string) (asset, error) {
	var found asset
	for _, candidate := range assets {
		if candidate.Name != name {
			continue
		}
		if found.ID != 0 || candidate.ID <= 0 {
			return asset{}, fmt.Errorf("release has duplicate or invalid asset %s", name)
		}
		found = candidate
	}
	if found.ID == 0 {
		return asset{}, fmt.Errorf("release is missing asset %s; check that the release upload finished", name)
	}
	return found, nil
}

func parseChecksum(data []byte, name string) ([]byte, error) {
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) == 2 {
		if strings.TrimPrefix(fields[1], "*") != name {
			return nil, errors.New("expected a SHA-256 sum for " + name)
		}
	} else if len(fields) != 1 {
		return nil, errors.New("expected one SHA-256 sum for " + name)
	}
	value, err := hex.DecodeString(fields[0])
	if err != nil || len(value) != sha256.Size {
		return nil, errors.New("expected a 64-digit SHA-256 sum")
	}
	return value, nil
}

func assetName(goos, goarch string) (string, error) {
	system := map[string]string{"linux": "linux", "darwin": "macos", "windows": "windows"}[goos]
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[goarch]
	if system == "" || arch == "" {
		return "", fmt.Errorf("self-update is unavailable for %s/%s", goos, goarch)
	}
	name := "pg-pull-" + system + "-" + arch
	if goos == "windows" {
		name += ".exe"
	}
	return name, nil
}
