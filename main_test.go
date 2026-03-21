package main

import (
	"testing"
)

func Test_table(t *testing.T) {
	var tbl table
	tbl = append(tbl, []any{"One", "Two", "Three", "Four"})
	tbl = append(tbl, []any{"1", "2", "", "4"})
	tbl = append(tbl, []any{"1a", "2a", "", "4a"})
	got := tbl.Render()
	const want = `One  Two  Four
1    2    4
1a   2a   4a
`
	if got != want {
		t.Logf("rendered table:\n%s", got)
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}
