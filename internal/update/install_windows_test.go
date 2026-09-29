//go:build windows

package update

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsFinishReplacesAndRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name, stageContents, want string
	}{
		{name: "replace", stageContents: "new", want: "new"},
		{name: "rollback missing stage", want: "old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "pg-pull.exe")
			stage := filepath.Join(dir, "candidate")
			if err := os.WriteFile(target, []byte("old"), 0755); err != nil {
				t.Fatal(err)
			}
			if tc.stageContents != "" {
				if err := os.WriteFile(stage, []byte(tc.stageContents), 0755); err != nil {
					t.Fatal(err)
				}
			}
			err := finish(target, stage)
			if (err == nil) != (tc.stageContents != "") {
				t.Fatalf("finish error = %v", err)
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != tc.want {
				t.Fatalf("installed executable = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
