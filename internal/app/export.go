package app

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"

	"github.com/nightio/pg-pull/internal/cli"
	"github.com/nightio/pg-pull/internal/database"
	"github.com/nightio/pg-pull/internal/dump"
)

type preparedTable struct {
	entry   dump.Table
	columns []string
}

type exportEvent struct {
	index int
	bytes int64
	entry dump.Table
	kind  exportEventKind
}

type exportEventKind uint8

const (
	exportStarted exportEventKind = iota
	exportProgress
	exportFinished
	exportFailed
)

func (a *App) exportTables(ctx context.Context, source *database.Source, coordinator *database.Snapshot, dir, schema string, tables []preparedTable, sizes map[string]int64) ([]dump.Table, error) {
	if len(tables) == 0 {
		return nil, fmt.Errorf("no tables to export")
	}
	parallelism := min(source.Parallelism(), len(tables))
	snapshots := make([]*database.Snapshot, 1, parallelism)
	snapshots[0] = coordinator
	if parallelism > 1 {
		id, err := coordinator.ExportID(ctx)
		if err != nil {
			return nil, fmt.Errorf("export source snapshot: %w", err)
		}
		// The exporting transaction stays open until every worker has finished.
		// Importing must precede a worker's first query to preserve one view.
		for i := 1; i < parallelism; i++ {
			worker, err := source.BeginImportedSnapshot(ctx, i-1, id)
			if err != nil {
				for _, opened := range snapshots[1:] {
					_ = opened.Rollback(context.Background())
				}
				return nil, fmt.Errorf("start source worker %d: %w", i+1, err)
			}
			snapshots = append(snapshots, worker)
		}
	}
	defer func() {
		for _, worker := range snapshots[1:] {
			_ = worker.Rollback(context.Background())
		}
	}()

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int, len(tables))
	for index := range tables {
		jobs <- index
	}
	close(jobs)
	events := make(chan exportEvent, 256)
	var group sync.WaitGroup
	var failed sync.Once
	var firstErr error
	for _, snapshot := range snapshots {
		group.Add(1)
		go func(snapshot *database.Snapshot) {
			defer group.Done()
			for index := range jobs {
				if workCtx.Err() != nil {
					return
				}
				table := tables[index]
				events <- exportEvent{index: index, kind: exportStarted}
				var rowCount int64
				stats, err := dump.WriteCompressed(filepath.Join(dir, filepath.FromSlash(table.entry.File)), func(bytes int64) {
					select {
					case events <- exportEvent{index: index, kind: exportProgress, bytes: bytes}:
					default: // Rendered progress may lag behind COPY without slowing it.
					}
				}, func(writer io.Writer) error {
					var copyErr error
					rowCount, copyErr = snapshot.CopyTable(workCtx, schema, table.entry.Name, table.columns, writer)
					return copyErr
				})
				if err != nil {
					failed.Do(func() {
						firstErr = fmt.Errorf("export table %s: %w", table.entry.Name, err)
						cancel()
					})
					events <- exportEvent{index: index, kind: exportFailed}
					return
				}
				entry := table.entry
				entry.RowCount = rowCount
				entry.CompressedBytes = stats.CompressedBytes
				entry.UncompressedBytes = stats.UncompressedBytes
				entry.SHA256 = stats.SHA256
				events <- exportEvent{index: index, kind: exportFinished, bytes: stats.UncompressedBytes, entry: entry}
			}
		}(snapshot)
	}
	go func() {
		group.Wait()
		close(events)
	}()

	result := make([]dump.Table, len(tables))
	trackers := make(map[int]*cli.Progress, parallelism)
	for event := range events {
		switch event.kind {
		case exportStarted:
			trackers[event.index] = a.UI.NewProgress("Exporting", tables[event.index].entry.Name, event.index+1, len(tables), sizes[tables[event.index].entry.Name])
		case exportProgress:
			trackers[event.index].Update(event.bytes)
		case exportFinished:
			trackers[event.index].Finish(event.bytes)
			result[event.index] = event.entry
		case exportFailed:
			trackers[event.index].Fail()
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if err := workCtx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
