package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nightio/pg-pull/internal/cli"
	"github.com/nightio/pg-pull/internal/config"
	"github.com/nightio/pg-pull/internal/database"
	"github.com/nightio/pg-pull/internal/dump"
	"github.com/nightio/pg-pull/internal/update"
)

type App struct {
	UI         *cli.UI
	Version    string
	RawVersion string
}

func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) > 0 && args[0] == "init" {
		return a.runInit(args[1:])
	}
	if len(args) > 0 && args[0] == "self-update" {
		return a.runSelfUpdate(ctx, args[1:])
	}

	flags := flag.NewFlagSet("pg-pull", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var configPath string
	var showVersion bool
	flags.StringVar(&configPath, "config", "", "path to config.yaml")
	flags.StringVar(&configPath, "c", "", "path to config.yaml")
	flags.BoolVar(&showVersion, "version", false, "print version")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			a.printHelp()
			return 0
		}
		a.UI.Error(database.UserMessage(err))
		return 2
	}
	if flags.NArg() != 0 {
		a.UI.Error("unknown command or argument: " + strings.Join(flags.Args(), " "))
		return 2
	}
	if showVersion {
		a.UI.Println("pg-pull " + a.Version)
		return 0
	}

	cfg, err := config.Load(configPath)
	if errors.Is(err, config.ErrNotFound) {
		return a.offerInit(err)
	}
	if err != nil {
		a.UI.Error(database.UserMessage(err))
		return 1
	}

	entries, legacy, err := dump.List(cfg.DumpBaseDir)
	if err != nil {
		a.UI.Error("inspect dump directory: " + database.UserMessage(err))
		return 1
	}
	if legacy > 0 {
		a.UI.Warning(fmt.Sprintf("found %d legacy dump.sql director%s; the Go version leaves them untouched but cannot replay them", legacy, plural(legacy, "y", "ies")))
	}
	choices := []string{"Create dump"}
	if cfg.Target != nil {
		choices = append(choices, "Create dump and restore")
		if len(entries) > 0 {
			choices = append(choices, "Restore existing dump")
		}
	}
	choice, err := a.UI.Choice("What would you like to do?", choices, 0)
	if err != nil {
		a.UI.Error(database.UserMessage(err))
		return 1
	}
	if choice == 0 {
		return a.fresh(ctx, cfg, false)
	}
	if choice == 1 {
		return a.fresh(ctx, cfg, true)
	}
	labels := make([]string, 0, len(entries))
	for _, entry := range entries {
		labels = append(labels, dumpChoiceLabel(entry.Manifest, time.Local))
	}
	selected, err := a.UI.Choice("Choose dump to restore", labels, 0)
	if err != nil {
		a.UI.Error(database.UserMessage(err))
		return 1
	}
	return a.reuse(ctx, cfg, entries[selected])
}

func dumpChoiceLabel(manifest dump.Manifest, location *time.Location) string {
	return fmt.Sprintf("%s — %s — %d table(s) — modules: %s",
		manifest.CreatedAt.In(location).Format("02 Jan 2006, 15:04:05"),
		manifest.Source,
		len(manifest.Tables),
		strings.Join(manifest.Modules, ", "),
	)
}

func (a *App) printHelp() {
	a.UI.Println("pg-pull creates reusable PostgreSQL table dumps and can restore them locally.")
	a.UI.Println("")
	a.UI.Println("Usage:")
	a.UI.Println("  pg-pull [--config PATH]")
	a.UI.Println("  pg-pull init")
	a.UI.Println("  pg-pull self-update [--check]")
	a.UI.Println("  pg-pull --version")
}

func (a *App) runSelfUpdate(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("pg-pull self-update", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	checkOnly := flags.Bool("check", false, "check for a newer release without installing")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			a.UI.Println("Usage: pg-pull self-update [--check]")
			return 0
		}
		a.UI.Error(err.Error())
		return 2
	}
	if flags.NArg() != 0 {
		a.UI.Error("pg-pull self-update does not accept positional arguments")
		return 2
	}
	updater := update.New()
	var downloadProgress *cli.Progress
	var downloaded int64
	updater.OnDownloadProgress = func(bytes, total int64) {
		downloaded = bytes
		if downloadProgress == nil {
			downloadProgress = a.UI.NewProgress("Downloading", "pg-pull", 1, 1, total)
			return
		}
		downloadProgress.Update(bytes)
	}
	result, err := updater.Run(ctx, a.RawVersion, *checkOnly)
	if err != nil {
		if downloadProgress != nil {
			downloadProgress.Fail()
		}
		a.UI.Error(err.Error())
		return 1
	}
	if downloadProgress != nil {
		downloadProgress.Finish(downloaded)
	}
	if !result.Available {
		a.UI.Println("pg-pull is up to date (" + result.Current + ").")
		return 0
	}
	if *checkOnly {
		a.UI.Println("Update available: " + result.Current + " → " + result.Latest)
		return 0
	}
	if result.Scheduled {
		a.UI.Println("Downloaded " + result.Latest + "; Windows will finish the update after pg-pull exits.")
		return 0
	}
	a.UI.Success("Updated pg-pull " + result.Current + " → " + result.Latest)
	return 0
}

