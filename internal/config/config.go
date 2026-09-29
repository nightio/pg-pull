package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	DirName                = ".pg-pull"
	FileName               = "config.yaml"
	LocalFileName          = "config-local.yaml"
	DefaultSchema          = "public"
	DefaultParallelExports = 2
	MaxParallelExports     = 8
)

var ErrNotFound = errors.New("configuration not found")

type Database struct {
	Host           string `yaml:"host"`
	Port           uint16 `yaml:"port"`
	DBName         string `yaml:"dbname"`
	User           string `yaml:"user"`
	Password       string `yaml:"password,omitempty"`
	Schema         string `yaml:"schema,omitempty"`
	SSLMode        string `yaml:"sslmode,omitempty"`
	ConnectTimeout string `yaml:"connect_timeout,omitempty"`
	LockTimeout    string `yaml:"lock_timeout,omitempty"`
}

// Timeouts also validates programmatically constructed configurations.
func (db Database) Timeouts() (connect, lock time.Duration, err error) {
	parse := func(name, value string) (time.Duration, error) {
		if value == "" {
			return 10 * time.Second, nil
		}
		d, err := time.ParseDuration(value)
		// PostgreSQL lock_timeout is an integer number of milliseconds.
		if err != nil || d < time.Millisecond || d > 2147483647*time.Millisecond || d%time.Millisecond != 0 {
			return 0, fmt.Errorf("%s must be a duration from 1ms to 2147483647ms in whole milliseconds (e.g. 10s or 1m)", name)
		}
		return d, nil
	}
	connect, err = parse("connect_timeout", db.ConnectTimeout)
	if err != nil {
		return
	}
	lock, err = parse("lock_timeout", db.LockTimeout)
	return
}

type Module struct {
	Description string   `yaml:"description,omitempty"`
	Patterns    []string `yaml:"patterns"`
}

type Config struct {
	Target          *Database
	Sources         map[string]Database
	SourceOrder     []string
	Modules         map[string]Module
	ModuleOrder     []string
	DumpBaseDir     string
	ParallelExports int
	Path            string
}

type rawConfig struct {
	Target          *Database           `yaml:"target"`
	Sources         map[string]Database `yaml:"sources"`
	Modules         map[string]Module   `yaml:"modules"`
	DumpDir         string              `yaml:"dump_dir,omitempty"`
	ParallelExports *int                `yaml:"parallel_exports,omitempty"`
}

func Load(explicitPath string) (*Config, error) {
	path, err := Resolve(explicitPath)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	if err := requireYAMLMapping(&node, path); err != nil {
		return nil, err
	}
	configDescription := path
	localPath := filepath.Join(filepath.Dir(path), LocalFileName)
	localData, err := os.ReadFile(localPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", localPath, err)
	}
	if err == nil {
		var localNode yaml.Node
		if err := yaml.Unmarshal(localData, &localNode); err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", localPath, err)
		}
		if err := requireYAMLMapping(&localNode, localPath); err != nil {
			return nil, err
		}
		mergeYAML(&node, &localNode)
		configDescription += " with " + localPath
	}
	var raw rawConfig
	if err := node.Decode(&raw); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", configDescription, err)
	}

	if raw.Target != nil {
		if err := validateDatabase(raw.Target, "target", true); err != nil {
			return nil, fmt.Errorf("%s: %w", configDescription, err)
		}
	}
	if len(raw.Sources) == 0 {
		return nil, fmt.Errorf("%s must define at least one entry under \"sources\"", configDescription)
	}
	for name, source := range raw.Sources {
		if err := validateDatabase(&source, "sources."+name, false); err != nil {
			return nil, fmt.Errorf("%s: %w", configDescription, err)
		}
		raw.Sources[name] = source
	}
	if len(raw.Modules) == 0 {
		return nil, fmt.Errorf("%s must define at least one entry under \"modules\"", configDescription)
	}
	parallelExports := DefaultParallelExports
	if raw.ParallelExports != nil {
		parallelExports = *raw.ParallelExports
	}
	if parallelExports < 1 || parallelExports > MaxParallelExports {
		return nil, fmt.Errorf("%s: parallel_exports must be between 1 and %d", configDescription, MaxParallelExports)
	}
	for name, module := range raw.Modules {
		if len(module.Patterns) == 0 {
			return nil, fmt.Errorf("module %q in %s must define a non-empty \"patterns\" list", name, configDescription)
		}
		for _, pattern := range module.Patterns {
			if strings.TrimSpace(pattern) == "" {
				return nil, fmt.Errorf("module %q in %s contains an empty pattern", name, configDescription)
			}
		}
	}

	dumpDir := raw.DumpDir
	if dumpDir == "" {
		dumpDir = filepath.Dir(path)
	} else if !filepath.IsAbs(dumpDir) {
		dumpDir = filepath.Join(filepath.Dir(path), dumpDir)
	}
	dumpDir, err = filepath.Abs(dumpDir)
	if err != nil {
		return nil, fmt.Errorf("resolve dump_dir: %w", err)
	}

	return &Config{
		Target:          raw.Target,
		Sources:         raw.Sources,
		SourceOrder:     mappingOrder(&node, "sources", raw.Sources),
		Modules:         raw.Modules,
		ModuleOrder:     mappingOrder(&node, "modules", raw.Modules),
		DumpBaseDir:     filepath.Clean(dumpDir),
		ParallelExports: parallelExports,
		Path:            path,
	}, nil
}

