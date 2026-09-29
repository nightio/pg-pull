package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/creack/pty"
)

func plainUI(t *testing.T, input string) (*UI, *bytes.Buffer) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	if _, err := write.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	return New(context.Background(), read, &output, &output), &output
}

func TestPlainPromptDefaultsAndSelections(t *testing.T) {
	u, output := plainUI(t, "\n2\n\n2,1,2\n\n")
	choice, err := u.Choice("Source", []string{"alpha", "beta"}, 1)
	if err != nil || choice != 1 {
		t.Fatalf("default choice = %d, %v", choice, err)
	}
	choice, err = u.Choice("Dump", []string{"new", "reuse"}, 0)
	if err != nil || choice != 1 {
		t.Fatalf("selected choice = %d, %v", choice, err)
	}
	modules, err := u.MultiChoice("Modules", []string{"ALL", "Fees"}, 0)
	if err != nil || len(modules) != 1 || modules[0] != 0 {
		t.Fatalf("default modules = %v, %v", modules, err)
	}
	modules, err = u.MultiChoice("Modules", []string{"ALL", "Fees"}, 0)
	if err != nil || len(modules) != 2 || modules[0] != 1 || modules[1] != 0 {
		t.Fatalf("selected modules = %v, %v", modules, err)
	}
	confirmed, err := u.Confirm("Wipe local tables?", false)
	if err != nil || confirmed {
		t.Fatalf("destructive default = %t, %v", confirmed, err)
	}
	if strings.Contains(output.String(), "\x1b[") {
		t.Fatal("plain prompts contain ANSI escape sequences")
	}
}

func TestPlainPromptRejectsEmptyModuleSelection(t *testing.T) {
	u, _ := plainUI(t, ",\n")
	if _, err := u.MultiChoice("Modules", []string{"ALL"}, 0); err == nil {
		t.Fatal("empty module selection was accepted")
	}
}

func TestPlainPasswordIsNotEchoed(t *testing.T) {
	u, output := plainUI(t, "a-secret-value\n")
	password, err := u.Password("Source password")
	if err != nil || password != "a-secret-value" {
		t.Fatalf("password prompt failed: %v", err)
	}
	if strings.Contains(output.String(), password) {
		t.Fatal("password appeared in terminal output")
	}
}

func TestPlainProgressHasNoTerminalControls(t *testing.T) {
	u, output := plainUI(t, "")
	p := u.NewProgress("Exporting", "fees", 1, 2, 0)
	p.Update(2048)
	p.Finish(2048)
	text := output.String()
	if !strings.Contains(text, "Exporting fees [1/2] complete (2.0 KB") {
		t.Fatalf("missing completion: %q", text)
	}
	if strings.ContainsAny(text, "\r\x1b") {
		t.Fatalf("plain progress contains terminal controls: %q", text)
	}
}

func TestProgressColumnsStayAligned(t *testing.T) {
	u, _ := plainUI(t, "")
	for _, width := range []int{80, 110, 140} {
		short := &Progress{u: u, phase: "Exporting", table: "fees", index: 1, total: 35, estimate: 0, bytes: 0}
		long := &Progress{u: u, phase: "Exporting", table: "project_service_aggregated_with_a_very_long_name", index: 35, total: 35, estimate: 8192, bytes: 8192}
		first := short.render(width, progressActive, 3*time.Second)
		second := long.render(width, progressComplete, 12*time.Second)
		if lipgloss.Width(first) > width || lipgloss.Width(second) > width {
			t.Fatalf("width %d overflow: %q / %q", width, first, second)
		}
		barStart := strings.Index(first, "─")
		completeStart := strings.Index(second, "━")
		if barStart < 0 || completeStart < 0 || lipgloss.Width(first[:barStart]) != lipgloss.Width(second[:completeStart]) {
			t.Fatalf("width %d bars do not align: %q / %q", width, first, second)
		}
		if !strings.Contains(first, "--") || !strings.Contains(second, "100%") {
			t.Fatalf("width %d missing reserved percent field: %q / %q", width, first, second)
		}
	}
}

func TestProgressCompactLayoutAndWideNames(t *testing.T) {
	u, _ := plainUI(t, "")
	p := &Progress{u: u, phase: "Restoring", table: "漢字 project_service_very_long", index: 2, total: 35, bytes: 4096}
	for _, width := range []int{40, 60, 75} {
		line := p.render(width, progressComplete, time.Minute)
		if lipgloss.Width(line) > width || strings.ContainsAny(line, "━─") {
			t.Fatalf("invalid compact line at width %d: %q", width, line)
		}
	}
	if lipgloss.Width(fitCell("漢字", 3)) != 3 {
		t.Fatal("wide characters were not fitted to display columns")
	}
}

func TestPhaseSummaries(t *testing.T) {
	u, output := plainUI(t, "")
	u.ExportSummary(3, 83*time.Second, 8*1024, 2*1024)
	u.RestoreSummary(2, 14*time.Second, 6*1024)
	text := output.String()
	for _, want := range []string{
		"Exported 3 table(s) in 1m23s — 8.0 KB COPY, 2.0 KB gzip",
		"Restored 2 table(s) in 14s — 6.0 KB COPY",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("summary %q missing from %q", want, text)
		}
	}
}

