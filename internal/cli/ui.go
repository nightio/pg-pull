package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"golang.org/x/term"
)

type UI struct {
	ctx         context.Context
	in          *os.File
	reader      *bufio.Reader
	out         io.Writer
	errOut      io.Writer
	interactive bool
	forms       bool
	accessible  bool
	color       bool
	outFile     *os.File
}

func New(ctx context.Context, in *os.File, out, errOut io.Writer) *UI {
	interactive := term.IsTerminal(int(in.Fd()))
	outFile, terminalOutput := out.(*os.File)
	terminalOutput = terminalOutput && term.IsTerminal(int(outFile.Fd()))
	renderable := false
	if terminalOutput {
		width, height, err := term.GetSize(int(outFile.Fd()))
		renderable = err == nil && width >= 40 && height >= 10
	}
	accessible := os.Getenv("ACCESSIBLE") != ""
	return &UI{
		ctx:         ctx,
		in:          in,
		reader:      bufio.NewReader(in),
		out:         out,
		errOut:      errOut,
		interactive: interactive,
		forms:       interactive && renderable && os.Getenv("TERM") != "dumb",
		accessible:  accessible,
		color:       terminalOutput && !accessible && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb",
		outFile:     outFile,
	}
}

func (u *UI) Interactive() bool { return u.interactive }

func IsAbort(err error) bool {
	return errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled)
}

func (u *UI) Printf(format string, args ...any) { fmt.Fprintf(u.out, format, args...) }
func (u *UI) Println(args ...any)               { fmt.Fprintln(u.out, args...) }

func (u *UI) Section(title string) {
	fmt.Fprintln(u.out)
	u.status(u.out, "▸", title, "#7D56F4")
}

func (u *UI) Success(message string) { u.status(u.out, "✓", message, "#04B575") }
func (u *UI) Warning(message string) { u.status(u.errOut, "!", "Warning: "+message, "#D79700") }
func (u *UI) Error(message string)   { u.status(u.errOut, "✗", "Error: "+message, "#E44F64") }

func (u *UI) status(w io.Writer, icon, message, color string) {
	if !u.color {
		fmt.Fprintln(w, message)
		return
	}
	styled := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(color)).Render(icon)
	lipgloss.Fprintln(w, styled, message)
}

func (u *UI) form(field huh.Field) error {
	output := u.out
	if u.accessible {
		// Huh's accessible prompts avoid screen redraws, but their theme still
		// emits color codes. Strip those for screen readers.
		output = &colorprofile.Writer{Forward: u.out, Profile: colorprofile.NoTTY}
	}
	return huh.NewForm(huh.NewGroup(field)).
		WithTheme(huh.ThemeFunc(huh.ThemeCharm)).
		WithAccessible(u.accessible).
		WithInput(u.in).
		WithOutput(output).
		RunWithContext(u.ctx)
}

func (u *UI) Choice(prompt string, choices []string, defaultIndex int) (int, error) {
	if len(choices) == 0 {
		return 0, fmt.Errorf("no choices available")
	}
	if defaultIndex < 0 || defaultIndex >= len(choices) {
		return 0, fmt.Errorf("default selection is out of range")
	}
	if u.forms {
		options := make([]huh.Option[int], len(choices))
		for i, choice := range choices {
			options[i] = huh.NewOption(choice, i)
		}
		selected := defaultIndex
		err := u.form(huh.NewSelect[int]().Title(prompt).Options(options...).Value(&selected))
		if err == nil {
			u.status(u.out, "✓", "Selected: "+choices[selected], "#04B575")
		}
		return selected, err
	}

	fmt.Fprintln(u.out, prompt)
	for i, choice := range choices {
		marker := " "
		if i == defaultIndex {
			marker = "*"
		}
		fmt.Fprintf(u.out, "  %s %d) %s\n", marker, i+1, choice)
	}
	fmt.Fprintf(u.out, "Selection [%d]: ", defaultIndex+1)
	line, err := u.readLine()
	if err != nil {
		return 0, err
	}
	if line == "" {
		return defaultIndex, nil
	}
	selected, err := strconv.Atoi(line)
	if err != nil || selected < 1 || selected > len(choices) {
		return 0, fmt.Errorf("selection must be a number from 1 to %d", len(choices))
	}
	return selected - 1, nil
}

