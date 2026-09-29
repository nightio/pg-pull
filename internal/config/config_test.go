package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLoadPreservesOrderAndDefaultsSchemas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `target:
  host: localhost
  port: 5432
  dbname: local
  user: app
sources:
  staging:
    host: remote
    port: 5432
    dbname: app
    user: reader
  production:
    host: prod
    port: 5432
    dbname: app
    user: reader
modules:
  Project:
    patterns: ['^project_']
  ALL:
    patterns: ['.+']
dump_dir: dumps
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Target.Schema != "public" || cfg.Sources["staging"].Schema != "public" {
		t.Fatalf("schemas were not defaulted: %#v", cfg)
	}
	if got := cfg.SourceOrder; len(got) != 2 || got[0] != "staging" || got[1] != "production" {
		t.Fatalf("source order = %v", got)
	}
	if got := cfg.ModuleOrder; len(got) != 2 || got[0] != "Project" || got[1] != "ALL" {
		t.Fatalf("module order = %v", got)
	}
	wantDump := filepath.Join(dir, "dumps")
	if cfg.DumpBaseDir != wantDump {
		t.Fatalf("dump dir = %q, want %q", cfg.DumpBaseDir, wantDump)
	}
	if cfg.ParallelExports != DefaultParallelExports {
		t.Fatalf("parallel exports = %d, want %d", cfg.ParallelExports, DefaultParallelExports)
	}
}

func TestLoadParallelExportsRange(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"1", true}, {"8", true}, {"0", false}, {"9", false}, {"-1", false},
	} {
		t.Run(test.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			data := `target: {host: localhost, port: 5432, dbname: local, user: app}
sources: {staging: {host: remote, port: 5432, dbname: app, user: reader}}
modules: {ALL: {patterns: ['.+']}}
parallel_exports: ` + test.value + "\n"
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if test.valid {
				if err != nil || cfg.ParallelExports != mustInt(t, test.value) {
					t.Fatalf("Load = %#v, %v", cfg, err)
				}
			} else if err == nil {
				t.Fatal("expected invalid parallel_exports to fail")
			}
		})
	}
}

func TestLoadWithoutTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `sources: {staging: {host: remote, port: 5432, dbname: app, user: reader}}
modules: {ALL: {patterns: ['.+']}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Target != nil {
		t.Fatalf("unexpected target: %#v", cfg.Target)
	}
}