func (a *App) runInit(args []string) int {
	if len(args) != 0 {
		a.UI.Error("pg-pull init does not accept arguments")
		return 2
	}
	dir, err := os.Getwd()
	if err != nil {
		a.UI.Error(database.UserMessage(err))
		return 1
	}
	result, err := config.Init(dir)
	if err != nil {
		a.UI.Error(database.UserMessage(err))
		return 1
	}
	if result.CreatedIgnore {
		a.UI.Success("Created " + result.IgnorePath)
	}
	if result.CreatedConfig {
		a.UI.Success("Created " + result.ConfigPath)
		a.UI.Println("Edit it, then run pg-pull.")
	} else {
		a.UI.Println("Existing config.yaml was left unchanged.")
	}
	return 0
}

func (a *App) offerInit(loadErr error) int {
	a.UI.Warning(loadErr.Error())
	if !a.UI.Interactive() {
		a.UI.Println("Run pg-pull init to create a starter config, then edit it and re-run.")
		return 1
	}
	create, err := a.UI.Confirm("Create a starter config in the current directory now?", true)
	if err != nil || !create {
		return 1
	}
	return a.runInit(nil)
}

func (a *App) fresh(ctx context.Context, cfg *config.Config, withRestore bool) int {
	var target *database.Target
	if withRestore {
		var err error
		target, err = database.OpenTarget(ctx, *cfg.Target)
		if err != nil {
			a.UI.Error(database.UserMessage(err))
			return 1
		}
		defer target.Close(context.Background()) //nolint:errcheck
	}
	sourceLabels := append([]string(nil), cfg.SourceOrder...)
	selectedSource, err := a.UI.Choice("Choose source database", sourceLabels, 0)
	if err != nil {
		if cli.IsAbort(err) {
			a.UI.Warning("aborted")
			return 1
		}
		a.UI.Error(database.UserMessage(err))
		return 1
	}
	sourceName := cfg.SourceOrder[selectedSource]
	sourceConfig := cfg.Sources[sourceName]
	password, err := a.UI.Password(fmt.Sprintf("Password for %s@%s:%d/%s", sourceConfig.User, sourceConfig.Host, sourceConfig.Port, sourceConfig.DBName))
	if err != nil {
		if cli.IsAbort(err) {
			a.UI.Warning("aborted")
			return 1
		}
		a.UI.Error("read password: " + database.UserMessage(err))
		return 1
	}
	source, err := database.OpenSource(ctx, sourceConfig, password, cfg.ParallelExports)
	password = ""
	if err != nil {
		a.UI.Error(database.UserMessage(err))
		return 1
	}
	defer source.Close(context.Background()) //nolint:errcheck

	moduleLabels := make([]string, 0, len(cfg.ModuleOrder))
	for _, name := range cfg.ModuleOrder {
		label := name
		if description := cfg.Modules[name].Description; description != "" {
			label += " — " + description
		}
		moduleLabels = append(moduleLabels, label)
	}
	selectedIndexes, err := a.UI.MultiChoice("Which module(s) should be copied?", moduleLabels, 0)
	if err != nil {
		if cli.IsAbort(err) {
			a.UI.Warning("aborted")
			return 1
		}
		a.UI.Error(database.UserMessage(err))
		return 1
	}
	modules := make([]string, 0, len(selectedIndexes))
	patterns := make([]string, 0)
	seenPatterns := make(map[string]struct{})
	for _, index := range selectedIndexes {
		name := cfg.ModuleOrder[index]
		modules = append(modules, name)
		for _, pattern := range cfg.Modules[name].Patterns {
			if _, exists := seenPatterns[pattern]; !exists {
				seenPatterns[pattern] = struct{}{}
				patterns = append(patterns, pattern)
			}
		}
	}
	snapshot, err := source.BeginSnapshot(ctx)
	if err != nil {
		a.UI.Error("begin source snapshot: " + database.UserMessage(err))
		return 1
	}
	defer snapshot.Rollback(context.Background()) //nolint:errcheck
	tables, err := snapshot.TablesForPatterns(ctx, sourceConfig.Schema, patterns)
	if err != nil {
		a.UI.Error("resolve selected tables: " + database.UserMessage(err))
		return 1
	}
	if len(tables) == 0 {
		a.UI.Warning(fmt.Sprintf("no matching tables found in source schema %q", sourceConfig.Schema))
		return 1
	}
	sequenceRefs, err := snapshot.SequenceRefs(ctx, sourceConfig.Schema, tables)
	if err != nil {
		a.UI.Error("resolve source sequences: " + database.UserMessage(err))
		return 1
	}
	schema, err := snapshot.CaptureSchema(ctx, sourceConfig.Schema, tables, sequenceRefs)
	if err != nil {
		a.UI.Error("capture source schema: " + database.UserMessage(err))
		return 1
	}
	var plan restorePlan
	if withRestore {
		plan, err = a.planRestore(ctx, target, schema, tables)
		if err != nil {
			a.UI.Error(database.UserMessage(err))
			return 1
		}
	}

	dir, err := dump.CreateDir(cfg.DumpBaseDir, time.Now())
	if err != nil {
		a.UI.Error(database.UserMessage(err))
		return 1
	}
	exportStart := time.Now()
	manifest, sequences, err := a.export(ctx, source, snapshot, dir, sourceName, sourceConfig.Schema, modules, tables, sequenceRefs)
	if err != nil {
		a.UI.Error("export failed: " + database.UserMessage(err))
		if dump.HasData(dir) {
			a.UI.Warning("partial dump preserved for inspection: " + dir)
		} else {
			_ = dump.RemoveIfEmpty(dir)
		}
		return 1
	}
	if err := dump.WriteSchema(dir, schema); err != nil {
		a.UI.Error("write schema metadata: " + database.UserMessage(err))
		return 1
	}
	if err := dump.WriteSequences(dir, sequences); err != nil {
		a.UI.Error("write sequence metadata: " + database.UserMessage(err))
		return 1
	}
	if err := dump.WriteManifest(dir, manifest); err != nil {
		a.UI.Error("write dump manifest: " + database.UserMessage(err))
		return 1
	}
	manifest, sequences, err = dump.Load(dir, true)
	if err != nil {
		a.UI.Error("validate completed dump: " + database.UserMessage(err))
		return 1
	}
	copyBytes, gzipBytes := tableTotals(manifest.Tables, nil)
	a.UI.ExportSummary(len(manifest.Tables), time.Since(exportStart), copyBytes, gzipBytes)
	if !withRestore {
		a.UI.Success("Dump preserved at " + dir)
		return 0
	}
	if len(plan.selected) == 0 {
		a.UI.Warning("No local tables selected for restore; dump preserved at " + dir)
		return 0
	}
	if err := a.confirmRestore(target, manifest, plan); err != nil {
		if cli.IsAbort(err) {
			a.UI.Warning("aborted")
			return 1
		}
		a.UI.Warning("Restore skipped; dump preserved at " + dir)
		return 0
	}
	if err := a.restore(ctx, target, dir, manifest, sequences, schema, plan); err != nil {
		a.UI.Error("restore failed: " + database.UserMessage(err))
		a.UI.Warning("completed dump preserved at " + dir)
		return 1
	}
	a.UI.Success(fmt.Sprintf("Copied %d table(s) from %q into local", len(plan.selected), sourceName))
	a.UI.Println("Dump preserved at " + dir)
	return 0
}

