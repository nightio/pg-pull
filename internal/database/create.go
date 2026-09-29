package database

import (
	"context"
	"fmt"
	"strings"

	"github.com/nightio/pg-pull/internal/dump"
	"github.com/jackc/pgx/v5"
)

// createMissing runs inside the same transaction as TRUNCATE and COPY. A failed
// restore therefore removes every new object along with data changes.
func createMissing(ctx context.Context, tx pgx.Tx, targetSchema, sourceSchema string, schema dump.Schema, create map[string]struct{}) error {
	sequenceByName := make(map[string]dump.SequenceDefinition, len(schema.Sequences))
	for _, seq := range schema.Sequences {
		sequenceByName[seq.Name] = seq
	}
	neededTypes := make(map[string]struct{})
	for _, table := range schema.Tables {
		if _, ok := create[table.Name]; ok {
			if len(table.Unsupported) > 0 {
				return fmt.Errorf("cannot create %s: %s", table.Name, strings.Join(table.Unsupported, ", "))
			}
			for _, name := range table.CustomTypes {
				neededTypes[name] = struct{}{}
			}
		}
	}
	orderedDomains, err := orderNeededDomains(schema, neededTypes)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoteIdentifier(targetSchema)); err != nil {
		return fmt.Errorf("create target schema: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+quoteIdentifier(targetSchema)+", pg_catalog"); err != nil {
		return err
	}
	for _, enum := range schema.Enums {
		if _, ok := neededTypes[enum.Name]; !ok {
			continue
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname=$1 AND t.typname=$2)`, targetSchema, enum.Name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			rows, err := tx.Query(ctx, `SELECT e.enumlabel FROM pg_catalog.pg_enum e JOIN pg_catalog.pg_type t ON t.oid=e.enumtypid JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname=$1 AND t.typname=$2 ORDER BY e.enumsortorder`, targetSchema, enum.Name)
			if err != nil {
				return err
			}
			var labels []string
			for rows.Next() {
				var label string
				if err := rows.Scan(&label); err != nil {
					rows.Close()
					return err
				}
				labels = append(labels, label)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if !sameStrings(labels, enum.Labels) {
				return fmt.Errorf("existing enum %s has different labels", enum.Name)
			}
			continue
		}
		var labels []string
		for _, label := range enum.Labels {
			labels = append(labels, quoteLiteral(label))
		}
		if _, err := tx.Exec(ctx, "CREATE TYPE "+quoteIdentifier(enum.Name)+" AS ENUM ("+strings.Join(labels, ", ")+")"); err != nil {
			return fmt.Errorf("create enum %s: %w", enum.Name, err)
		}
	}
	for _, domain := range orderedDomains {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname=$1 AND t.typname=$2)`, targetSchema, domain.Name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			var base string
			var notNull bool
			var defaultValue string
			if err := tx.QueryRow(ctx, `SELECT pg_catalog.format_type(t.typbasetype,t.typtypmod),t.typnotnull,COALESCE(t.typdefault,'') FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname=$1 AND t.typname=$2`, targetSchema, domain.Name).Scan(&base, &notNull, &defaultValue); err != nil {
				return err
			}
			if base != domain.BaseType || notNull != domain.NotNull || defaultValue != domain.Default {
				return fmt.Errorf("existing domain %s differs from dump", domain.Name)
			}
			rows, err := tx.Query(ctx, `SELECT c.conname,pg_catalog.pg_get_constraintdef(c.oid,false) FROM pg_catalog.pg_constraint c JOIN pg_catalog.pg_type t ON t.oid=c.contypid JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname=$1 AND t.typname=$2 ORDER BY c.conname`, targetSchema, domain.Name)
			if err != nil {
				return err
			}
			var checks []dump.NamedDefinition
			for rows.Next() {
				var check dump.NamedDefinition
				if err := rows.Scan(&check.Name, &check.Definition); err != nil {
					rows.Close()
					return err
				}
				checks = append(checks, check)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if len(checks) != len(domain.Checks) {
				return fmt.Errorf("existing domain %s has different constraints", domain.Name)
			}
			for i := range checks {
				if checks[i] != domain.Checks[i] {
					return fmt.Errorf("existing domain %s has different constraints", domain.Name)
				}
			}
			continue
		}
		query := "CREATE DOMAIN " + quoteIdentifier(domain.Name) + " AS " + domain.BaseType
		if domain.Default != "" {
			query += " DEFAULT " + domain.Default
		}
		if domain.NotNull {
			query += " NOT NULL"
		}
		for _, check := range domain.Checks {
			query += " CONSTRAINT " + quoteIdentifier(check.Name) + " " + check.Definition
		}
		if _, err := tx.Exec(ctx, query); err != nil {
			return fmt.Errorf("create domain %s: %w", domain.Name, err)
		}
	}
	for _, seq := range schema.Sequences {
		if !sequenceNeeded(seq, create) || (seq.Identity && selectedTable(create, seq.OwnerTable)) {
			continue
		}
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", quoteIdentifier(targetSchema, seq.Name)).Scan(&exists); err != nil {
			return err
		}
		if exists {
			var kind, owner, column string
			if err := tx.QueryRow(ctx, `SELECT c.relkind, COALESCE(o.relname,''), COALESCE(a.attname,'')
FROM pg_catalog.pg_class c
LEFT JOIN pg_catalog.pg_depend d ON d.classid='pg_catalog.pg_class'::regclass AND d.objid=c.oid
 AND d.refclassid='pg_catalog.pg_class'::regclass AND d.deptype IN ('a','i')
LEFT JOIN pg_catalog.pg_class o ON o.oid=d.refobjid
LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid=o.oid AND a.attnum=d.refobjsubid
WHERE c.oid=to_regclass($1)`, quoteIdentifier(targetSchema, seq.Name)).Scan(&kind, &owner, &column); err != nil {
				return err
			}
			if kind != "S" {
				return fmt.Errorf("target object %s is not a sequence", seq.Name)
			}
			if owner != "" && (owner != seq.OwnerTable || column != seq.OwnerColumn) {
				return fmt.Errorf("existing sequence %s belongs to %s.%s, not the dump owner", seq.Name, owner, column)
			}
			continue
		}
		if seq.OwnerTable != "" && !selectedTable(create, seq.OwnerTable) {
			return fmt.Errorf("sequence %s is missing and its owner %s is not being created", seq.Name, seq.OwnerTable)
		}
		cycle := "NO CYCLE"
		if seq.Cycle {
			cycle = "CYCLE"
		}
		query := fmt.Sprintf("CREATE SEQUENCE %s AS %s INCREMENT BY %d MINVALUE %d MAXVALUE %d START WITH %d CACHE %d %s", quoteIdentifier(seq.Name), seq.Type, seq.Increment, seq.Minimum, seq.Maximum, seq.Start, seq.Cache, cycle)
		if _, err := tx.Exec(ctx, query); err != nil {
			return fmt.Errorf("create sequence %s: %w", seq.Name, err)
		}
	}
	for _, table := range schema.Tables {
		if _, ok := create[table.Name]; !ok {
			continue
		}
		var cols []string
		for _, col := range table.Columns {
			part := quoteIdentifier(col.Name) + " " + col.Type
			if col.Collation != nil {
				part += " COLLATE " + targetCollation(*col.Collation, sourceSchema, targetSchema)
			}
			if col.Generated != "" {
				part += " GENERATED ALWAYS AS (" + col.Generated + ") STORED"
			}
			if col.Identity != "" {
				mode := "BY DEFAULT"
				if col.Identity == "a" {
					mode = "ALWAYS"
				}
				part += " GENERATED " + mode + " AS IDENTITY"
				if col.IdentitySequence != "" {
					seq, ok := sequenceByName[col.IdentitySequence]
					if !ok {
						return fmt.Errorf("identity sequence %s not found in dump", col.IdentitySequence)
					}
					cycle := "NO CYCLE"
					if seq.Cycle {
						cycle = "CYCLE"
					}
					part += fmt.Sprintf(" (SEQUENCE NAME %s START WITH %d INCREMENT BY %d MINVALUE %d MAXVALUE %d CACHE %d %s)", quoteIdentifier(col.IdentitySequence), seq.Start, seq.Increment, seq.Minimum, seq.Maximum, seq.Cache, cycle)
				}
			}
			if col.Default != "" && col.Generated == "" && col.Identity == "" {
				part += " DEFAULT " + col.Default
			}
			if col.NotNull {
				part += " NOT NULL"
			}
			cols = append(cols, part)
		}
		kind := "CREATE TABLE "
		if table.Unlogged {
			kind = "CREATE UNLOGGED TABLE "
		}
		if _, err := tx.Exec(ctx, kind+quoteIdentifier(table.Name)+" ("+strings.Join(cols, ", ")+")"); err != nil {
			return fmt.Errorf("create table %s: %w", table.Name, err)
		}
	}
	for _, seq := range schema.Sequences {
		if _, ok := create[seq.OwnerTable]; !ok || seq.Identity || seq.OwnerColumn == "" {
			continue
		}
		query := "ALTER SEQUENCE " + quoteIdentifier(seq.Name) + " OWNED BY " + quoteIdentifier(seq.OwnerTable, seq.OwnerColumn)
		if _, err := tx.Exec(ctx, query); err != nil {
			return fmt.Errorf("own sequence %s: %w", seq.Name, err)
		}
	}
	return nil
}