func mustInt(t *testing.T, value string) int {
	t.Helper()
	result, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestResolveNotFound(t *testing.T) {
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	defer os.Chdir(old) //nolint:errcheck
	_, err = Resolve("")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestInitDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	result, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CreatedConfig || !result.CreatedIgnore {
		t.Fatalf("incomplete init: %#v", result)
	}
	if _, err := os.Stat(result.ConfigPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result.IgnorePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != IgnoreTemplate {
		t.Fatalf("ignore file = %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, DirName, LocalFileName)); !os.IsNotExist(err) {
		t.Fatalf("init unexpectedly created local config: %v", err)
	}
	if _, err := Init(dir); err == nil {
		t.Fatal("expected second Init to fail")
	}
}

func TestInitFillsMissingIgnoreWithoutChangingConfig(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, FileName)
	if err := os.WriteFile(path, []byte("existing config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.CreatedConfig || !result.CreatedIgnore {
		t.Fatalf("unexpected init result: %#v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing config\n" {
		t.Fatalf("config overwritten: %q", data)
	}
}

func TestInitPreservesExistingIgnore(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ignorePath := filepath.Join(configDir, ".gitignore")
	if err := os.WriteFile(ignorePath, []byte("custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CreatedConfig || result.CreatedIgnore {
		t.Fatalf("unexpected init result: %#v", result)
	}
	data, err := os.ReadFile(ignorePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "custom\n" {
		t.Fatalf("ignore file overwritten: %q", data)
	}
}

func TestGeneratedIgnoreRules(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	init := exec.Command(git, "init", "-q", dir)
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", output, err)
	}
	if _, err := Init(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		filepath.Join(DirName, "2026-09-28_10-11-12"),
		filepath.Join(DirName, "dumps", "2026-09-28_10-11-12-1"),
		filepath.Join(DirName, "custom-cache"),
	} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{filepath.Join(DirName, LocalFileName), filepath.Join(DirName, "notes.txt")} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("local\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		path    string
		ignored bool
	}{
		{filepath.Join(DirName, LocalFileName), true},
		{filepath.Join(DirName, "2026-09-28_10-11-12"), true},
		{filepath.Join(DirName, "dumps", "2026-09-28_10-11-12-1"), true},
		{filepath.Join(DirName, "custom-cache"), true},
		{filepath.Join(DirName, "notes.txt"), true},
		{filepath.Join(DirName, FileName), false},
		{filepath.Join(DirName, ".gitignore"), false},
	} {
		cmd := exec.Command(git, "-C", dir, "-c", "core.excludesFile=/dev/null", "check-ignore", "-q", "--", test.path)
		err := cmd.Run()
		if (err == nil) != test.ignored {
			t.Fatalf("ignore status for %s: %v, want ignored=%t", test.path, err, test.ignored)
		}
	}
}

func TestLocalConfigMergesFieldsAndPreservesOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	base := `target: {host: localhost, port: 5432, dbname: local, user: app}
sources:
  alpha: {host: alpha, port: 5432, dbname: app, user: reader}
  beta: {host: beta, port: 5432, dbname: app, user: reader}
modules:
  First: {description: original, patterns: ['^first_']}
  ALL: {patterns: ['.+']}
parallel_exports: 2
dump_dir: dumps
`
	local := `target: {password: secret, port: 5544}
sources:
  beta: {host: new-beta, password: ignored}
  gamma: {host: gamma, port: 5432, dbname: app, user: reader}
modules:
  First: {description: changed}
  ALL: {patterns: ['^selected_']}
  Third: {patterns: ['^third_']}
parallel_exports: 3
dump_dir: local-dumps
`
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, LocalFileName), []byte(local), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Target.Host != "localhost" || cfg.Target.Port != 5544 || cfg.Target.Password != "secret" {
		t.Fatalf("target = %#v", cfg.Target)
	}
	if cfg.Sources["beta"].Host != "new-beta" || cfg.Sources["beta"].User != "reader" || cfg.Sources["beta"].Password != "" {
		t.Fatalf("beta = %#v", cfg.Sources["beta"])
	}
	if got := strings.Join(cfg.SourceOrder, ","); got != "alpha,beta,gamma" {
		t.Fatalf("source order = %s", got)
	}
	if got := strings.Join(cfg.ModuleOrder, ","); got != "First,ALL,Third" {
		t.Fatalf("module order = %s", got)
	}
	if cfg.Modules["First"].Description != "changed" || cfg.Modules["First"].Patterns[0] != "^first_" || cfg.Modules["ALL"].Patterns[0] != "^selected_" {
		t.Fatalf("modules = %#v", cfg.Modules)
	}
	if cfg.ParallelExports != 3 || cfg.DumpBaseDir != filepath.Join(dir, "local-dumps") {
		t.Fatalf("settings = %#v", cfg)
	}
}

func TestInvalidLocalConfigReportsItsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	data := "sources: {beta: {host: beta, port: 5432, dbname: app, user: reader}}\nmodules: {ALL: {patterns: ['.+']}}\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(dir, LocalFileName)
	if err := os.WriteFile(localPath, []byte("target: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), localPath) {
		t.Fatalf("expected local file error, got %v", err)
	}
}

func TestDiscoveredConfigUsesAdjacentLocalFile(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := "sources: {beta: {host: remote, port: 5432, dbname: app, user: reader}}\nmodules: {ALL: {patterns: ['.+']}}\n"
	if err := os.WriteFile(filepath.Join(configDir, FileName), []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, LocalFileName), []byte("sources: {beta: {host: local-override}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(child)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources["beta"].Host != "local-override" || cfg.Path != filepath.Join(configDir, FileName) {
		t.Fatalf("discovered config = %#v", cfg)
	}
}
