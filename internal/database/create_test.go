package database

import "testing"

func TestIndexForTargetWithQuotedKeywords(t *testing.T) {
	definition := `CREATE INDEX "lookup ""ON"" name" ON "source USING schema"."table ON name" USING btree (id)`
	got, err := indexForTarget(definition, "target", "table")
	if err != nil {
		t.Fatal(err)
	}
	want := `CREATE INDEX "lookup ""ON"" name" ON "target"."table" USING btree (id)`
	if got != want {
		t.Fatalf("index definition = %q, want %q", got, want)
	}
}