func (a *App) export(
	ctx context.Context,
	source *database.Source,
	snapshot *database.Snapshot,
	dir, sourceName, sourceSchema string,
	modules, tables []string,
	sequenceRefs []database.SequenceRef,
) (dump.Manifest, []dump.Sequence, error) {
	defer snapshot.Rollback(context.Background()) // safe after commit; closes failed exports too
	version, err := source.ServerVersion(ctx)
	if err != nil {
		return dump.Manifest{}, nil, err
	}
	sizes, err := snapshot.RelationSizes(ctx, sourceSchema, tables)
	if err != nil {
		sizes = make(map[string]int64)
	}

	manifest := dump.Manifest{
		CreatedAt:           time.Now(),
		Source:              sourceName,
		SourceServerVersion: version,
		SourceSchema:        sourceSchema,
		Modules:             append([]string(nil), modules...),
		CopySettings: map[string]string{
			"format": "text", "compression": "gzip", "client_encoding": "UTF8",
			"DateStyle": "ISO", "IntervalStyle": "postgres", "extra_float_digits": "3",
		},
	}

	prepared := make([]preparedTable, 0, len(tables))
	for index, tableName := range tables {
		sourceColumns, err := snapshot.Columns(ctx, sourceSchema, tableName)
		if err != nil {
			return dump.Manifest{}, nil, fmt.Errorf("inspect source table %s: %w", tableName, err)
		}
		entry := dump.Table{Name: tableName, File: filepath.ToSlash(filepath.Join("tables", fmt.Sprintf("%04d.copy.gz", index+1)))}
		copyColumns := make([]string, 0, len(sourceColumns))
		for _, column := range sourceColumns {
			if column.Generated {
				continue
			}
			copyColumns = append(copyColumns, column.Name)
			entry.Columns = append(entry.Columns, dump.Column{Name: column.Name, Type: column.Type})
		}
		prepared = append(prepared, preparedTable{entry: entry, columns: copyColumns})
	}
	a.UI.Section(fmt.Sprintf("Exporting %d table(s) to %s", len(tables), dir))
	manifest.Tables, err = a.exportTables(ctx, source, snapshot, dir, sourceSchema, prepared, sizes)
	if err != nil {
		return dump.Manifest{}, nil, err
	}

	sequences := make([]dump.Sequence, 0, len(sequenceRefs))
	for _, ref := range sequenceRefs {
		lastValue, isCalled, err := snapshot.SequenceState(ctx, sourceSchema, ref.Name)
		if err != nil {
			return dump.Manifest{}, nil, fmt.Errorf("read source sequence %s: %w", ref.Name, err)
		}
		sequences = append(sequences, dump.Sequence{Name: ref.Name, LastValue: lastValue, IsCalled: isCalled})
	}
	if err := snapshot.Commit(ctx); err != nil {
		return dump.Manifest{}, nil, fmt.Errorf("finish source snapshot: %w", err)
	}
	return manifest, sequences, nil
}