func orderNeededDomains(schema dump.Schema, needed map[string]struct{}) ([]dump.DomainDefinition, error) {
	domains := make(map[string]dump.DomainDefinition, len(schema.Domains))
	for _, domain := range schema.Domains {
		domains[domain.Name] = domain
	}
	enums := make(map[string]struct{}, len(schema.Enums))
	for _, enum := range schema.Enums {
		enums[enum.Name] = struct{}{}
	}
	var ordered []dump.DomainDefinition
	state := make(map[string]uint8)
	var visit func(string) error
	visit = func(name string) error {
		if state[name] == 2 {
			return nil
		}
		if state[name] == 1 {
			return fmt.Errorf("cyclic domain dependency at %s", name)
		}
		domain, isDomain := domains[name]
		if !isDomain {
			if _, isEnum := enums[name]; isEnum {
				needed[name] = struct{}{}
				return nil
			}
			return fmt.Errorf("custom type %s is missing from dump schema", name)
		}
		state[name] = 1
		needed[name] = struct{}{}
		if domain.BaseCustomType != "" {
			if err := visit(domain.BaseCustomType); err != nil {
				return fmt.Errorf("domain %s: %w", name, err)
			}
		}
		state[name] = 2
		ordered = append(ordered, domain)
		return nil
	}
	for _, domain := range schema.Domains {
		if _, ok := needed[domain.Name]; ok {
			if err := visit(domain.Name); err != nil {
				return nil, err
			}
		}
	}
	for name := range needed {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func finishCreated(ctx context.Context, tx pgx.Tx, targetSchema string, schema dump.Schema, create map[string]struct{}) error {
	// Foreign keys may reference any created table, including later tables or
	// cycles. Every supporting key/index must exist before the first FK.
	for _, foreignKeys := range []bool{false, true} {
		for _, table := range schema.Tables {
			if !selectedTable(create, table.Name) {
				continue
			}
			for _, constraint := range table.Constraints {
				if (constraint.Kind == "f") != foreignKeys {
					continue
				}
				query := "ALTER TABLE " + quoteIdentifier(targetSchema, table.Name) + " ADD CONSTRAINT " + quoteIdentifier(constraint.Name) + " " + constraint.Definition
				if _, err := tx.Exec(ctx, query); err != nil {
					return fmt.Errorf("create constraint %s.%s: %w", table.Name, constraint.Name, err)
				}
			}
			if foreignKeys {
				continue
			}
			for _, index := range table.Indexes {
				definition, err := indexForTarget(index.Definition, targetSchema, table.Name)
				if err != nil {
					return fmt.Errorf("create index %s: %w", index.Name, err)
				}
				if _, err := tx.Exec(ctx, definition); err != nil {
					return fmt.Errorf("create index %s: %w", index.Name, err)
				}
			}
		}
	}
	return nil
}

func indexForTarget(definition, targetSchema, table string) (string, error) {
	on := sqlKeywordOutsideQuotes(definition, "ON", 0)
	if on < 0 {
		return "", fmt.Errorf("unsupported index definition")
	}
	using := sqlKeywordOutsideQuotes(definition, "USING", on+len("ON"))
	if using < 0 {
		return "", fmt.Errorf("unsupported index definition")
	}
	// pg_get_indexdef always qualifies the source table, even with search_path
	// set. Replace only its ON target, leaving index keys and predicates intact.
	return definition[:on+len("ON")] + " " + quoteIdentifier(targetSchema, table) + " " + definition[using:], nil
}

func sqlKeywordOutsideQuotes(sql, keyword string, start int) int {
	quoted := false
	for i := 0; i < len(sql); i++ {
		if sql[i] == '"' {
			if quoted && i+1 < len(sql) && sql[i+1] == '"' {
				i++
			} else {
				quoted = !quoted
			}
			continue
		}
		if quoted || i < start || !strings.HasPrefix(sql[i:], keyword) {
			continue
		}
		end := i + len(keyword)
		if (i == 0 || sql[i-1] == ' ' || sql[i-1] == '\t' || sql[i-1] == '\n') &&
			(end == len(sql) || sql[end] == ' ' || sql[end] == '\t' || sql[end] == '\n') {
			return i
		}
	}
	return -1
}

func quoteLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
