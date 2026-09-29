package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadTimeouts(t *testing.T) {
	for _, value := range []string{"10s", "250ms", "1m", "0", "0s", "-1s", "1us", "1.5ms", "2147483648ms", "forever"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			data := "sources: {beta: {host: localhost, port: 5432, dbname: app, user: reader, connect_timeout: '" + value + "', lock_timeout: '" + value + "'}}\nmodules: {ALL: {patterns: ['.+']}}\n"
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			valid := value == "10s" || value == "250ms" || value == "1m"
			if valid && err != nil || !valid && err == nil {
				t.Fatalf("Load: %v", err)
			}
			if !valid && !strings.Contains(err.Error(), "sources.beta.connect_timeout") {
				t.Fatal(err)
			}
		})
	}
}

func TestTimeoutDefaultsAndLocalOverrides(t *testing.T) {
	connect, lock, err := (Database{}).Timeouts()
	if err != nil || connect != 10*time.Second || lock != 10*time.Second {
		t.Fatalf("defaults: %v %v %v", connect, lock, err)
	}
	if _, _, err := (Database{LockTimeout: "-1s"}).Timeouts(); err == nil {
		t.Fatal("invalid lock timeout accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := "target: {host: localhost, port: 5432, dbname: local, user: app, connect_timeout: 2s, lock_timeout: 3s}\nsources: {beta: {host: remote, port: 5432, dbname: app, user: reader}}\nmodules: {ALL: {patterns: ['.+']}}\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, LocalFileName), []byte("target: {lock_timeout: 500ms}\nsources: {beta: {connect_timeout: 4s}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	connect, lock, err = cfg.Target.Timeouts()
	if err != nil || connect != 2*time.Second || lock != 500*time.Millisecond {
		t.Fatalf("target overrides: %v %v %v", connect, lock, err)
	}
	connect, lock, err = cfg.Sources["beta"].Timeouts()
	if err != nil || connect != 4*time.Second || lock != 10*time.Second {
		t.Fatalf("source overrides: %v %v %v", connect, lock, err)
	}
}
