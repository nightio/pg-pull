package dump

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteLoadAndOpen(t *testing.T) {
	dir, err := CreateDir(t.TempDir(), time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	relative := filepath.Join("tables", "0001.copy.gz")
	stats, err := WriteCompressed(filepath.Join(dir, relative), nil, func(w io.Writer) error {
		_, err := io.WriteString(w, "1\thello\\nworld\n")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		CreatedAt:    time.Now(),
		Source:       "staging",
		SourceSchema: "public",
		Tables: []Table{{
			Name: "example", File: relative,
			Columns:         []Column{{Name: "id", Type: "integer"}},
			CompressedBytes: stats.CompressedBytes, UncompressedBytes: stats.UncompressedBytes, SHA256: stats.SHA256,
		}},
	}
	if err := WriteSequences(dir, []Sequence{}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSchema(dir, Schema{Tables: []TableDefinition{{Name: "example", Columns: []ColumnDefinition{{Name: "id", Type: "integer"}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := Load(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenTable(dir, loaded.Tables[0])
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "1\thello\\nworld\n" {
		t.Fatalf("unexpected payload %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, SchemaFile), []byte(`{"tables":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir, false); err == nil {
		t.Fatal("expected schema checksum mismatch")
	}
}

func TestRejectsPreviousV1Layout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(`{"format":"pg-pull-dump","version":1,"tables":[{"name":"items"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir, false); err == nil {
		t.Fatal("previous v1 layout must not be accepted")
	}
}

func TestLoadRejectsTraversalAndCorruption(t *testing.T) {
	dir := t.TempDir()
	manifest := Manifest{
		CreatedAt: time.Now(), Source: "x", SourceSchema: "public",
		Tables: []Table{{Name: "bad", File: "../secret", Columns: []Column{{Name: "id"}}}},
	}
	if err := WriteSequences(dir, nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteSchema(dir, Schema{Tables: []TableDefinition{{Name: "bad", Columns: []ColumnDefinition{{Name: "id", Type: "integer"}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir, true); err == nil {
		t.Fatal("expected path traversal rejection")
	}

	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"format":"wrong","version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir, false); err == nil {
		t.Fatal("expected format rejection")
	}
}

func TestLoadRejectsChangedTableFile(t *testing.T) {
	dir, err := CreateDir(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	relative := filepath.Join("tables", "0001.copy.gz")
	stats, err := WriteCompressed(filepath.Join(dir, relative), nil, func(w io.Writer) error {
		_, err := io.WriteString(w, "1\n")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{CreatedAt: time.Now(), Source: "test", SourceSchema: "public", Tables: []Table{{
		Name: "items", File: relative, Columns: []Column{{Name: "id", Type: "integer"}}, RowCount: 1,
		CompressedBytes: stats.CompressedBytes, UncompressedBytes: stats.UncompressedBytes, SHA256: stats.SHA256,
	}}}
	if err := WriteSequences(dir, nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteSchema(dir, Schema{Tables: []TableDefinition{{Name: "items", Columns: []ColumnDefinition{{Name: "id", Type: "integer"}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(dir, relative), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("tampered")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir, true); err == nil {
		t.Fatal("expected checksum or size validation to reject changed file")
	}
}

func TestLoadRejectsTableSymlinkOutsideDump(t *testing.T) {
	base := t.TempDir()
	dir, err := CreateDir(base, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.copy.gz")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	relative := filepath.Join("tables", "0001.copy.gz")
	if err := os.Symlink(outside, filepath.Join(dir, relative)); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{CreatedAt: time.Now(), Source: "test", SourceSchema: "public", Tables: []Table{{
		Name: "items", File: relative, Columns: []Column{{Name: "id", Type: "integer"}},
	}}}
	if err := WriteSequences(dir, nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteSchema(dir, Schema{Tables: []TableDefinition{{Name: "items", Columns: []ColumnDefinition{{Name: "id", Type: "integer"}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir, false); err == nil {
		t.Fatal("expected symlink escaping dump directory to be rejected")
	}
}

func TestValidateSequenceMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Schema, *[]Sequence)
		valid  bool
	}{
		{"owned", func(*Schema, *[]Sequence) {}, true},
		{"unowned_shared", func(s *Schema, _ *[]Sequence) { s.Sequences[0].OwnerTable = ""; s.Sequences[0].OwnerColumn = "" }, true},
		{"incomplete_owner", func(s *Schema, _ *[]Sequence) { s.Sequences[0].OwnerColumn = "" }, false},
		{"unknown_consumer", func(s *Schema, _ *[]Sequence) { s.Sequences[0].ReferencedTables = []string{"missing"} }, false},
		{"duplicate_consumer", func(s *Schema, _ *[]Sequence) { s.Sequences[0].ReferencedTables = []string{"a", "a"} }, false},
		{"duplicate_state", func(_ *Schema, s *[]Sequence) { (*s)[1].Name = "counter1" }, false},
		{"incomplete_collation", func(s *Schema, _ *[]Sequence) { s.Tables[0].Columns[0].Collation = &Collation{Name: "C"} }, false},
		{"unknown_constraint_kind", func(s *Schema, _ *[]Sequence) {
			s.Tables[0].Constraints = []NamedDefinition{{Name: "key", Kind: "invalid"}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := Manifest{Tables: []Table{{Name: "a", Columns: []Column{{Name: "id", Type: "bigint"}}}}}
			schema := Schema{Tables: []TableDefinition{{Name: "a", Columns: []ColumnDefinition{{Name: "id", Type: "bigint"}}}}, Sequences: []SequenceDefinition{
				{Name: "counter1", OwnerTable: "a", OwnerColumn: "id", ReferencedTables: []string{"a"}},
				{Name: "counter2", ReferencedTables: []string{"a"}},
			}}
			states := []Sequence{{Name: "counter1", LastValue: 5, IsCalled: true}, {Name: "counter2", LastValue: 6}}
			tc.change(&schema, &states)
			if err := validateMetadata(manifest, states, schema); (err == nil) != tc.valid {
				t.Fatalf("validation error=%v, want valid=%t", err, tc.valid)
			}
		})
	}
}
