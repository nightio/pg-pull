package database

import (
	"context"
	"sort"
	"strings"
)

// Table selection and sequence ownership belong to the source snapshot: a
// fresh dump must not depend on a local database or a changing source catalog.
func (s *Snapshot) TablesForPatterns(ctx context.Context, schema string, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	parts := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		parts = append(parts, "("+pattern+")")
	}
	rows, err := s.tx.Query(ctx, `
SELECT tablename FROM pg_catalog.pg_tables
WHERE schemaname = $1 AND tablename ~ $2 ORDER BY tablename`, schema, strings.Join(parts, "|"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		result = append(result, table)
	}
	return result, rows.Err()
}

func (s *Snapshot) SequenceRefs(ctx context.Context, schema string, tables []string) ([]SequenceRef, error) {
	// Discover actual ownership even when the owner is not among the selected
	// tables. A DEFAULT reference is a consumer, never evidence of ownership.
	rows, err := s.tx.Query(ctx, `
SELECT seq.relname, COALESCE(tbl.relname, ''), COALESCE(a.attname, '')
FROM pg_catalog.pg_class seq
JOIN pg_catalog.pg_namespace n ON n.oid = seq.relnamespace
LEFT JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_class'::regclass
 AND d.objid = seq.oid AND d.refclassid = 'pg_catalog.pg_class'::regclass AND d.deptype IN ('a', 'i')
LEFT JOIN pg_catalog.pg_class tbl ON tbl.oid = d.refobjid
LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = tbl.oid AND a.attnum = d.refobjsubid
WHERE n.nspname = $1 AND seq.relkind = 'S' ORDER BY seq.relname`, schema)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]SequenceRef)
	for rows.Next() {
		var ref SequenceRef
		if err := rows.Scan(&ref.Name, &ref.OwnerTable, &ref.OwnerColumn); err != nil {
			rows.Close()
			return nil, err
		}
		refs[ref.Name] = ref
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	defaultRows, err := s.tx.Query(ctx, `
SELECT DISTINCT seq.relname, tbl.relname
FROM pg_catalog.pg_attrdef ad
JOIN pg_catalog.pg_class tbl ON tbl.oid = ad.adrelid
JOIN pg_catalog.pg_namespace tn ON tn.oid = tbl.relnamespace
JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_attrdef'::regclass AND d.objid = ad.oid
 AND d.refclassid = 'pg_catalog.pg_class'::regclass
JOIN pg_catalog.pg_class seq ON seq.oid = d.refobjid AND seq.relkind = 'S'
JOIN pg_catalog.pg_namespace sn ON sn.oid = seq.relnamespace
WHERE tn.nspname = $1 AND sn.nspname = $1 AND tbl.relname = ANY($2::text[])
ORDER BY seq.relname, tbl.relname`, schema, tables)
	if err != nil {
		return nil, err
	}
	for defaultRows.Next() {
		var name, table string
		if err := defaultRows.Scan(&name, &table); err != nil {
			defaultRows.Close()
			return nil, err
		}
		ref := refs[name]
		ref.ReferencedTables = append(ref.ReferencedTables, table)
		refs[name] = ref
	}
	defaultRows.Close()
	if err := defaultRows.Err(); err != nil {
		return nil, err
	}
	var result []SequenceRef
	for _, ref := range refs {
		if ref.OwnerTable == "" && len(ref.ReferencedTables) == 0 {
			best := ""
			for _, table := range tables {
				if strings.HasPrefix(ref.Name, table+"_") && strings.HasSuffix(ref.Name, "_seq") && len(table) > len(best) {
					best = table
				}
			}
			if best != "" {
				ref.ReferencedTables = []string{best}
			}
		}
		if containsString(tables, ref.OwnerTable) || len(ref.ReferencedTables) > 0 {
			result = append(result, ref)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}
