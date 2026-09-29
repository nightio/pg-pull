package cli

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"golang.org/x/term"
)

type Progress struct {
	u         *UI
	phase     string
	table     string
	index     int
	total     int
	estimate  int64
	bytes     int64
	start     time.Time
	last      time.Time
	inline    bool
	completed bool
}

type progressState uint8

const (
	progressActive progressState = iota
	progressComplete
	progressFailed
)

func (u *UI) NewProgress(phase, table string, index, total int, estimate int64) *Progress {
	p := &Progress{
		u: u, phase: phase, table: table, index: index, total: total,
		estimate: estimate, start: time.Now(), inline: u.progressInline(),
	}
	p.Update(0)
	return p
}

func (u *UI) progressInline() bool {
	return u.forms && !u.accessible
}

func (p *Progress) Update(bytes int64) {
	p.bytes = bytes
	now := time.Now()
	if !p.last.IsZero() && now.Sub(p.last) < 200*time.Millisecond {
		return
	}
	p.last = now
	if !p.inline {
		if bytes == 0 {
			fmt.Fprintf(p.u.out, "%s %s [%d/%d] started\n", p.phase, p.table, p.index, p.total)
		}
		return
	}
	p.draw(progressActive)
}

func (p *Progress) Finish(bytes int64) {
	if p.completed {
		return
	}
	p.completed = true
	p.bytes = bytes
	// pg_relation_size is only an estimate of COPY output. The final count is
	// authoritative, so the completed display never stops at a false percentage.
	p.estimate = bytes
	if p.inline {
		p.draw(progressComplete)
		fmt.Fprintln(p.u.out)
		return
	}
	fmt.Fprintf(p.u.out, "%s %s [%d/%d] complete (%s, %s)\n",
		p.phase, p.table, p.index, p.total, formatBytes(bytes), time.Since(p.start).Round(time.Second))
}

func (p *Progress) Fail() {
	if p.completed {
		return
	}
	p.completed = true
	if p.inline {
		p.draw(progressFailed)
		fmt.Fprintln(p.u.out)
	}
}

func (p *Progress) draw(state progressState) {
	width, _, err := term.GetSize(int(p.u.outFile.Fd()))
	if err != nil {
		width = 80
	}
	fmt.Fprintf(p.u.out, "\r\x1b[2K%s", p.render(width, state, time.Since(p.start)))
}

func (p *Progress) render(width int, state progressState, elapsed time.Duration) string {
	digits := len(fmt.Sprint(p.total))
	prefix := fmt.Sprintf("%-9s [%*d/%d]", p.phase, digits, p.index, p.total)
	status := " "
	if state == progressComplete {
		status = "✓"
	} else if state == progressFailed {
		status = "✗"
	}
	sizeText := formatBytes(p.bytes)
	if lipgloss.Width(sizeText) > 10 {
		sizeText = fitCell(sizeText, 10)
	}
	size := fmt.Sprintf("%10s", sizeText)
	// Reserve every optional column even when the size estimate is unavailable.
	// This keeps completed lines aligned while parallel COPY jobs finish in any order.
	const barWidth = 16
	fullFixed := lipgloss.Width(prefix) + 1 + 2 + barWidth + 2 + 10 + 2 + 4 + 2 + 9 + 2 + 1
	if width >= fullFixed+8 {
		nameWidth := min(40, width-fullFixed)
		percent := "--"
		if state == progressComplete {
			percent = "100%"
		} else if p.estimate > 0 {
			value := min(100, int(float64(p.bytes)/float64(p.estimate)*100))
			percent = fmt.Sprintf("%d%%", value)
		}
		return fmt.Sprintf("%s %s  %s  %s  %4s  %9s  %s",
			prefix, fitCell(p.table, nameWidth), p.bar(barWidth, state), size,
			percent, fitCell(elapsed.Round(time.Second).String(), 9), status)
	}
	compactFixed := lipgloss.Width(prefix) + 1 + 2 + 10 + 2 + 1
	nameWidth := max(1, min(40, width-compactFixed))
	return fmt.Sprintf("%s %s  %s  %s", prefix, fitCell(p.table, nameWidth), size, status)
}

func (p *Progress) bar(width int, state progressState) string {
	fraction := 0.0
	if state == progressComplete {
		fraction = 1
	} else if p.estimate > 0 {
		fraction = min(1, float64(p.bytes)/float64(p.estimate))
	}
	filled := max(0, int(fraction*float64(width)))
	bar := strings.Repeat("━", filled) + strings.Repeat("─", width-filled)
	if p.u.color {
		bar = lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Render(bar)
	}
	return bar
}

func fitCell(value string, width int) string {
	if lipgloss.Width(value) <= width {
		return value + strings.Repeat(" ", width-lipgloss.Width(value))
	}
	if width <= 1 {
		return "…"
	}
	var result strings.Builder
	used := 0
	for _, character := range value {
		characterWidth := lipgloss.Width(string(character))
		if used+characterWidth > width-1 {
			break
		}
		result.WriteRune(character)
		used += characterWidth
	}
	result.WriteRune('…')
	return result.String() + strings.Repeat(" ", width-used-1)
}

func formatBytes(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	size := float64(bytes) / 1024
	unit := 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	return fmt.Sprintf("%.1f %s", size, units[unit])
}

func (u *UI) ExportSummary(tables int, elapsed time.Duration, copyBytes, gzipBytes int64) {
	u.Success(fmt.Sprintf("Exported %d table(s) in %s — %s COPY, %s gzip",
		tables, elapsed.Round(time.Second), formatBytes(copyBytes), formatBytes(gzipBytes)))
}

func (u *UI) RestoreSummary(tables int, elapsed time.Duration, copyBytes int64) {
	u.Success(fmt.Sprintf("Restored %d table(s) in %s — %s COPY",
		tables, elapsed.Round(time.Second), formatBytes(copyBytes)))
}
