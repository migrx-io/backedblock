package ui

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Table writes rows under a header, inside box-drawing borders.
func Table(w io.Writer, header []string, rows ...[]string) {
	widths := make([]int, len(header))
	for _, r := range append([][]string{header}, rows...) {
		for i, c := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	line := func(l, m, r string) {
		parts := make([]string, len(widths))
		for i, n := range widths {
			parts[i] = strings.Repeat("─", n+2)
		}
		fmt.Fprintln(w, l+strings.Join(parts, m)+r)
	}
	row := func(r []string) {
		for i, n := range widths {
			c := ""
			if i < len(r) {
				c = r[i]
			}
			fmt.Fprintf(w, "│ %s%s ", c, strings.Repeat(" ", n-utf8.RuneCountInString(c)))
		}
		fmt.Fprintln(w, "│")
	}
	line("┌", "┬", "┐")
	row(header)
	line("├", "┼", "┤")
	for _, r := range rows {
		row(r)
	}
	line("└", "┴", "┘")
}
