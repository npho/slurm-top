package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

const tileWidth = 24

func gridColumns(width int) int {
	if width < tileWidth {
		return 1
	}
	return (width + 1) / (tileWidth + 1)
}
func gridPageSize(width, height int) int {
	// Three header lines and one footer line in interactive mode.
	rows := height - 4
	if rows < 1 {
		rows = 1
	}
	return gridColumns(width) * rows
}
func gridPages(count, size int) int {
	if size < 1 {
		size = 1
	}
	if count == 0 {
		return 1
	}
	return (count + size - 1) / size
}
func gridHeader(nodes []Node, at time.Time) string {
	total, alloc, free, bad := 0, 0, 0, 0
	for _, n := range nodes {
		total += n.GPUTotal
		alloc += n.GPUAllocated
		if unavailable(n.State) {
			bad++
		} else {
			free += n.GPUTotal - n.GPUAllocated
		}
	}
	return fmt.Sprintf("Slurm GPU health  %s\n%d nodes  •  GPUs %d/%d allocated  •  %d free  •  %d restricted/unavailable\n● allocated  ○ free  R reserved  P planned  × unavailable\n", at.Format("15:04:05"), len(nodes), alloc, total, free, bad)
}
func gridTile(n Node, color bool, width int) string {
	status, shade := nodeStatus(n)
	// Keep the node's status visible even if its glyph sequence must be shortened.
	label := ""
	if n.GPUTotal > 0 {
		label = fmt.Sprintf("%d/%d", n.GPUAllocated, n.GPUTotal)
	}
	glyphs := ""
	if n.GPUTotal > 0 {
		max := width - len([]rune(n.Name)) - len([]rune(label)) - 5
		if max < 0 {
			max = 0
		}
		if max > n.GPUTotal {
			max = n.GPUTotal
		}
		allocated := n.GPUAllocated
		if allocated > max {
			allocated = max
		}
		glyphs = strings.Repeat("●", allocated) + strings.Repeat("○", max-allocated)
		if max < n.GPUTotal && max > 0 {
			glyphs = strings.TrimSuffix(glyphs, "○") + "…"
		}
	}
	marker := "○"
	switch {
	case strings.HasPrefix(status, "×"):
		marker = "×"
	case strings.HasPrefix(status, "R"):
		marker = "R"
	case strings.HasPrefix(status, "P"):
		marker = "P"
	case n.GPUTotal == 0:
		marker = "-"
	case n.GPUAllocated >= n.GPUTotal:
		marker = "●"
	case n.GPUAllocated > 0:
		marker = "◐"
	}
	line := fit(fmt.Sprintf("%s %s %s %s", n.Name, marker, glyphs, label), width)
	if color {
		// Color the whole tile by health, retaining a text marker for NO_COLOR.
		line = tint(line, shade, true)
	}
	return line
}
func renderGrid(w io.Writer, nodes []Node, at time.Time, width, page, pageSize int, color bool) error {
	if width < tileWidth {
		width = tileWidth
	}
	if _, err := io.WriteString(w, gridHeader(nodes, at)); err != nil {
		return err
	}
	columns := gridColumns(width)
	start := page * pageSize
	if start > len(nodes) {
		start = len(nodes)
	}
	end := len(nodes)
	if pageSize > 0 && start+pageSize < end {
		end = start + pageSize
	}
	for i := start; i < end; i += columns {
		parts := make([]string, 0, columns)
		for j := i; j < end && j < i+columns; j++ {
			parts = append(parts, gridTile(nodes[j], color, tileWidth))
		}
		if _, err := fmt.Fprintln(w, strings.Join(parts, " ")); err != nil {
			return err
		}
	}
	return nil
}