func TestRestorePreviewGroupsTablesAndFormatsSize(t *testing.T) {
	u, output := plainUI(t, "")
	u.RestorePreview("local.public", "beta", 21_709_045,
		[]string{"project_project", "project_service"},
		[]string{"new_table"},
		[]string{"missing_table"})
	want := "\nRestore preview\nTarget: local.public\nSource: beta\nData: 20.7 MB\n\n" +
		"Replace (2):\n  - project_project\n  - project_service\n\n" +
		"Create (1):\n  - new_table\n\n" +
		"Skip (1):\n  - missing_table\n\n"
	if got := output.String(); got != want {
		t.Fatalf("preview = %q, want %q", got, want)
	}
}

func TestRestorePreviewOmitsEmptyGroups(t *testing.T) {
	u, output := plainUI(t, "")
	u.RestorePreview("local.public", "beta", 0, []string{"items"}, nil, nil)
	got := output.String()
	if !strings.Contains(got, "Data: 0 B\n\nReplace (1):\n  - items\n") || strings.Contains(got, "Create (") || strings.Contains(got, "Skip (") {
		t.Fatalf("unexpected preview: %q", got)
	}
}

func TestZeroSizeTerminalUsesPlainPrompts(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := pty.Setsize(master, &pty.Winsize{}); err != nil {
		t.Fatal(err)
	}
	u := New(context.Background(), slave, slave, slave)
	if u.forms || u.progressInline() {
		t.Fatal("zero-size terminal enabled redraws")
	}
}

func terminalUI(t *testing.T, title string) (*UI, *os.File, func() string) {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close(); _ = master.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	var mu sync.Mutex
	var output bytes.Buffer
	ready := make(chan struct{})
	go func() {
		data := make([]byte, 4096)
		for {
			n, err := master.Read(data)
			if n > 0 {
				mu.Lock()
				output.Write(data[:n])
				if strings.Contains(output.String(), title) {
					select {
					case <-ready:
					default:
						close(ready)
					}
				}
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return New(ctx, slave, slave, slave), master, func() string {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("terminal prompt did not appear")
		}
		mu.Lock()
		defer mu.Unlock()
		return output.String()
	}
}

func TestInteractiveDestructiveConfirmDefaultsToNo(t *testing.T) {
	u, master, waitForPrompt := terminalUI(t, "Wipe local")
	result := make(chan struct {
		confirmed bool
		err       error
	}, 1)
	go func() {
		confirmed, err := u.Confirm("Wipe local tables?", false)
		result <- struct {
			confirmed bool
			err       error
		}{confirmed, err}
	}()
	waitForPrompt()
	if _, err := master.Write([]byte("\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.confirmed {
			t.Fatalf("destructive confirmation = %t, %v", got.confirmed, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("confirmation did not finish")
	}
}

func TestInteractivePromptCanBeAborted(t *testing.T) {
	u, master, waitForPrompt := terminalUI(t, "Choose source")
	result := make(chan error, 1)
	go func() {
		_, err := u.Choice("Choose source", []string{"alpha", "beta"}, 0)
		result <- err
	}()
	waitForPrompt()
	if _, err := master.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, huh.ErrUserAborted) {
			t.Fatalf("abort returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("aborted prompt did not finish")
	}
}

func TestInteractiveModuleSelectionRequiresOne(t *testing.T) {
	u, master, waitForPrompt := terminalUI(t, "Which modules")
	type selectionResult struct {
		values []int
		err    error
	}
	result := make(chan selectionResult, 1)
	go func() {
		values, err := u.MultiChoice("Which modules?", []string{"ALL", "Fees"}, 0)
		result <- selectionResult{values, err}
	}()
	waitForPrompt()
	if _, err := master.Write([]byte(" \r")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		t.Fatalf("empty selection was accepted: %v, %v", got.values, got.err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := master.Write([]byte(" \r")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || len(got.values) != 1 || got.values[0] != 0 {
			t.Fatalf("module selection = %v, %v", got.values, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("module selection did not finish")
	}
}

func TestInteractivePasswordIsHidden(t *testing.T) {
	u, master, waitForPrompt := terminalUI(t, "Source password")
	type passwordResult struct {
		value string
		err   error
	}
	result := make(chan passwordResult, 1)
	go func() {
		value, err := u.Password("Source password")
		result <- passwordResult{value, err}
	}()
	waitForPrompt()
	const secret = "a-secret-value"
	if _, err := master.Write([]byte(secret + "\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.value != secret {
			t.Fatalf("password prompt failed: %v", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("password prompt did not finish")
	}
	if strings.Contains(waitForPrompt(), secret) {
		t.Fatal("password appeared in terminal output")
	}
}

func TestAccessiblePromptHasNoANSI(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	u, master, waitForPrompt := terminalUI(t, "Choose source")
	type choiceResult struct {
		value int
		err   error
	}
	result := make(chan choiceResult, 1)
	go func() {
		value, err := u.Choice("Choose source", []string{"alpha", "beta"}, 0)
		result <- choiceResult{value, err}
	}()
	waitForPrompt()
	if _, err := master.Write([]byte("\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.value != 0 {
			t.Fatalf("accessible choice = %d, %v", got.value, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accessible prompt did not finish")
	}
	if strings.Contains(waitForPrompt(), "\x1b[") {
		t.Fatal("accessible prompt contains ANSI escape sequences")
	}
}
