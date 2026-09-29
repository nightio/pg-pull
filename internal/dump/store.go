package dump

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type FileStats struct {
	CompressedBytes   int64
	UncompressedBytes int64
	SHA256            string
}

type countingWriter struct {
	w       io.Writer
	written int64
	onWrite func(int64)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.written += int64(n)
	if w.onWrite != nil && n > 0 {
		w.onWrite(w.written)
	}
	return n, err
}

type hashingWriter struct {
	w       io.Writer
	hash    hash.Hash
	written int64
}

func (w *hashingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		_, _ = w.hash.Write(p[:n])
		w.written += int64(n)
	}
	return n, err
}

func CreateDir(base string, now time.Time) (string, error) {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create dump base directory: %w", err)
	}
	name := now.Format("2006-01-02_15-04-05")
	for suffix := 0; ; suffix++ {
		candidate := filepath.Join(base, name)
		if suffix > 0 {
			candidate = filepath.Join(base, fmt.Sprintf("%s-%d", name, suffix))
		}
		err := os.Mkdir(candidate, 0o700)
		if err == nil {
			if err := os.Mkdir(filepath.Join(candidate, "tables"), 0o700); err != nil {
				_ = os.Remove(candidate)
				return "", fmt.Errorf("create table dump directory: %w", err)
			}
			return candidate, nil
		}
		if !os.IsExist(err) {
			return "", fmt.Errorf("create dump directory: %w", err)
		}
	}
}

// WriteCompressed writes a table COPY stream through gzip. The final path only
// appears after the producer and all compression/fsync steps have succeeded.
func WriteCompressed(path string, onWrite func(int64), producer func(io.Writer) error) (FileStats, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return FileStats{}, err
	}
	tmp := path + ".partial"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return FileStats{}, fmt.Errorf("create %s: %w", tmp, err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tmp)
	}

	h := sha256.New()
	compressed := &hashingWriter{w: file, hash: h}
	gz, err := gzip.NewWriterLevel(compressed, gzip.BestSpeed)
	if err != nil {
		cleanup()
		return FileStats{}, err
	}
	// Stable gzip headers make archives reproducible for identical COPY bytes.
	gz.Header.ModTime = time.Unix(0, 0)
	uncompressed := &countingWriter{w: gz, onWrite: onWrite}
	if err := producer(uncompressed); err != nil {
		_ = gz.Close()
		cleanup()
		return FileStats{}, err
	}
	if err := gz.Close(); err != nil {
		cleanup()
		return FileStats{}, fmt.Errorf("close gzip stream: %w", err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return FileStats{}, fmt.Errorf("sync table dump: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return FileStats{}, fmt.Errorf("close table dump: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return FileStats{}, fmt.Errorf("publish table dump: %w", err)
	}
	return FileStats{
		CompressedBytes:   compressed.written,
		UncompressedBytes: uncompressed.written,
		SHA256:            hex.EncodeToString(h.Sum(nil)),
	}, nil
}

func WriteSequences(dir string, sequences []Sequence) error {
	return writeJSONAtomic(filepath.Join(dir, SequencesFile), sequences)
}

func WriteSchema(dir string, schema Schema) error {
	return writeJSONAtomic(filepath.Join(dir, SchemaFile), schema)
}

func WriteManifest(dir string, manifest Manifest) error {
	var err error
	manifest.SchemaSHA256, err = fileSHA256(filepath.Join(dir, SchemaFile))
	if err != nil {
		return fmt.Errorf("checksum schema: %w", err)
	}
	manifest.SequencesSHA256, err = fileSHA256(filepath.Join(dir, SequencesFile))
	if err != nil {
		return fmt.Errorf("checksum sequences: %w", err)
	}
	manifest.Format = FormatName
	manifest.Version = FormatVersion
	return writeJSONAtomic(filepath.Join(dir, ManifestFile), manifest)
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func Load(dir string, verifyFiles bool) (Manifest, []Sequence, error) {
	var manifest Manifest
	if err := readJSON(filepath.Join(dir, ManifestFile), &manifest); err != nil {
		return Manifest{}, nil, fmt.Errorf("read manifest: %w", err)
	}
	if manifest.Format != FormatName || manifest.Version != FormatVersion {
		return Manifest{}, nil, fmt.Errorf("unsupported dump format %q version %d", manifest.Format, manifest.Version)
	}
	// The required schema checksum distinguishes the replacement v1 layout from
	// older dumps that carried the same version number.
	if len(manifest.SchemaSHA256) != 64 || len(manifest.SequencesSHA256) != 64 {
		return Manifest{}, nil, fmt.Errorf("obsolete dump layout: create a new dump")
	}
	for _, file := range []struct{ name, checksum string }{{SchemaFile, manifest.SchemaSHA256}, {SequencesFile, manifest.SequencesSHA256}} {
		actual, err := fileSHA256(filepath.Join(dir, file.name))
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("read %s: %w", file.name, err)
		}
		if actual != file.checksum {
			return Manifest{}, nil, fmt.Errorf("%s SHA-256 mismatch", file.name)
		}
	}
	if len(manifest.Tables) == 0 {
		return Manifest{}, nil, fmt.Errorf("manifest contains no tables")
	}
	seen := make(map[string]struct{}, len(manifest.Tables))
	for _, table := range manifest.Tables {
		if table.Name == "" || len(table.Columns) == 0 {
			return Manifest{}, nil, fmt.Errorf("manifest contains an invalid table entry")
		}
		cleanFile := filepath.Clean(table.File)
		if !strings.HasPrefix(cleanFile, "tables"+string(filepath.Separator)) || !strings.HasSuffix(cleanFile, ".copy.gz") {
			return Manifest{}, nil, fmt.Errorf("table %q has invalid data path %q", table.Name, table.File)
		}
		path, err := safePath(dir, table.File)
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("table %q: %w", table.Name, err)
		}
		if _, exists := seen[path]; exists {
			return Manifest{}, nil, fmt.Errorf("multiple tables reference %s", table.File)
		}
		seen[path] = struct{}{}
		if verifyFiles {
			if err := verifyFile(path, table); err != nil {
				return Manifest{}, nil, fmt.Errorf("table %q: %w", table.Name, err)
			}
		}
	}

	var sequences []Sequence
	if err := readJSON(filepath.Join(dir, SequencesFile), &sequences); err != nil {
		return Manifest{}, nil, fmt.Errorf("read sequences: %w", err)
	}
	schema, err := LoadSchema(dir)
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("read schema: %w", err)
	}
	if err := validateMetadata(manifest, sequences, schema); err != nil {
		return Manifest{}, nil, err
	}
	return manifest, sequences, nil
}