func requireYAMLMapping(node *yaml.Node, path string) error {
	if len(node.Content) > 0 && node.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s must contain a YAML mapping", path)
	}
	return nil
}

// Mapping entries merge recursively so a local password does not replace the
// other target fields. Explicit scalars, sequences, and nulls replace the base.
func mergeYAML(base, local *yaml.Node) {
	if local.Kind == yaml.DocumentNode {
		if len(local.Content) == 0 {
			return
		}
		if len(base.Content) == 0 {
			*base = *local
			return
		}
		mergeYAML(base.Content[0], local.Content[0])
		return
	}
	if base.Kind != yaml.MappingNode || local.Kind != yaml.MappingNode {
		*base = *local
		return
	}
	for i := 0; i+1 < len(local.Content); i += 2 {
		key, value := local.Content[i], local.Content[i+1]
		found := false
		for j := 0; j+1 < len(base.Content); j += 2 {
			if base.Content[j].Value == key.Value {
				mergeYAML(base.Content[j+1], value)
				found = true
				break
			}
		}
		if !found {
			base.Content = append(base.Content, key, value)
		}
	}
}

func Resolve(explicitPath string) (string, error) {
	if explicitPath != "" {
		path, err := filepath.Abs(explicitPath)
		if err != nil {
			return "", fmt.Errorf("resolve config path: %w", err)
		}
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return "", fmt.Errorf("config file not found: %s", path)
		}
		return filepath.Clean(path), nil
	}

	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	for {
		candidate := filepath.Join(dir, DirName, FileName)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("%w: no %s/%s found in the current directory or any parent", ErrNotFound, DirName, FileName)
}

func validateDatabase(db *Database, context string, target bool) error {
	if _, _, err := db.Timeouts(); err != nil {
		return fmt.Errorf("%s.%w", context, err)
	}
	switch db.SSLMode {
	case "", "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return fmt.Errorf("%s.sslmode must be disable, allow, prefer, require, verify-ca, or verify-full", context)
	}
	if strings.TrimSpace(db.Host) == "" {
		return fmt.Errorf("missing %q", context+".host")
	}
	if db.Port == 0 {
		return fmt.Errorf("missing or invalid %q", context+".port")
	}
	if strings.TrimSpace(db.DBName) == "" {
		return fmt.Errorf("missing %q", context+".dbname")
	}
	if strings.TrimSpace(db.User) == "" {
		return fmt.Errorf("missing %q", context+".user")
	}
	if db.Schema == "" {
		db.Schema = DefaultSchema
	}
	if strings.TrimSpace(db.Schema) == "" {
		return fmt.Errorf("%q must not be empty", context+".schema")
	}
	if !target {
		db.Password = ""
	}
	return nil
}

func mappingOrder[T any](document *yaml.Node, key string, values map[string]T) []string {
	if len(document.Content) == 0 {
		return sortedKeys(values)
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return sortedKeys(values)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != key || root.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		mapping := root.Content[i+1]
		order := make([]string, 0, len(values))
		for j := 0; j+1 < len(mapping.Content); j += 2 {
			name := mapping.Content[j].Value
			if _, ok := values[name]; ok {
				order = append(order, name)
			}
		}
		if len(order) == len(values) {
			return order
		}
	}
	return sortedKeys(values)
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slicesSort(keys)
	return keys
}

// Kept local to avoid making ordering depend on YAML-map iteration.
func slicesSort(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
