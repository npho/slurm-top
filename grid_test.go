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
func TestGridNodeAt(t *testing.T) {
	nodes := []Node{{Name: "g1", GPUTotal: 8}, {Name: "g2", GPUTotal: 12}, {Name: "g3", GPUTotal: 8}}
	// Grid header occupies rows 0-2; g2 makes the first tile group two rows.
	if got := gridNodeAt(nodes, 50, 3, 1); got != 0 {
		t.Fatalf("first node = %d", got)
	}
	if got := gridNodeAt(nodes, 50, 4, 26); got != 1 {
		t.Fatalf("wrapped node = %d", got)
	}
	if got := gridNodeAt(nodes, 50, 5, 1); got != 2 {
		t.Fatalf("second grid row = %d", got)
	}
	if got := gridNodeAt(nodes, 50, 3, 25); got != -1 { // column separator
		t.Fatalf("separator = %d", got)
	}
}

func TestGPUCircleRowsWrapAtEight(t *testing.T) {
	if got := gpuGlyphRows(10, 12, 8); len(got) != 2 || got[0] != "●●●●●●●●" || got[1] != "●●○○" {
		t.Fatalf("GPU rows = %q", got)
	}
	lines := gridTileLines(Node{Name: "mig", State: "IDLE", GPUTotal: 12, GPUAllocated: 10}, false, tileWidth)
	if len(lines) != 2 || !strings.Contains(lines[0], "●●●●●●●●") || !strings.Contains(lines[1], "●●○○") {
		t.Fatalf("grid tile lines = %q", lines)
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
