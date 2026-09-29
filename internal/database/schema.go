package database

import (
	"context"
	"fmt"
	"sort"

	"github.com/nightio/pg-pull/internal/dump"
)

func (s *Snapshot) CaptureSchema(ctx context.Context, schema string, tables []string, refs []SequenceRef) (dump.Schema, error) {
	// Deparsed defaults, constraints, and indexes must resolve source objects by
	// unqualified name, so restore can resolve them in the configured target schema.
	if _, err := s.tx.Exec(ctx, "SET LOCAL search_path TO "+quoteIdentifier(schema)+", pg_catalog"); err != nil {
		return dump.Schema{}, err
	}
	result := dump.Schema{}
	typeKinds := make(map[string]string)
	typeCache := make(map[uint32]pgTypeInfo)
	tableTypeOIDs := make(map[string][]uint32, len(tables))
	for _, table := range tables {
		var oid uint32
		var kind, persistence string
		var partition, rls, rules, triggers, inherited bool
		err := s.tx.QueryRow(ctx, `
SELECT c.oid, c.relkind, c.relpersistence, c.relispartition, c.relrowsecurity OR c.relforcerowsecurity,
       c.relhasrules, EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t WHERE t.tgrelid = c.oid AND NOT t.tgisinternal),
       EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhrelid = c.oid OR i.inhparent = c.oid)
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2`, schema, table).
			Scan(&oid, &kind, &persistence, &partition, &rls, &rules, &triggers, &inherited)
		if err != nil {
			return dump.Schema{}, fmt.Errorf("inspect table %s: %w", table, err)
		}
		definition := dump.TableDefinition{Name: table, Unlogged: persistence == "u"}
		if kind != "r" || partition || inherited {
			definition.Unsupported = append(definition.Unsupported, "partitioning or inheritance")
		}
		if rls {
			definition.Unsupported = append(definition.Unsupported, "row-level security")
		}
		if rules {
			definition.Unsupported = append(definition.Unsupported, "rules")
		}
		if triggers {
			definition.Unsupported = append(definition.Unsupported, "user triggers")
		}

		rows, err := s.tx.Query(ctx, `
SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull,
       COALESCE(pg_catalog.pg_get_expr(d.adbin, d.adrelid, false), ''), a.attgenerated, a.attidentity,
       a.atttypid, COALESCE(cn.nspname, ''), COALESCE(co.collname, '')
FROM pg_catalog.pg_attribute a
LEFT JOIN pg_catalog.pg_collation co ON co.oid = a.attcollation
 AND co.oid <> 'pg_catalog.default'::regcollation
LEFT JOIN pg_catalog.pg_namespace cn ON cn.oid = co.collnamespace
LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, oid)
		if err != nil {
			return dump.Schema{}, err
		}
		for rows.Next() {
			var col dump.ColumnDefinition
			var expr, generated, identity string
			var typeOID uint32
			var collationSchema, collationName string
			if err := rows.Scan(&col.Name, &col.Type, &col.NotNull, &expr, &generated, &identity, &typeOID, &collationSchema, &collationName); err != nil {
				rows.Close()
				return dump.Schema{}, err
			}
			if collationName != "" {
				col.Collation = &dump.Collation{Schema: collationSchema, Name: collationName}
			}
			if generated == "s" || generated == "v" {
				col.Generated = expr
				if generated == "v" {
					definition.Unsupported = append(definition.Unsupported, "virtual generated column "+col.Name)
				}
			} else {
				col.Default = expr
			}
			if identity == "a" || identity == "d" {
				col.Identity = identity
			}
			definition.Columns = append(definition.Columns, col)
			tableTypeOIDs[table] = append(tableTypeOIDs[table], typeOID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return dump.Schema{}, err
		}

		constraints, err := s.tx.Query(ctx, `SELECT conname, contype, pg_catalog.pg_get_constraintdef(oid, false)
FROM pg_catalog.pg_constraint WHERE conrelid = $1 ORDER BY conname`, oid)
		if err != nil {
			return dump.Schema{}, err
		}
		for constraints.Next() {
			var name, kind, sql string
			if err := constraints.Scan(&name, &kind, &sql); err != nil {
				constraints.Close()
				return dump.Schema{}, err
			}
			if kind == "p" || kind == "u" || kind == "c" || kind == "f" {
				definition.Constraints = append(definition.Constraints, dump.NamedDefinition{Name: name, Kind: kind, Definition: sql})
			} else if kind != "n" {
				definition.Unsupported = append(definition.Unsupported, "constraint "+name+" ("+kind+")")
			}
		}
		constraints.Close()
		if err := constraints.Err(); err != nil {
			return dump.Schema{}, err
		}
		indexes, err := s.tx.Query(ctx, `SELECT ic.relname, pg_catalog.pg_get_indexdef(i.indexrelid, 0, false)
FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
WHERE i.indrelid = $1 AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint c WHERE c.conindid = i.indexrelid AND c.conrelid = i.indrelid AND c.contype IN ('p', 'u', 'x'))
ORDER BY ic.relname`, oid)
		if err != nil {
			return dump.Schema{}, err
		}
		for indexes.Next() {
			var item dump.NamedDefinition
			if err := indexes.Scan(&item.Name, &item.Definition); err != nil {
				indexes.Close()
				return dump.Schema{}, err
			}
			definition.Indexes = append(definition.Indexes, item)
		}
		indexes.Close()
		if err := indexes.Err(); err != nil {
			return dump.Schema{}, err
		}
		result.Tables = append(result.Tables, definition)
	}
	for i := range result.Tables {
		seen := make(map[uint32]struct{})
		for _, oid := range tableTypeOIDs[result.Tables[i].Name] {
			if err := s.captureCustomType(ctx, schema, oid, &result.Tables[i], typeKinds, typeCache, seen); err != nil {
				return dump.Schema{}, err
			}
		}
	}
	for name, kind := range typeKinds {
		switch kind {
		case "e":
			labels, err := s.enumLabels(ctx, schema, name)
			if err != nil {
				return dump.Schema{}, err
			}
			result.Enums = append(result.Enums, dump.EnumDefinition{Name: name, Labels: labels})
		case "d":
			domain, err := s.domainDefinition(ctx, schema, name, typeCache)
			if err != nil {
				return dump.Schema{}, err
			}
			result.Domains = append(result.Domains, domain)
		default:
			for i := range result.Tables {
				if containsString(result.Tables[i].CustomTypes, name) {
					result.Tables[i].Unsupported = append(result.Tables[i].Unsupported, "custom type "+name)
				}
			}
		}
	}
	for _, ref := range refs {
		var seq dump.SequenceDefinition
		var dependency *string
		err := s.tx.QueryRow(ctx, `
SELECT pg_catalog.format_type(q.seqtypid, NULL), q.seqstart, q.seqincrement, q.seqmin, q.seqmax, q.seqcache, q.seqcycle,
       d.deptype
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_sequence q ON q.seqrelid = c.oid
LEFT JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = c.oid AND d.refclassid = 'pg_catalog.pg_class'::regclass AND d.deptype IN ('a','i')
WHERE n.nspname = $1 AND c.relname = $2 LIMIT 1`, schema, ref.Name).
			Scan(&seq.Type, &seq.Start, &seq.Increment, &seq.Minimum, &seq.Maximum, &seq.Cache, &seq.Cycle, &dependency)
		if err != nil {
			return dump.Schema{}, fmt.Errorf("inspect sequence %s: %w", ref.Name, err)
		}
		seq.Name, seq.OwnerTable, seq.OwnerColumn = ref.Name, ref.OwnerTable, ref.OwnerColumn
		seq.ReferencedTables = append([]string(nil), ref.ReferencedTables...)
		seq.Identity = dependency != nil && *dependency == "i"
		result.Sequences = append(result.Sequences, seq)
		if seq.Identity {
			for i := range result.Tables {
				if result.Tables[i].Name != seq.OwnerTable {
					continue
				}
				for j := range result.Tables[i].Columns {
					if result.Tables[i].Columns[j].Name == seq.OwnerColumn {
						result.Tables[i].Columns[j].IdentitySequence = seq.Name
					}
				}
			}
		}
	}
	sort.Slice(result.Enums, func(i, j int) bool { return result.Enums[i].Name < result.Enums[j].Name })
	sort.Slice(result.Domains, func(i, j int) bool { return result.Domains[i].Name < result.Domains[j].Name })
	return result, nil
}

func containsString(items []string, item string) bool {
	for _, candidate := range items {
		if candidate == item {
			return true
		}
	}
	return false
}

type pgTypeInfo struct {
	Schema, Name, Kind, Category string
	Element, Base                uint32
}

func (s *Snapshot) typeInfo(ctx context.Context, oid uint32, cache map[uint32]pgTypeInfo) (pgTypeInfo, error) {
	if info, ok := cache[oid]; ok {
		return info, nil
	}
	var info pgTypeInfo
	err := s.tx.QueryRow(ctx, `SELECT n.nspname, t.typname, t.typtype, t.typcategory, t.typelem, t.typbasetype
FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
WHERE t.oid = $1`, oid).Scan(&info.Schema, &info.Name, &info.Kind, &info.Category, &info.Element, &info.Base)
	if err != nil {
		return pgTypeInfo{}, fmt.Errorf("inspect type %d: %w", oid, err)
	}
	cache[oid] = info
	return info, nil
}

func (s *Snapshot) captureCustomType(ctx context.Context, schema string, oid uint32, table *dump.TableDefinition, kinds map[string]string, cache map[uint32]pgTypeInfo, seen map[uint32]struct{}) error {
	if oid == 0 {
		return nil
	}
	if _, ok := seen[oid]; ok {
		return nil
	}
	seen[oid] = struct{}{}
	info, err := s.typeInfo(ctx, oid, cache)
	if err != nil {
		return err
	}
	// PostgreSQL stores enum[] and domain[] as generated base array types.
	// Follow typelem before classifying them as unsupported custom base types.
	if info.Kind != "d" && info.Category == "A" && info.Element != 0 {
		return s.captureCustomType(ctx, schema, info.Element, table, kinds, cache, seen)
	}
	if info.Schema == schema {
		if !containsString(table.CustomTypes, info.Name) {
			table.CustomTypes = append(table.CustomTypes, info.Name)
		}
		kinds[info.Name] = info.Kind
	}
	if info.Kind == "d" && info.Base != 0 {
		return s.captureCustomType(ctx, schema, info.Base, table, kinds, cache, seen)
	}
	return nil
}

func (s *Snapshot) enumLabels(ctx context.Context, schema, name string) ([]string, error) {
	rows, err := s.tx.Query(ctx, `SELECT e.enumlabel FROM pg_catalog.pg_enum e
JOIN pg_catalog.pg_type t ON t.oid = e.enumtypid JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname = $1 AND t.typname = $2 ORDER BY e.enumsortorder`, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

func (s *Snapshot) domainDefinition(ctx context.Context, schema, name string, cache map[uint32]pgTypeInfo) (dump.DomainDefinition, error) {
	var domain dump.DomainDefinition
	domain.Name = name
	var baseOID uint32
	err := s.tx.QueryRow(ctx, `SELECT pg_catalog.format_type(t.typbasetype, t.typtypmod), t.typnotnull, COALESCE(t.typdefault, ''), t.typbasetype
FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname = $1 AND t.typname = $2`, schema, name).Scan(&domain.BaseType, &domain.NotNull, &domain.Default, &baseOID)
	if err != nil {
		return dump.DomainDefinition{}, err
	}
	for baseOID != 0 {
		info, err := s.typeInfo(ctx, baseOID, cache)
		if err != nil {
			return dump.DomainDefinition{}, err
		}
		if info.Kind != "d" && info.Category == "A" && info.Element != 0 {
			baseOID = info.Element
			continue
		}
		if info.Schema == schema {
			domain.BaseCustomType = info.Name
		}
		break
	}
	rows, err := s.tx.Query(ctx, `SELECT c.conname, pg_catalog.pg_get_constraintdef(c.oid, false)
FROM pg_catalog.pg_constraint c JOIN pg_catalog.pg_type t ON t.oid = c.contypid
JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace WHERE n.nspname = $1 AND t.typname = $2 ORDER BY c.conname`, schema, name)
	if err != nil {
		return dump.DomainDefinition{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var c dump.NamedDefinition
		if err := rows.Scan(&c.Name, &c.Definition); err != nil {
			return dump.DomainDefinition{}, err
		}
		domain.Checks = append(domain.Checks, c)
	}
	return domain, rows.Err()
}