func (u *UI) MultiChoice(prompt string, choices []string, defaultIndex int) ([]int, error) {
	if len(choices) == 0 {
		return nil, fmt.Errorf("no choices available")
	}
	if defaultIndex < 0 || defaultIndex >= len(choices) {
		return nil, fmt.Errorf("default selection is out of range")
	}
	if u.forms {
		options := make([]huh.Option[int], len(choices))
		for i, choice := range choices {
			options[i] = huh.NewOption(choice, i).Selected(i == defaultIndex)
		}
		selected := []int{defaultIndex}
		err := u.form(huh.NewMultiSelect[int]().
			Title(prompt).
			Description("Space selects, Enter continues").
			Options(options...).
			Value(&selected).
			Validate(func(values []int) error {
				if len(values) == 0 {
					return fmt.Errorf("select at least one module")
				}
				return nil
			}))
		if err == nil {
			labels := make([]string, 0, len(selected))
			for _, index := range selected {
				labels = append(labels, choices[index])
			}
			u.status(u.out, "✓", "Selected: "+strings.Join(labels, ", "), "#04B575")
		}
		return selected, err
	}

	fmt.Fprintln(u.out, prompt)
	for i, choice := range choices {
		marker := " "
		if i == defaultIndex {
			marker = "*"
		}
		fmt.Fprintf(u.out, "  %s %d) %s\n", marker, i+1, choice)
	}
	fmt.Fprintf(u.out, "Selections, comma-separated [%d]: ", defaultIndex+1)
	line, err := u.readLine()
	if err != nil {
		return nil, err
	}
	if line == "" {
		return []int{defaultIndex}, nil
	}
	seen := make(map[int]struct{})
	result := make([]int, 0)
	for _, part := range strings.Split(line, ",") {
		selected, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || selected < 1 || selected > len(choices) {
			return nil, fmt.Errorf("each selection must be a number from 1 to %d", len(choices))
		}
		index := selected - 1
		if _, exists := seen[index]; !exists {
			seen[index] = struct{}{}
			result = append(result, index)
		}
	}
	return result, nil
}

func (u *UI) Confirm(prompt string, defaultYes bool) (bool, error) {
	if u.forms {
		confirmed := defaultYes
		err := u.form(huh.NewConfirm().Title(prompt).Affirmative("Yes").Negative("No").Value(&confirmed))
		if err == nil {
			answer := "No"
			if confirmed {
				answer = "Yes"
			}
			u.status(u.out, "✓", "Answer: "+answer, "#04B575")
		}
		return confirmed, err
	}

	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	fmt.Fprintf(u.out, "%s %s ", prompt, suffix)
	line, err := u.readLine()
	if err != nil {
		return false, err
	}
	switch strings.ToLower(line) {
	case "":
		return defaultYes, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("answer yes or no")
	}
}

func (u *UI) Password(prompt string) (string, error) {
	if u.forms {
		var password string
		// EchoModeNone avoids even showing the number of typed characters.
		err := u.form(huh.NewInput().Title(prompt).EchoMode(huh.EchoModeNone).Value(&password))
		return password, err
	}

	fmt.Fprintf(u.out, "%s: ", prompt)
	if u.interactive {
		password, err := term.ReadPassword(int(u.in.Fd()))
		fmt.Fprintln(u.out)
		if err != nil {
			return "", err
		}
		return string(password), nil
	}
	return u.readLine()
}

func (u *UI) readLine() (string, error) {
	line, err := u.reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	if err == io.EOF && line == "" {
		return "", io.EOF
	}
	return strings.TrimSpace(line), nil
}
