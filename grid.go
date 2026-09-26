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

const gpuRowWidth = 8

func gpuGlyphRows(allocated, total, width int) []string {
	if total <= 0 || width < 1 {
		return nil
	}
	allocated = min(max(allocated, 0), total)
	rows := make([]string, 0, (total+width-1)/width)
	for start := 0; start < total; start += width {
		end := min(start+width, total)
		filled := min(max(allocated-start, 0), end-start)
		rows = append(rows, strings.Repeat("●", filled)+strings.Repeat("○", end-start-filled))
	}
	return rows
}

func gridTileLines(n Node, color bool, width int) []string {
	return gridTileLinesSelected(n, color, width, false)
}

func gridTileLinesSelected(n Node, color bool, width int, selected bool) []string {
	status, shade := nodeStatus(n)
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
	label := ""
	if n.GPUTotal > 0 {
		label = fmt.Sprintf("%d/%d", n.GPUAllocated, n.GPUTotal)
	}
	rows := gpuGlyphRows(n.GPUAllocated, n.GPUTotal, gpuRowWidth)
	if len(rows) == 0 {
		rows = []string{""}
	}
	indent := strings.Repeat(" ", len([]rune(n.Name))+3)
	lines := make([]string, len(rows))
	lines[0] = fit(fmt.Sprintf("%s %s %s %s", n.Name, marker, rows[0], label), width)
	for i := 1; i < len(rows); i++ {
		lines[i] = fit(indent+rows[i], width)
	}
	if color {
		for i := range lines {
			// Color the whole tile by health, retaining a text marker for NO_COLOR.
			lines[i] = tint(lines[i], shade, true)
		}
	}
	if selected {
		for i := range lines {
			lines[i] = "\x1b[7m" + lines[i] + "\x1b[27m"
		}
	}
	return lines
}

func gridTile(n Node, color bool, width int) string {
	return gridTileLines(n, color, width)[0]
}

// gridNodeAt returns the node under a 1-based terminal coordinate in the grid
// body. bodyLine is zero-based and includes the grid's three-line header.
func gridNodeAt(nodes []Node, width, bodyLine, x int) int {
	if bodyLine < 3 || x < 1 {
		return -1
	}
	column := (x - 1) / (tileWidth + 1)
	if column >= gridColumns(width) || (x-1)%(tileWidth+1) >= tileWidth {
		return -1
	}
	line := bodyLine - 3
	columns := gridColumns(width)
	for start := 0; start < len(nodes); start += columns {
		rows := 1
		for i := start; i < min(start+columns, len(nodes)); i++ {
			rows = max(rows, len(gpuGlyphRows(nodes[i].GPUAllocated, nodes[i].GPUTotal, gpuRowWidth)))
		}
		if line < rows {
			index := start + column
			if index < len(nodes) {
				return index
			}
			return -1
		}
		line -= rows
	}
	return -1
}

func renderGrid(w io.Writer, nodes []Node, at time.Time, width, page, pageSize int, color bool) error {
	return renderGridSelected(w, nodes, at, width, page, pageSize, color, -1)
}

func renderGridSelected(w io.Writer, nodes []Node, at time.Time, width, page, pageSize int, color bool, selected int) error {
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
		tiles := make([][]string, 0, columns)
		rows := 0
		for j := i; j < end && j < i+columns; j++ {
			tile := gridTileLinesSelected(nodes[j], color, tileWidth, j == selected)
			tiles = append(tiles, tile)
			rows = max(rows, len(tile))
		}
		for row := 0; row < rows; row++ {
			parts := make([]string, len(tiles))
			for column, tile := range tiles {
				if row < len(tile) {
					parts[column] = tile[row]
				} else {
					parts[column] = strings.Repeat(" ", tileWidth)
				}
			}
			if _, err := fmt.Fprintln(w, strings.Join(parts, " ")); err != nil {
				return err
			}
		}
	}
	return nil
}
