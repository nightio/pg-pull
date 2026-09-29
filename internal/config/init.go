package config

import (
	"fmt"
	"os"
	"path/filepath"
)

type InitResult struct {
	ConfigPath    string
	IgnorePath    string
	CreatedConfig bool
	CreatedIgnore bool
}

func Init(baseDir string) (InitResult, error) {
	dir, err := filepath.Abs(filepath.Join(baseDir, DirName))
	if err != nil {
		return InitResult{}, fmt.Errorf("resolve config directory: %w", err)
	}
	configPath := filepath.Join(dir, FileName)
	ignorePath := filepath.Join(dir, ".gitignore")
	configExists, err := pathExists(configPath)
	if err != nil {
		return InitResult{}, err
	}
	ignoreExists, err := pathExists(ignorePath)
	if err != nil {
		return InitResult{}, err
	}
	if configExists && ignoreExists {
		return InitResult{}, fmt.Errorf("configuration and .gitignore already exist in %s — refusing to overwrite them", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return InitResult{}, fmt.Errorf("create %s: %w", dir, err)
	}
	result := InitResult{ConfigPath: configPath, IgnorePath: ignorePath}
	// Write ignore rules before the starter config so an interrupted init does not
	// leave a newly generated config visible to Git.
	if !ignoreExists {
		if err := writeNewFile(ignorePath, []byte(IgnoreTemplate), 0o644); err != nil {
			return InitResult{}, err
		}
		result.CreatedIgnore = true
	}
	if !configExists {
		if err := writeNewFile(configPath, []byte(Template), 0o600); err != nil {
			return InitResult{}, err
		}
		result.CreatedConfig = true
	}
	return result, nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("inspect %s: %w", path, err)
}

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

const IgnoreTemplate = `# Keep only the shared config and these ignore rules in Git.
*
!/config.yaml
!/.gitignore
`

const Template = `# pg-pull configuration — lives at .pg-pull/config.yaml.
# pg-pull searches here and in parent directories. Use --config for an explicit file.
# Optional config-local.yaml in this directory overrides individual fields and is ignored by Git.

# Optional local database for restoring a dump.
# target:
#     host: 127.0.0.1
#     port: 5432
#     dbname: CHANGE_ME
#     user: postgres
#     password: CHANGE_ME    # put real local secrets in config-local.yaml
#     schema: public          # optional; defaults to public
#     connect_timeout: 10s    # per connection, including TLS/authentication
#     lock_timeout: 10s       # per lock wait; does not limit COPY duration

# dump_dir: dumps          # optional; relative to .pg-pull
# parallel_exports: 2      # optional; 1-8 simultaneous source tables

# Passwords for source databases are prompted at runtime and never stored.
sources:
    beta:
        host: CHANGE_ME
        port: 5432
        dbname: CHANGE_ME
        user: CHANGE_ME
        schema: public      # optional; defaults to public
        # sslmode: require
        # connect_timeout: 10s
        # lock_timeout: 10s

modules:
    ALL:
        description: "Every table in the configured schema"
        patterns: ['.+']

    # Project:
    #     description: "Project-related tables"
    #     patterns: ['^project_', '^institutions_management_']
`
