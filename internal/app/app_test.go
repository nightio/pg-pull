package app

import (
	"testing"
	"time"

	"github.com/nightio/pg-pull/internal/dump"
)

func TestDumpChoiceLabel(t *testing.T) {
	manifest := dump.Manifest{
		CreatedAt: time.Date(2026, time.September, 26, 12, 23, 16, 0, time.UTC),
		Source:    "beta",
		Tables:    make([]dump.Table, 35),
		Modules:   []string{"ALL", "Fees"},
	}
	got := dumpChoiceLabel(manifest, time.FixedZone("CEST", 2*60*60))
	want := "26 Sep 2026, 14:23:16 — beta — 35 table(s) — modules: ALL, Fees"
	if got != want {
		t.Fatalf("dump label = %q, want %q", got, want)
	}
}

func TestTableTotalsRespectsRestoredSelection(t *testing.T) {
	tables := []dump.Table{
		{Name: "first", UncompressedBytes: 100, CompressedBytes: 25},
		{Name: "second", UncompressedBytes: 300, CompressedBytes: 75},
	}
	copyBytes, gzipBytes := tableTotals(tables, nil)
	if copyBytes != 400 || gzipBytes != 100 {
		t.Fatalf("export totals = %d, %d", copyBytes, gzipBytes)
	}
	copyBytes, gzipBytes = tableTotals(tables, map[string]struct{}{"second": {}})
	if copyBytes != 300 || gzipBytes != 75 {
		t.Fatalf("selected totals = %d, %d", copyBytes, gzipBytes)
	}
}
