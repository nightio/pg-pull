//go:build mage

package main

import (
	"debug/buildinfo"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type platform struct {
	name, goos, goarch string
}

var releasePlatforms = []platform{
	{"pg-pull-linux-x86_64", "linux", "amd64"},
	{"pg-pull-linux-aarch64", "linux", "arm64"},
	{"pg-pull-macos-x86_64", "darwin", "amd64"},
	{"pg-pull-macos-aarch64", "darwin", "arm64"},
	{"pg-pull-windows-x86_64.exe", "windows", "amd64"},
	{"pg-pull-windows-aarch64.exe", "windows", "arm64"},
}

// Test runs the unit tests. Integration tests are skipped unless explicitly enabled.
func Test() error { return runGo(nil, "test", "./...") }

// Vet checks all Go packages.
func Vet() error { return runGo(nil, "vet", "./...") }

// Integration runs PostgreSQL tests using Testcontainers unless DSNs are supplied.
func Integration() error {
	return runGo([]string{"PG_PULL_INTEGRATION=1"}, "test", "./internal/app", "./internal/database", "-v", "-count=1")
}

// Build compiles a native executable for the current platform.
func Build() error {
	name := "pg-pull"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return build(name, runtime.GOOS, runtime.GOARCH)
}

// Dist cross-compiles all six release executables into a clean dist directory.
func Dist() error {
	if err := os.RemoveAll("dist"); err != nil {
		return err
	}
	if err := os.MkdirAll("dist", 0o755); err != nil {
		return err
	}
	for _, target := range releasePlatforms {
		if err := build(filepath.Join("dist", target.name), target.goos, target.goarch); err != nil {
			return fmt.Errorf("build %s: %w", target.name, err)
		}
	}
	return Verify()
}

// Verify checks that dist contains exactly the expected Go binaries and platforms.
func Verify() error {
	entries, err := os.ReadDir("dist")
	if err != nil {
		return err
	}
	if len(entries) != len(releasePlatforms) {
		return fmt.Errorf("dist contains %d entries, want %d", len(entries), len(releasePlatforms))
	}
	for _, target := range releasePlatforms {
		path := filepath.Join("dist", target.name)
		info, err := buildinfo.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		settings := make(map[string]string, len(info.Settings))
		for _, setting := range info.Settings {
			settings[setting.Key] = setting.Value
		}
		if settings["GOOS"] != target.goos || settings["GOARCH"] != target.goarch || settings["CGO_ENABLED"] != "0" {
			return fmt.Errorf("%s has GOOS=%s GOARCH=%s CGO_ENABLED=%s", path,
				settings["GOOS"], settings["GOARCH"], settings["CGO_ENABLED"])
		}
	}
	fmt.Println("Verified all six release binaries")
	return nil
}

func build(output, goos, goarch string) error {
	flags := fmt.Sprintf("-s -w -X main.version=%s -X main.commit=%s -X main.builtAt=%s",
		metadata("VERSION", "dev"), metadata("COMMIT", "unknown"), metadata("BUILD_DATE", "unknown"))
	fmt.Printf("Building %s (%s/%s)\n", output, goos, goarch)
	return runGo([]string{"CGO_ENABLED=0", "GOOS=" + goos, "GOARCH=" + goarch},
		"build", "-trimpath", "-ldflags", flags, "-o", output, "./cmd/pg-pull")
}

func metadata(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func runGo(overrides []string, args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), overrides...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