func validateMetadata(manifest Manifest, sequences []Sequence, schema Schema) error {
	definitions := make(map[string]TableDefinition, len(schema.Tables))
	for _, table := range schema.Tables {
		if table.Name == "" || len(table.Columns) == 0 {
			return fmt.Errorf("invalid schema table definition")
		}
		if _, exists := definitions[table.Name]; exists {
			return fmt.Errorf("duplicate schema table %s", table.Name)
		}
		for _, col := range table.Columns {
			if col.Collation != nil && (col.Collation.Schema == "" || col.Collation.Name == "") {
				return fmt.Errorf("invalid collation for %s.%s", table.Name, col.Name)
			}
		}
		for _, constraint := range table.Constraints {
			switch constraint.Kind {
			case "p", "u", "c", "f":
			default:
				return fmt.Errorf("invalid constraint kind for %s.%s", table.Name, constraint.Name)
			}
		}
		definitions[table.Name] = table
	}
	if len(definitions) != len(manifest.Tables) {
		return fmt.Errorf("schema and manifest table counts differ")
	}
	seenNames := make(map[string]struct{}, len(manifest.Tables))
	for _, table := range manifest.Tables {
		if _, exists := seenNames[table.Name]; exists {
			return fmt.Errorf("duplicate manifest table %s", table.Name)
		}
		seenNames[table.Name] = struct{}{}
		def, ok := definitions[table.Name]
		if !ok {
			return fmt.Errorf("schema lacks table %s", table.Name)
		}
		var writable []Column
		for _, col := range def.Columns {
			if col.Generated == "" {
				writable = append(writable, Column{Name: col.Name, Type: col.Type})
			}
		}
		if len(writable) != len(table.Columns) {
			return fmt.Errorf("columns differ for table %s", table.Name)
		}
		for i, col := range table.Columns {
			if writable[i] != col {
				return fmt.Errorf("columns differ for table %s", table.Name)
			}
		}
	}
	sequenceDefinitions := make(map[string]struct{}, len(schema.Sequences))
	for _, seq := range schema.Sequences {
		if seq.Name == "" || (seq.OwnerTable == "") != (seq.OwnerColumn == "") || (seq.Identity && seq.OwnerTable == "") {
			return fmt.Errorf("invalid sequence definition %s", seq.Name)
		}
		if _, ok := sequenceDefinitions[seq.Name]; ok {
			return fmt.Errorf("duplicate sequence %s", seq.Name)
		}
		sequenceDefinitions[seq.Name] = struct{}{}
		seenRefs := make(map[string]struct{})
		for _, table := range seq.ReferencedTables {
			if _, ok := definitions[table]; !ok {
				return fmt.Errorf("sequence %s references unknown table %s", seq.Name, table)
			}
			if _, ok := seenRefs[table]; ok {
				return fmt.Errorf("duplicate sequence reference %s.%s", seq.Name, table)
			}
			seenRefs[table] = struct{}{}
		}
	}
	if len(sequenceDefinitions) != len(sequences) {
		return fmt.Errorf("schema and sequence counts differ")
	}
	for _, seq := range sequences {
		if _, ok := sequenceDefinitions[seq.Name]; !ok {
			return fmt.Errorf("sequence %s does not match schema", seq.Name)
		}
		delete(sequenceDefinitions, seq.Name)
	}
	return nil
}

