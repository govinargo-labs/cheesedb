package main

// Terminal styling for the demo. Colors are used only when writing to a
// terminal and NO_COLOR is unset, so output piped to a file stays plain.

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

var useColor = func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}()

func paint(code, s string) string {
	if !useColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func bold(s string) string   { return paint("1", s) }
func dim(s string) string    { return paint("2", s) }
func red(s string) string    { return paint("31", s) }
func green(s string) string  { return paint("32", s) }
func yellow(s string) string { return paint("33", s) }
func cyan(s string) string   { return paint("36", s) }
func plain(s string) string  { return s }

// pad fills s with spaces to w runes. Pad before coloring: escape codes
// have no width on screen but would count here.
func pad(s string, w int) string {
	return s + strings.Repeat(" ", max(0, w-utf8.RuneCountInString(s)))
}

// clip shortens s to at most n runes, ending in "…" when cut.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// wrap breaks text into lines of at most width runes, at spaces.
func wrap(text string, width int) []string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(text) {
		if line != "" && utf8.RuneCountInString(line)+1+utf8.RuneCountInString(w) > width {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// box draws lines in bold inside a rounded frame.
func box(lines ...string) {
	w := 0
	for _, l := range lines {
		w = max(w, utf8.RuneCountInString(l))
	}
	fmt.Println("  ╭" + strings.Repeat("─", w+4) + "╮")
	for _, l := range lines {
		fmt.Println("  │  " + bold(pad(l, w)) + "  │")
	}
	fmt.Println("  ╰" + strings.Repeat("─", w+4) + "╯")
}

// cell is one table cell: its text and how to color it.
type cell struct {
	text  string
	color func(string) string
}

// table draws rows with box-drawing lines. Row 0 is the header.
func table(widths []int, rows [][]cell) {
	rule := func(l, m, r string) {
		parts := make([]string, len(widths))
		for i, w := range widths {
			parts[i] = strings.Repeat("─", w+2)
		}
		fmt.Println("    " + l + strings.Join(parts, m) + r)
	}
	rule("┌", "┬", "┐")
	for i, row := range rows {
		var b strings.Builder
		b.WriteString("    │")
		for j, c := range row {
			b.WriteString(" " + c.color(pad(c.text, widths[j])) + " │")
		}
		fmt.Println(b.String())
		if i == 0 {
			rule("├", "┼", "┤")
		}
	}
	rule("└", "┴", "┘")
}
