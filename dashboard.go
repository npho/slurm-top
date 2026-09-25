package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// The dashboard uses only printable runes for layout. ANSI styling is applied
// after padding so terminal escape sequences never affect column widths.
func fit(s string, width int) string {
	r := []rune(s)
	if len(r) > width {
		return string(r[:width])
	}
	return s + strings.Repeat(" ", width-len(r))
}
func bar(used, total, width int) string {
	if total <= 0 {
		return strings.Repeat("?", width)
	}
	if used < 0 {
		used = 0
	}
	if used > total {
		used = total
	}
	filled := (used*width + total/2) / total
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}
func tint(s, code string, enabled bool) string {
	if !enabled {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func nodeStatus(n Node) (string, string) {
	s := strings.ToUpper(n.State)
	switch {
	case strings.Contains(s, "DOWN"), strings.Contains(s, "DRAIN"), strings.Contains(s, "FAIL"), strings.Contains(s, "MAINT"), strings.Contains(s, "RESPOND"), strings.Contains(s, "INVAL"):
		return "× unavailable", "31"
	case strings.Contains(s, "RESERVED"):
		return "R reserved", "33"
	case strings.Contains(s, "PLANNED"):
		return "P planned", "33"
	case n.GPUTotal == 0:
		return "CPU only", "36"
	case n.GPUAllocated >= n.GPUTotal:
		return "● full", "36"
	case n.GPUAllocated > 0:
		return "◐ partial", "33"
	default:
		return "○ free", "32"
	}
}
func card(n Node, width int, color bool) []string {
	inner := width - 2
	status, shade := nodeStatus(n)
	content := []string{n.Name + "  " + status}
	if n.GPUTotal > 0 {
		glyphs := strings.Repeat("●", n.GPUAllocated) + strings.Repeat("○", n.GPUTotal-n.GPUAllocated)
		content = append(content, fmt.Sprintf("GPU %-7s %s %d/%d", n.GPUType, glyphs, n.GPUAllocated, n.GPUTotal))
	} else {
		content = append(content, "GPU -")
	}
	content = append(content, fmt.Sprintf("CPU alloc %s %d/%d", bar(n.CPUAllocated, n.CPUTotal, 8), n.CPUAllocated, n.CPUTotal))
	content = append(content, fmt.Sprintf("Mem alloc %s %s/%s GiB", bar(n.MemoryAllocatedMB, n.MemoryTotalMB, 8), gib(n.MemoryAllocatedMB), gib(n.MemoryTotalMB)))
	if n.MemoryUsedApproxMB != nil {
		content = append(content, fmt.Sprintf("Mem used~ %s %.0f%%", bar(*n.MemoryUsedApproxMB, n.MemoryTotalMB, 8), float64(*n.MemoryUsedApproxMB)*100/float64(n.MemoryTotalMB)))
	} else {
		content = append(content, "Mem used~ -")
	}
	result := []string{"╭" + strings.Repeat("─", inner) + "╮"}
	for i, line := range content {
		line = fit(line, inner)
		if color {
			if i == 0 {
				line = tint(line, shade, true)
			}
			if i == 1 && n.GPUTotal > 0 {
				line = strings.ReplaceAll(line, "●", tint("●", "36", true))
				line = strings.ReplaceAll(line, "○", tint("○", "32", true))
			}
		}
		result = append(result, "│"+line+"│")
	}
	return append(result, "╰"+strings.Repeat("─", inner)+"╯")
}
func renderDashboard(w io.Writer, nodes []Node, at time.Time, width int, color bool) error {
	return renderDashboardFor(w, nodes, nodes, at, width, color)
}
func renderDashboardFor(w io.Writer, summaryNodes, nodes []Node, at time.Time, width int, color bool) error {
	if width < 30 {
		width = 30
	} // a legible single column on very narrow terminals
	const cardWidth = 44
	columns := width / (cardWidth + 1)
	if columns < 1 {
		columns = 1
	}
	if columns > 4 {
		columns = 4
	}
	cw := cardWidth
	if columns == 1 && width < cardWidth {
		cw = width
	}
	total, alloc, free, bad := 0, 0, 0, 0
	for _, n := range summaryNodes {
		total += n.GPUTotal
		alloc += n.GPUAllocated
		if unavailable(n.State) {
			bad++
		} else {
			free += n.GPUTotal - n.GPUAllocated
		}
	}
	if _, err := fmt.Fprintf(w, "Slurm GPU health  %s\n%d nodes  •  %d/%d GPUs allocated  •  %d free on usable nodes  •  %d unavailable/restricted\n", at.Format("15:04:05"), len(summaryNodes), alloc, total, free, bad); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "● allocated  ○ free  ◐ partial  R reserved  P planned  × unavailable"); err != nil {
		return err
	}
	for i := 0; i < len(nodes); i += columns {
		cards := make([][]string, 0, columns)
		for j := i; j < len(nodes) && j < i+columns; j++ {
			cards = append(cards, card(nodes[j], cw, color))
		}
		for line := range cards[0] {
			parts := make([]string, 0, len(cards))
			for _, c := range cards {
				parts = append(parts, c[line])
			}
			if _, err := fmt.Fprintln(w, strings.Join(parts, " ")); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintln(w, "CPU/Mem alloc = scheduler reservations; Mem used~ = approximate OS usage. CPU utilization is not sampled.")
	return err
}
