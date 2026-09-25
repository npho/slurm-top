package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestGridPagesAndWidths(t *testing.T) {
	nodes := make([]Node, 0, 24)
	for i := 1; i <= 24; i++ {
		nodes = append(nodes, Node{Name: fmt.Sprintf("g%03d", i), State: "IDLE", GPUTotal: 8, GPUAllocated: 2})
	}
	size := gridPageSize(100, 8)
	if size != 16 || gridPages(len(nodes), size) != 2 {
		t.Fatalf("size=%d pages=%d", size, gridPages(len(nodes), size))
	}
	var b bytes.Buffer
	if err := renderGrid(&b, nodes, time.Unix(0, 0), 100, 1, size, false); err != nil {
		t.Fatal(err)
	}
	s := b.String()
	if strings.Contains(s, "g001") || !strings.Contains(s, "g017") || !strings.Contains(s, "24 nodes") {
		t.Fatalf("bad page: %s", s)
	}
	for _, line := range strings.Split(s, "\n")[3:] {
		if utf8.RuneCountInString(line) > 100 {
			t.Errorf("row too wide: %q", line)
		}
	}
	if strings.Contains(s, "\x1b[") {
		t.Fatal("ANSI in non-terminal grid")
	}
}
func TestGridStatusWithoutColor(t *testing.T) {
	for _, tc := range []struct{ state, marker string }{{"DOWN", "×"}, {"RESERVED", "R"}, {"PLANNED", "P"}, {"IDLE", "○"}} {
		n := Node{Name: "g1", State: tc.state, GPUTotal: 4, GPUAllocated: 0}
		if !strings.Contains(gridTile(n, false, 24), "g1 "+tc.marker) {
			t.Errorf("status %s: %s", tc.state, gridTile(n, false, 24))
		}
	}
}
