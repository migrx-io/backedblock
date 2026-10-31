package ui

import (
	"strings"
	"testing"
)

func TestTable(t *testing.T) {
	var b strings.Builder
	Table(&b, []string{"A", "VERSION"}, []string{"terraform", "1.16.7"})
	want := `┌───────────┬─────────┐
│ A         │ VERSION │
├───────────┼─────────┤
│ terraform │ 1.16.7  │
└───────────┴─────────┘
`
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
}