func LoadSchema(dir string) (Schema, error) {
	var schema Schema
	if err := readJSON(filepath.Join(dir, SchemaFile), &schema); err != nil {
		return Schema{}, err
	}
	if len(schema.Tables) == 0 {
		return Schema{}, fmt.Errorf("schema contains no tables")
	}
	return schema, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func safePath(dir, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", fmt.Errorf("invalid table file path %q", relative)
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("table file escapes dump directory: %q", relative)
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(dir, clean))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("table file escapes dump directory: %q", relative)
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve dump directory: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	realRel, err := filepath.Rel(realBase, realPath)
	if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("table file symlink escapes dump directory: %q", relative)
	}
	return path, nil
}

func verifyFile(path string, table Table) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	h := sha256.New()
	n, err := io.Copy(h, file)
	if err != nil {
		return err
	}
	if n != table.CompressedBytes {
		return fmt.Errorf("size is %d bytes, expected %d", n, table.CompressedBytes)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != table.SHA256 {
		return fmt.Errorf("SHA-256 mismatch")
	}
	return nil
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON data")
		}
		return err
	}
	return nil
}

// List returns complete current-format dumps newest first and the number of
// legacy dump.sql directories that were deliberately left untouched.
func List(base string) ([]Entry, int, error) {
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	result := make([]Entry, 0)
	legacy := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, "dump.sql")); err == nil {
			legacy++
			continue
		}
		manifest, _, err := Load(dir, false)
		if err != nil {
			continue
		}
		result = append(result, Entry{Path: dir, Manifest: manifest})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Manifest.CreatedAt.After(result[j].Manifest.CreatedAt)
	})
	return result, legacy, nil
}

func HasData(dir string) bool {
	tablesDir := filepath.Join(dir, "tables")
	entries, err := os.ReadDir(tablesDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			if info, err := entry.Info(); err == nil && info.Size() > 0 {
				return true
			}
		}
	}
	return false
}

func RemoveIfEmpty(dir string) error {
	if HasData(dir) {
		return nil
	}
	return os.RemoveAll(dir)
}

func OpenTable(dir string, table Table) (io.ReadCloser, error) {
	path, err := safePath(dir, table.File)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &compoundReadCloser{Reader: gz, closers: []io.Closer{gz, file}}, nil
}

type compoundReadCloser struct {
	io.Reader
	closers []io.Closer
}

func (r *compoundReadCloser) Close() error {
	var first error
	for _, closer := range r.closers {
		if err := closer.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
