package cli

import "fmt"

// RestorePreview keeps every table visible before the destructive confirmation.
func (u *UI) RestorePreview(target, source string, copyBytes int64, replace, create, skipped []string) {
	u.Section("Restore preview")
	fmt.Fprintf(u.out, "Target: %s\nSource: %s\nData: %s\n", target, source, formatBytes(copyBytes))
	u.tableGroup("Replace", replace)
	u.tableGroup("Create", create)
	u.tableGroup("Skip", skipped)
	fmt.Fprintln(u.out)
}

func (u *UI) tableGroup(label string, tables []string) {
	if len(tables) == 0 {
		return
	}
	fmt.Fprintf(u.out, "\n%s (%d):\n", label, len(tables))
	for _, table := range tables {
		fmt.Fprintf(u.out, "  - %s\n", table)
	}
}
