package dump

import "time"

const (
	FormatName    = "pg-pull-dump"
	FormatVersion = 1
	ManifestFile  = "manifest.json"
	SequencesFile = "sequences.json"
	SchemaFile    = "schema.json"
)

type Manifest struct {
	Format              string            `json:"format"`
	Version             int               `json:"version"`
	CreatedAt           time.Time         `json:"created_at"`
	Source              string            `json:"source"`
	SourceServerVersion string            `json:"source_server_version"`
	SourceSchema        string            `json:"source_schema"`
	SchemaSHA256        string            `json:"schema_sha256"`
	SequencesSHA256     string            `json:"sequences_sha256"`
	Modules             []string          `json:"modules"`
	CopySettings        map[string]string `json:"copy_settings"`
	Tables              []Table           `json:"tables"`
}

type Table struct {
	Name              string   `json:"name"`
	File              string   `json:"file"`
	Columns           []Column `json:"columns"`
	RowCount          int64    `json:"row_count"`
	CompressedBytes   int64    `json:"compressed_bytes"`
	UncompressedBytes int64    `json:"uncompressed_bytes"`
	SHA256            string   `json:"sha256"`
}

type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type Sequence struct {
	Name      string `json:"name"`
	LastValue int64  `json:"last_value"`
	IsCalled  bool   `json:"is_called"`
}

type Entry struct {
	Path     string
	Manifest Manifest
}

type Schema struct {
	Tables    []TableDefinition    `json:"tables"`
	Enums     []EnumDefinition     `json:"enums"`
	Domains   []DomainDefinition   `json:"domains"`
	Sequences []SequenceDefinition `json:"sequences"`
}

type TableDefinition struct {
	Name        string             `json:"name"`
	Unlogged    bool               `json:"unlogged,omitempty"`
	CustomTypes []string           `json:"custom_types,omitempty"`
	Columns     []ColumnDefinition `json:"columns"`
	Constraints []NamedDefinition  `json:"constraints,omitempty"`
	Indexes     []NamedDefinition  `json:"indexes,omitempty"`
	Unsupported []string           `json:"unsupported,omitempty"`
}

type Collation struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

type ColumnDefinition struct {
	Collation        *Collation `json:"collation,omitempty"`
	Name             string     `json:"name"`
	Type             string     `json:"type"`
	NotNull          bool       `json:"not_null,omitempty"`
	Default          string     `json:"default,omitempty"`
	Generated        string     `json:"generated,omitempty"`
	Identity         string     `json:"identity,omitempty"`
	IdentitySequence string     `json:"identity_sequence,omitempty"`
}

type NamedDefinition struct {
	Kind       string `json:"kind,omitempty"`
	Name       string `json:"name"`
	Definition string `json:"definition"`
}

type EnumDefinition struct {
	Name   string   `json:"name"`
	Labels []string `json:"labels"`
}

type DomainDefinition struct {
	Name           string            `json:"name"`
	BaseType       string            `json:"base_type"`
	BaseCustomType string            `json:"base_custom_type,omitempty"`
	NotNull        bool              `json:"not_null,omitempty"`
	Default        string            `json:"default,omitempty"`
	Checks         []NamedDefinition `json:"checks,omitempty"`
}

type SequenceDefinition struct {
	Name             string   `json:"name"`
	OwnerTable       string   `json:"owner_table,omitempty"`
	OwnerColumn      string   `json:"owner_column,omitempty"`
	ReferencedTables []string `json:"referenced_tables,omitempty"`
	Identity         bool     `json:"identity,omitempty"`
	Type             string   `json:"type"`
	Start            int64    `json:"start"`
	Increment        int64    `json:"increment"`
	Minimum          int64    `json:"minimum"`
	Maximum          int64    `json:"maximum"`
	Cache            int64    `json:"cache"`
	Cycle            bool     `json:"cycle,omitempty"`
}