type restorePlan struct {
	selected map[string]struct{}
	create   map[string]struct{}
	skipped  []string
}

func (a *App) planRestore(ctx context.Context, target *database.Target, schema dump.Schema, tables []string) (restorePlan, error) {
	plan := restorePlan{selected: make(map[string]struct{}), create: make(map[string]struct{})}
	local, err := target.LocalTables(ctx)
	if err != nil {
		return plan, fmt.Errorf("inspect local tables: %w", err)
	}
	var missing []string
	for _, name := range tables {
		if _, ok := local[name]; !ok {
			missing = append(missing, name)
			continue
		}
		columns, err := target.Columns(ctx, name)
		if err != nil {
			return plan, err
		}
		var candidate dump.Table
		candidate.Name = name
		for _, def := range schema.Tables {
			if def.Name == name {
				for _, col := range def.Columns {
					if col.Generated == "" {
						candidate.Columns = append(candidate.Columns, dump.Column{Name: col.Name, Type: col.Type})
					}
				}
				break
			}
		}
		if err := database.VerifyTargetColumns(candidate, columns); err != nil {
			return plan, err
		}
		plan.selected[name] = struct{}{}
	}
	if len(missing) > 0 {
		choice, err := a.UI.Choice("Missing local tables: "+strings.Join(missing, ", ")+". What should happen?", []string{"Skip missing tables", "Abort", "Create missing tables"}, 0)
		if err != nil {
			return plan, err
		}
		switch choice {
		case 0:
			plan.skipped = missing
		case 1:
			return plan, fmt.Errorf("restore aborted")
		case 2:
			for _, def := range schema.Tables {
				if contains(missing, def.Name) && len(def.Unsupported) > 0 {
					return plan, fmt.Errorf("cannot create %s: unsupported source features: %s", def.Name, strings.Join(def.Unsupported, ", "))
				}
			}
			for _, name := range missing {
				plan.selected[name] = struct{}{}
				plan.create[name] = struct{}{}
			}
		}
	}
	return plan, nil
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func (a *App) confirmRestore(target *database.Target, manifest dump.Manifest, plan restorePlan) error {
	var replaceNames, createNames []string
	for _, table := range manifest.Tables {
		if _, ok := plan.selected[table.Name]; !ok {
			continue
		}
		if _, ok := plan.create[table.Name]; ok {
			createNames = append(createNames, table.Name)
		} else {
			replaceNames = append(replaceNames, table.Name)
		}
	}
	copyBytes, _ := tableTotals(manifest.Tables, plan.selected)
	a.UI.RestorePreview(target.Description(), manifest.Source, copyBytes, replaceNames, createNames, plan.skipped)
	confirmed, err := a.UI.Confirm("Replace data in the selected local tables?", false)
	if err != nil {
		return err
	}
	if !confirmed {
		return fmt.Errorf("restore declined")
	}
	return nil
}

func (a *App) reuse(ctx context.Context, cfg *config.Config, entry dump.Entry) int {
	target, err := database.OpenTarget(ctx, *cfg.Target)
	if err != nil {
		a.UI.Error(database.UserMessage(err))
		return 1
	}
	defer target.Close(context.Background()) //nolint:errcheck
	manifest, sequences, err := dump.Load(entry.Path, true)
	if err != nil {
		a.UI.Error("validate dump: " + database.UserMessage(err))
		return 1
	}
	schema, err := dump.LoadSchema(entry.Path)
	if err != nil {
		a.UI.Error("read schema: " + database.UserMessage(err))
		return 1
	}
	names := make([]string, 0, len(manifest.Tables))
	for _, table := range manifest.Tables {
		names = append(names, table.Name)
	}
	plan, err := a.planRestore(ctx, target, schema, names)
	if err != nil {
		a.UI.Error(database.UserMessage(err))
		return 1
	}
	if len(plan.selected) == 0 {
		a.UI.Warning("No local tables selected for restore")
		return 0
	}
	if err := a.confirmRestore(target, manifest, plan); err != nil {
		if cli.IsAbort(err) {
			a.UI.Warning("aborted")
			return 1
		}
		a.UI.Warning("Restore skipped")
		return 0
	}
	if err := a.restore(ctx, target, entry.Path, manifest, sequences, schema, plan); err != nil {
		a.UI.Error("restore failed: " + database.UserMessage(err))
		return 1
	}
	a.UI.Success(fmt.Sprintf("Restored %d table(s) from %s", len(plan.selected), entry.Path))
	return 0
}

func (a *App) restore(ctx context.Context, target *database.Target, dir string, manifest dump.Manifest, sequences []dump.Sequence, schema dump.Schema, plan restorePlan) error {
	// Full checksum validation occurs before opening the destructive transaction.
	if _, _, err := dump.Load(dir, true); err != nil {
		return err
	}
	a.UI.Section(fmt.Sprintf("Restoring %d table(s) atomically", len(plan.selected)))
	restoreStart := time.Now()
	indexes := make(map[string]int)
	position := 0
	for _, table := range manifest.Tables {
		if _, ok := plan.selected[table.Name]; ok {
			position++
			indexes[table.Name] = position
		}
	}
	trackers := make(map[string]*cli.Progress)
	err := target.Restore(ctx, dir, manifest, sequences, plan.selected, schema, plan.create, func(table string, read, total int64) {
		tracker := trackers[table]
		if tracker == nil {
			tracker = a.UI.NewProgress("Restoring", table, indexes[table], len(plan.selected), total)
			trackers[table] = tracker
		}
		if read >= total {
			tracker.Finish(read)
		} else {
			tracker.Update(read)
		}
	})
	if err != nil {
		for _, tracker := range trackers {
			tracker.Fail()
		}
		return err
	}
	copyBytes, _ := tableTotals(manifest.Tables, plan.selected)
	a.UI.RestoreSummary(len(plan.selected), time.Since(restoreStart), copyBytes)
	return nil
}

func tableTotals(tables []dump.Table, selected map[string]struct{}) (copyBytes, gzipBytes int64) {
	for _, table := range tables {
		if selected != nil {
			if _, ok := selected[table.Name]; !ok {
				continue
			}
		}
		copyBytes += table.UncompressedBytes
		gzipBytes += table.CompressedBytes
	}
	return copyBytes, gzipBytes
}

func plural(value int, singular, plural string) string {
	if value == 1 {
		return singular
	}
	return plural
}
