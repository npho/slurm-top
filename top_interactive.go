package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
	"syscall"
)

type uiEvent struct {
	key   string
	x, y  int
	wheel int
}

// SGR mouse reports are 1-based: ESC [ < button ; x ; y M (press) / m (release).
func readTopEvents(in *os.File, events chan<- uiEvent) {
	bytes := make(chan byte)
	go func() {
		defer close(bytes)
		var buf [1]byte
		for {
			if _, err := in.Read(buf[:]); err != nil {
				return
			}
			bytes <- buf[0]
		}
	}()

	state := 0
	var seq strings.Builder
	var escapeTimeout <-chan time.Time
	var escapeTimer *time.Timer
	send := func(e uiEvent) {
		select {
		case events <- e:
		default:
		}
	}
	stopEscapeTimer := func() {
		if escapeTimer != nil && !escapeTimer.Stop() {
			select {
			case <-escapeTimer.C:
			default:
			}
		}
		escapeTimeout = nil
	}
	for {
		select {
		case <-escapeTimeout:
			// A lone ESC is distinct from an escape sequence. Give terminals a
			// brief opportunity to deliver the remainder of an arrow sequence.
			send(uiEvent{key: "escape"})
			state = 0
			escapeTimeout = nil
		case b, ok := <-bytes:
			if !ok {
				return
			}
			switch state {
			case 0:
				if b == 27 {
					state = 1
					escapeTimer = time.NewTimer(50 * time.Millisecond)
					escapeTimeout = escapeTimer.C
				} else {
					send(uiEvent{key: string(b)})
				}
			case 1:
				stopEscapeTimer()
				if b == '[' {
					state = 2
					seq.Reset()
				} else {
					send(uiEvent{key: "escape"})
					state = 0
					send(uiEvent{key: string(b)})
				}
			case 2:
				if b == 'A' || b == 'B' || b == 'C' || b == 'D' || b == 'Z' {
					send(uiEvent{key: map[byte]string{'A': "up", 'B': "down", 'C': "right", 'D': "left", 'Z': "shift-tab"}[b]})
					state = 0
					continue
				}
				if b == 'M' || b == 'm' {
					text := seq.String()
					state = 0
					if !strings.HasPrefix(text, "<") || b == 'm' {
						continue
					}
					parts := strings.Split(text[1:], ";")
					if len(parts) != 3 {
						continue
					}
					button, e1 := strconv.Atoi(parts[0])
					x, e2 := strconv.Atoi(parts[1])
					y, e3 := strconv.Atoi(parts[2])
					if e1 != nil || e2 != nil || e3 != nil {
						continue
					}
					if button == 64 {
						send(uiEvent{wheel: -1})
					} else if button == 65 {
						send(uiEvent{wheel: 1})
					} else if button == 0 {
						send(uiEvent{key: "click", x: x, y: y})
					}
				} else if seq.Len() < 40 {
					seq.WriteByte(b)
				} else {
					state = 0
				}
			}
		}
	}
}
func sortJobs(jobs []Job, by string, asc bool) {
	sort.Slice(jobs, func(i, j int) bool {
		a, b := jobs[i], jobs[j]
		cmp := 0
		switch by {
		case "account":
			cmp = strings.Compare(a.Account, b.Account)
		case "name":
			cmp = strings.Compare(a.Name, b.Name)
		case "qos":
			cmp = strings.Compare(a.QoS, b.QoS)
		case "partition":
			cmp = strings.Compare(a.Partition, b.Partition)
		case "progress":
			cmp = a.Progress - b.Progress
		case "elapsed":
			cmp = a.ElapsedMinutes - b.ElapsedMinutes
		case "cpu-gpu":
			left, right := ratioSortValue(a.CPUs, a.GPUs), ratioSortValue(b.CPUs, b.GPUs)
			if left < right {
				cmp = -1
			} else if left > right {
				cmp = 1
			}
		case "memory-cpu":
			left, right := ratioSortValue(a.MemoryMB, a.CPUs), ratioSortValue(b.MemoryMB, b.CPUs)
			if left < right {
				cmp = -1
			} else if left > right {
				cmp = 1
			}
		case "cpu":

			cmp = a.CPUs - b.CPUs
		case "gpu":
			cmp = a.GPUs - b.GPUs
		case "mem":
			cmp = a.MemoryMB - b.MemoryMB
		default:
			cmp = a.ID - b.ID
		}
		if cmp == 0 {
			return a.ID < b.ID
		}
		if asc {
			return cmp < 0
		}
		return cmp > 0
	})
}
func displayQoS(qos string, color bool) string {
	if color && strings.EqualFold(qos, "urgent") {
		// Do not use SGR reset (0): it would clear a selected row's background.
		return "\x1b[1;31m" + qos + "\x1b[22;39m"
	}
	return qos
}

func progressBar(percent, width int, color bool) string {
	plain := bar(percent, 100, width)
	if !color || percent <= 0 {
		return plain
	}
	filled := (min(100, percent)*width + 50) / 100
	// Restore the default foreground without clearing a selected row's reverse-video background.
	return "\x1b[32m" + strings.Repeat("█", filled) + "\x1b[39m" + strings.Repeat("░", width-filled)
}

type tableColumn struct {
	field string
	label string
}

func rightAlignedColumn(field string) bool {
	switch field {
	case "jobs", "pending-jobs", "gpu", "cpu", "cpu-gpu", "mem", "memory-cpu", "pending-gpu", "pending-cpu", "pending-cpu-gpu", "pending-mem", "pending-memory-cpu", "elapsed":
		return true
	default:
		return false
	}
}

func renderTable(columns []tableColumn, rows [][]string, by string, asc bool) (string, []string, []headerColumn) {
	widths := make([]int, len(columns))
	for i, column := range columns {
		// Reserve the space and sort arrow even while inactive so selecting a
		// column does not change the table layout.
		widths[i] = utf8.RuneCountInString(column.label) + 2
	}
	for _, row := range rows {
		for i, value := range row {
			widths[i] = max(widths[i], utf8.RuneCountInString(sgrPattern.ReplaceAllString(value, "")))
		}
	}
	headerParts := make([]string, len(columns))
	headerColumns := make([]headerColumn, len(columns))
	start := 1
	for i, column := range columns {
		headerParts[i] = leftHeader(column.label, column.field, by, asc, widths[i])
		headerColumns[i] = headerColumn{column.field, start, start + widths[i] - 1}
		start += widths[i] + 2
	}
	lines := make([]string, len(rows))
	for i, row := range rows {
		parts := make([]string, len(row))
		for j, value := range row {
			// Do not reset ANSI state between cells: a reset here would cancel
			// the reverse-video background of a selected row after its progress bar.
			visible := utf8.RuneCountInString(sgrPattern.ReplaceAllString(value, ""))
			padding := strings.Repeat(" ", widths[j]-visible)
			if rightAlignedColumn(columns[j].field) {
				parts[j] = padding + value
			} else {
				parts[j] = value + padding
			}
		}
		lines[i] = strings.Join(parts, "  ")
	}
	return "\x1b[1m" + strings.Join(headerParts, "  ") + "\x1b[22m", lines, headerColumns
}

func topRows(s Snapshot, user, by string, asc bool, color ...bool) ([]string, []string, []string, []headerColumn) {
	colorEnabled := len(color) > 0 && color[0]
	if user == "" {
		users := append([]Usage(nil), s.Users...)
		sortUsers(users, by)
		if asc {
			for i, j := 0, len(users)-1; i < j; i, j = i+1, j-1 {
				users[i], users[j] = users[j], users[i]
			}
		}
		columns := []tableColumn{{"user", "USER"}, {"jobs", "RUN"}, {"gpu", "GPU"}, {"cpu", "CPU"}, {"cpu-gpu", "C:G"}, {"mem", "MEM"}, {"memory-cpu", "M:C"}, {"pending-jobs", "PEND"}, {"pending-gpu", "GPU"}, {"pending-cpu", "CPU"}, {"pending-cpu-gpu", "C:G"}, {"pending-mem", "MEM"}, {"pending-memory-cpu", "M:C"}}
		rows := make([][]string, 0, len(users))
		ids := make([]string, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.User)
			rows = append(rows, []string{u.User, strconv.Itoa(u.RunningJobs), strconv.Itoa(u.GPUs), strconv.Itoa(u.CPUs), cpuGPU(u.CPUs, u.GPUs), gb(u.MemoryMB), memoryCPU(u.MemoryMB, u.CPUs), strconv.Itoa(u.PendingJobs), strconv.Itoa(u.PendingGPUs), strconv.Itoa(u.PendingCPUs), cpuGPU(u.PendingCPUs, u.PendingGPUs), gb(u.PendingMemoryMB), memoryCPU(u.PendingMemoryMB, u.PendingCPUs)})
		}
		header, lines, headerColumns := renderTable(columns, rows, by, asc)
		return []string{header}, lines, ids, headerColumns
	}
	jobs := []Job{}
	for _, j := range s.Jobs {
		if j.User == user {
			jobs = append(jobs, j)
		}
	}
	sortJobs(jobs, by, asc)
	columns := []tableColumn{{"id", "JOB ID"}, {"account", "ACCOUNT"}, {"qos", "QOS"}, {"progress", "PROGRESS"}, {"elapsed", "ELAPSED"}, {"partition", "PARTITION"}, {"gpu", "GPU"}, {"cpu", "CPU"}, {"cpu-gpu", "C:G"}, {"mem", "MEM"}, {"memory-cpu", "M:C"}, {"name", "NAME"}}
	rows := make([][]string, 0, len(jobs))
	ids := make([]string, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, strconv.Itoa(j.ID)+"/"+j.State)
		rows = append(rows, []string{strconv.Itoa(j.ID), j.Account, displayQoS(j.QoS, colorEnabled), progressBar(j.Progress, 10, colorEnabled), j.Elapsed, j.Partition, strconv.Itoa(j.GPUs), strconv.Itoa(j.CPUs), cpuGPU(j.CPUs, j.GPUs), gb(j.MemoryMB), memoryCPU(j.MemoryMB, j.CPUs), j.Name})
	}
	header, lines, headerColumns := renderTable(columns, rows, by, asc)
	return []string{header}, lines, ids, headerColumns
}

type headerColumn struct {
	field      string
	start, end int // 1-based, inclusive terminal columns
}

func headerColumns(user string) []headerColumn {
	if user == "" {
		return []headerColumn{
			{"user", 1, 16}, {"jobs", 18, 23}, {"gpu", 25, 30}, {"cpu", 32, 37},
			{"cpu-gpu", 39, 44}, {"mem", 46, 51}, {"memory-cpu", 53, 58}, {"pending-jobs", 62, 67},
			{"pending-gpu", 69, 74}, {"pending-cpu", 76, 81}, {"pending-cpu-gpu", 83, 88},
			{"pending-mem", 90, 95}, {"pending-memory-cpu", 97, 102},
		}
	}
	return []headerColumn{
		{"id", 1, 11}, {"account", 13, 26}, {"qos", 28, 37}, {"progress", 39, 48},
		{"partition", 50, 65}, {"gpu", 67, 74}, {"cpu", 76, 83}, {"cpu-gpu", 85, 92},
		{"mem", 94, 103}, {"memory-cpu", 105, 114}, {"name", 117, 9999},
	}
}

func headerSort(x int, user string) string {
	for _, column := range headerColumns(user) {
		if x >= column.start && x <= column.end {
			return column.field
		}
	}
	return ""
}

// headerByteOffset converts a terminal-cell offset to a byte offset. Headers
// include Unicode sort arrows, so byte indexes would shift later columns.
func headerByteOffset(header string, cells int) int {
	if cells <= 0 {
		return 0
	}
	for i := 0; i < len(header) && cells > 0; {
		if header[i] == 27 && i+1 < len(header) && header[i+1] == '[' {
			if end := strings.IndexByte(header[i:], 'm'); end >= 0 {
				i += end + 1
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(header[i:])
		i += size
		cells--
		if cells == 0 {
			return i
		}
	}
	return len(header)
}

func highlightHeader(header string, column headerColumn) string {
	start := headerByteOffset(header, column.start-1)
	end := headerByteOffset(header, column.end)
	if start >= end {
		return header
	}
	return header[:start] + "\x1b[7m" + header[start:end] + "\x1b[27m" + header[end:]
}

// Color reflects Slurm allocation / configured capacity, not measured usage.
func coloredBar(n, total, width int, enabled bool) string {
	plain := percentBar(n, total, width)
	if !enabled {
		return plain
	}
	if total <= 0 {
		return "\x1b[38;2;128;128;128m" + plain + "\x1b[0m"
	}
	p := float64(n) / float64(total)
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	red, green := 0, 200
	if p < 0.5 {
		red = int(p*2*255 + 0.5)
	} else {
		red = 255
		green = int((1-p)*2*200 + 0.5)
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;0m%s\x1b[0m", red, green, plain)
}

// Width-aware padding/clipping of ANSI SGR text: escape codes have zero width.
func cropANSI(s string, offset, width int) string {
	var out strings.Builder
	cells, written := 0, 0
	for i := 0; i < len(s) && written < width; {
		if s[i] == 27 && i+1 < len(s) && s[i+1] == '[' {
			end := strings.IndexByte(s[i:], 'm')
			if end >= 0 {
				if cells >= offset {
					out.WriteString(s[i : i+end+1])
				}
				i += end + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if cells >= offset {
			out.WriteRune(r)
			written++
		}
		cells++
	}
	if written < width {
		out.WriteString(strings.Repeat(" ", width-written))
	}
	out.WriteString("\x1b[0m")
	return out.String()
}

func fitANSI(s string, width int) string {
	var out strings.Builder
	cells := 0
	for i := 0; i < len(s) && cells < width; {
		if s[i] == 27 && i+1 < len(s) && s[i+1] == '[' {
			end := strings.IndexByte(s[i:], 'm')
			if end >= 0 {
				out.WriteString(s[i : i+end+1])
				i += end + 1
				continue
			}
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		out.WriteRune(r)
		i += n
		cells++
	}
	if cells < width {
		out.WriteString(strings.Repeat(" ", width-cells))
	}
	out.WriteString("\x1b[0m") // reset even when clipped inside a colored bar
	return out.String()
}

var sgrPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visibleWidth(s string) int { return utf8.RuneCountInString(sgrPattern.ReplaceAllString(s, "")) }

type usageValue struct {
	percent string
	used    string
	total   string
	bold    bool
}

func usageValueFor(used, total int, usedText, totalText string, bold bool) usageValue {
	percent := 0.0
	if total > 0 {
		percent = float64(used) * 100 / float64(total)
	}
	return usageValue{strings.TrimSuffix(fmt.Sprintf("%.1f", percent), ".0") + "%", usedText, totalText, bold}
}

// usagePair aligns percent signs and slash separators across allocation and
// unschedulable-capacity rows before their widths are used for status bars.
func usagePair(primary, secondary usageValue) (string, string) {
	percentWidth := max(visibleWidth(primary.percent), visibleWidth(secondary.percent))
	usedWidth := max(visibleWidth(primary.used), visibleWidth(secondary.used))
	render := func(value usageValue) string {
		percent := strings.Repeat(" ", percentWidth-visibleWidth(value.percent)) + value.percent
		if value.bold {
			percent = "\x1b[1m" + percent + "\x1b[0m"
		}
		used := strings.Repeat(" ", usedWidth-visibleWidth(value.used)) + value.used
		return percent + "  " + used + "/" + value.total
	}
	return render(primary), render(secondary)
}

func statBox(label string, content ...string) []string {
	inner := 0
	for _, line := range content {
		inner = max(inner, visibleWidth(line))
	}
	result := []string{"╭─ " + label + " " + strings.Repeat("─", max(0, inner-visibleWidth(label)-3)) + "╮"}
	for _, line := range content {
		result = append(result, "│"+fitANSI(line, inner)+"│")
	}
	return append(result, "╰"+strings.Repeat("─", inner)+"╯")
}

func paneMenuLabel(pane string) string {
	return map[string]string{"cluster": "JOBS", "user": "JOBS", "gpu": "GPU", "node": "NODE"}[pane]
}

// viewMenu presents every lower-pane view and highlights the active label.
func viewMenu(active string) []string {
	labels := []string{"JOBS", "GPU", "NODE"}
	for i, label := range labels {
		padded := " " + label + " "
		if label == active {
			labels[i] = "\x1b[7m" + padded + "\x1b[27m"
		} else {
			labels[i] = padded
		}
	}
	return []string{strings.Join(labels, "  ")}
}

func menuColumns() []headerColumn {
	labels := []struct {
		field, label string
	}{{"cluster", "JOBS"}, {"gpu", "GPU"}, {"node", "NODE"}}
	columns := make([]headerColumn, len(labels))
	start := 1
	for i, item := range labels {
		end := start + visibleWidth(item.label) + 1
		columns[i] = headerColumn{item.field, start, end}
		start = end + 3 // two spaces separate adjacent menu items
	}
	return columns
}

// cycleTopPane skips USER because a user-specific job view can only be opened
// from CLUSTER with Enter or Right.
func cycleTopPane(pane string, reverse bool) string {
	if pane == "user" {
		if reverse {
			return "cluster"
		}
		return "gpu"
	}
	panes := []string{"cluster", "gpu", "node"}
	for i, candidate := range panes {
		if pane != candidate {
			continue
		}
		if reverse {
			return panes[(i+len(panes)-1)%len(panes)]
		}
		return panes[(i+1)%len(panes)]
	}
	return "cluster"
}

func joinStatBoxes(boxes ...[]string) []string {
	lines := make([]string, 0, len(boxes[0]))
	for row := range boxes[0] {
		parts := make([]string, len(boxes))
		for i, box := range boxes {
			parts[i] = box[row]
		}
		lines = append(lines, strings.Join(parts, "  "))
	}
	return lines
}

func horizontalBars(s Snapshot, width int, colored ...bool) []string {
	cpu, mem := s.AllocatableCPUUsed, s.AllocatableMemoryUsedMB
	enabled := len(colored) > 0 && colored[0]
	cpuInfo, cpuUnavailable := usagePair(
		usageValueFor(cpu, s.AllocatableCPU, strconv.Itoa(cpu), strconv.Itoa(s.AllocatableCPU), true),
		usageValueFor(s.CapacityCPU-s.AllocatableCPU, s.CapacityCPU, strconv.Itoa(s.CapacityCPU-s.AllocatableCPU), strconv.Itoa(s.CapacityCPU), false))
	memInfo, memUnavailable := usagePair(
		usageValueFor(mem, s.AllocatableMemoryMB, tb(mem), tb(s.AllocatableMemoryMB)+" TB", true),
		usageValueFor(s.CapacityMemoryMB-s.AllocatableMemoryMB, s.CapacityMemoryMB, tb(s.CapacityMemoryMB-s.AllocatableMemoryMB), tb(s.CapacityMemoryMB)+" TB", false))
	h200Info, h200Unavailable := usagePair(
		usageValueFor(s.AllocatableH200Used, s.AllocatableH200, strconv.Itoa(s.AllocatableH200Used), strconv.Itoa(s.AllocatableH200), true),
		usageValueFor(s.H200Capacity-s.AllocatableH200, s.H200Capacity, strconv.Itoa(s.H200Capacity-s.AllocatableH200), strconv.Itoa(s.H200Capacity), false))
	migInfo, migUnavailable := usagePair(
		usageValueFor(s.AllocatableMIGUsed, s.AllocatableMIG, strconv.Itoa(s.AllocatableMIGUsed), strconv.Itoa(s.AllocatableMIG), true),
		usageValueFor(s.MIGCapacity-s.AllocatableMIG, s.MIGCapacity, strconv.Itoa(s.MIGCapacity-s.AllocatableMIG), strconv.Itoa(s.MIGCapacity), false))
	cpuWidth := max(visibleWidth(cpuInfo), visibleWidth(cpuUnavailable))
	memWidth := max(visibleWidth(memInfo), visibleWidth(memUnavailable))
	cpuBox := statBox("CPU", coloredBar(cpu, s.AllocatableCPU, cpuWidth, enabled), cpuInfo, cpuUnavailable)
	memBox := statBox("MEM", coloredBar(mem, s.AllocatableMemoryMB, memWidth, enabled), memInfo, memUnavailable)
	h200Width := max(visibleWidth(h200Info), visibleWidth(h200Unavailable))
	migWidth := max(visibleWidth(migInfo), visibleWidth(migUnavailable))
	h200Info, h200Unavailable = fitANSI(h200Info, h200Width), fitANSI(h200Unavailable, h200Width)
	migInfo, migUnavailable = fitANSI(migInfo, migWidth), fitANSI(migUnavailable, migWidth)
	h200Label, migLabel := "H200 ", "H200-MIG "
	gpuStatus := h200Label + coloredBar(s.AllocatableH200Used, s.AllocatableH200, h200Width, enabled) + "  " + migLabel + coloredBar(s.AllocatableMIGUsed, s.AllocatableMIG, migWidth, enabled)
	gpuInfo := strings.Repeat(" ", visibleWidth(h200Label)) + h200Info + "  " + strings.Repeat(" ", visibleWidth(migLabel)) + migInfo
	gpuUnavailable := strings.Repeat(" ", visibleWidth(h200Label)) + h200Unavailable + "  " + strings.Repeat(" ", visibleWidth(migLabel)) + migUnavailable
	gpuBox := statBox("GPU", gpuStatus, gpuInfo, gpuUnavailable)
	all := joinStatBoxes(gpuBox, cpuBox, memBox)
	if width >= visibleWidth(all[0]) {
		return all
	}
	return append(gpuBox, joinStatBoxes(cpuBox, memBox)...)
}
func wrapPaneText(text string, width int) []string {
	if width < 1 {
		return []string{""}
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	lines := []string{}
	line := ""
	for _, word := range words {
		if visibleWidth(word) > width {
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			for len([]rune(word)) > width {
				lines = append(lines, string([]rune(word)[:width]))
				word = string([]rune(word)[width:])
			}
		}
		if line == "" {
			line = word
		} else if visibleWidth(line)+1+visibleWidth(word) <= width {
			line += " " + word
		} else {
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func jobRunsOnNode(nodes, node string) bool {
	for len(nodes) > 0 {
		part, rest, found := strings.Cut(nodes, ",")
		if open := strings.IndexByte(part, '['); open >= 0 {
			// A comma inside a hostlist bracket belongs to the same expression.
			if close := strings.IndexByte(nodes[open:], ']'); close >= 0 {
				end := open + close
				part = nodes[:end+1]
				if end+1 < len(nodes) && nodes[end+1] == ',' {
					rest = nodes[end+2:]
				} else {
					rest = nodes[end+1:]
				}
				found = true
			}
		}
		if part == node {
			return true
		}
		open, close := strings.IndexByte(part, '['), strings.IndexByte(part, ']')
		if open >= 0 && close > open {
			prefix, suffix := part[:open], part[close+1:]
			for _, span := range strings.Split(part[open+1:close], ",") {
				first, last, ranged := strings.Cut(span, "-")
				if !ranged {
					last = first
				}
				start, e1 := strconv.Atoi(first)
				end, e2 := strconv.Atoi(last)
				if e1 != nil || e2 != nil {
					continue
				}
				padding := max(len(first), len(last))
				for i := start; i <= end; i++ {
					if node == prefix+fmt.Sprintf("%0*d", padding, i)+suffix {
						return true
					}
				}
			}
		}
		if !found {
			break
		}
		nodes = rest
	}
	return false
}

type nodeBarContent struct {
	heading, status, info, unavailable string
}

func nodeResourceContent(label string, used, usable, total int, usedText, usableText, unavailableText, totalText string, color bool) nodeBarContent {
	info, unavailable := usagePair(
		usageValueFor(used, usable, usedText, usableText, true),
		usageValueFor(total-usable, total, unavailableText, totalText, false))
	barWidth := max(visibleWidth(info), visibleWidth(unavailable))
	return nodeBarContent{
		status:      label + coloredBar(used, usable, barWidth, color),
		info:        strings.Repeat(" ", visibleWidth(label)) + info,
		unavailable: strings.Repeat(" ", visibleWidth(label)) + unavailable,
	}
}

type nodeGPUStatus struct {
	label             string
	used, usable, all int
}

func nodeGPUStatuses(n Node, available bool) []nodeGPUStatus {
	if n.GPUTotal == 0 {
		return nil
	}
	h200Total, migTotal := n.h200Total, n.migTotal
	if h200Total+migTotal == 0 {
		classifyGPU(n.GPUType, n.GPUTotal, &h200Total, &migTotal, new(int))
	}
	if n.GPUAllocated > 0 && !n.gpuAllocationTyped {
		return []nodeGPUStatus{{"", n.GPUAllocated, n.GPUTotal, n.GPUTotal}}
	}
	statuses := []nodeGPUStatus{}
	if h200Total > 0 {
		statuses = append(statuses, nodeGPUStatus{"H200 ", n.h200Allocated, h200Total, h200Total})
	}
	if migTotal > 0 {
		statuses = append(statuses, nodeGPUStatus{"H200-MIG ", n.migAllocated, migTotal, migTotal})
	}
	if otherTotal := n.GPUTotal - h200Total - migTotal; otherTotal > 0 {
		otherUsed := n.GPUAllocated - n.h200Allocated - n.migAllocated
		statuses = append(statuses, nodeGPUStatus{"GPU ", max(0, otherUsed), otherTotal, otherTotal})
	}
	if len(statuses) == 0 {
		statuses = append(statuses, nodeGPUStatus{"", n.GPUAllocated, n.GPUTotal, n.GPUTotal})
	}
	if !available {
		for i := range statuses {
			statuses[i].used, statuses[i].usable = 0, 0
		}
	}
	return statuses
}

func nodeGPUContent(n Node, available, color bool) nodeBarContent {
	statuses := nodeGPUStatuses(n, available)
	parts := make([]nodeBarContent, 0, len(statuses))
	for _, status := range statuses {
		parts = append(parts, nodeResourceContent(status.label, status.used, status.usable, status.all, strconv.Itoa(status.used), strconv.Itoa(status.usable), strconv.Itoa(status.all-status.usable), strconv.Itoa(status.all), color))
	}
	content := nodeBarContent{heading: "GPU"}
	for i, part := range parts {
		if i > 0 {
			content.status += "  "
			content.info += "  "
			content.unavailable += "  "
		}
		content.status += part.status
		content.info += part.info
		content.unavailable += part.unavailable
	}
	return content
}

// nodeStatusBars mirrors the cluster summary for one node inside one
// allocation box. The final row is capacity unavailable for scheduling.
func nodeStatusBars(n Node, _ int, color bool) []string {
	available := !unavailable(n.State)
	cpuUsable, memUsable := n.CPUTotal, n.MemoryTotalMB
	cpuUsed, memUsed := n.CPUAllocated, n.MemoryAllocatedMB
	if !available {
		cpuUsable, memUsable, cpuUsed, memUsed = 0, 0, 0, 0
	}
	contents := []nodeBarContent{}
	if n.GPUTotal > 0 {
		contents = append(contents, nodeGPUContent(n, available, color))
	}
	cpu := nodeResourceContent("", cpuUsed, cpuUsable, n.CPUTotal, strconv.Itoa(cpuUsed), strconv.Itoa(cpuUsable), strconv.Itoa(n.CPUTotal-cpuUsable), strconv.Itoa(n.CPUTotal), color)
	cpu.heading = "CPU"
	mem := nodeResourceContent("", memUsed, memUsable, n.MemoryTotalMB, gb(memUsed), gb(memUsable)+" GB", gb(n.MemoryTotalMB-memUsable), gb(n.MemoryTotalMB)+" GB", color)
	mem.heading = "MEM"
	contents = append(contents, cpu, mem)
	lines := nodeBarContent{}
	for i, content := range contents {
		if i > 0 {
			lines.heading += "  "
			lines.status += "  "
			lines.info += "  "
			lines.unavailable += "  "
		}
		contentWidth := visibleWidth(content.status)
		heading := strings.Repeat(" ", max(0, contentWidth-visibleWidth(content.heading))) + content.heading
		lines.heading += fitANSI(heading, contentWidth)
		lines.status += content.status
		lines.info += content.info
		lines.unavailable += content.unavailable
	}
	return statBox("ALLOCATED", lines.heading, lines.status, lines.info, lines.unavailable)
}

func nodeDetailsTitle(n Node) string {
	title := "Node: " + printable(n.Name) + "    State: " + printable(n.State)
	if !n.bootTime.IsZero() {
		title += "    Booted: " + n.bootTime.Format("15:04:05 on January 02, 2006")
	}
	return title
}

// nodeDetails renders a selected GPU node in the current pane rather than as
// an overlay. Its output always occupies the available pane height.
func nodeDetails(n Node, jobs []Job, width, height, scroll int, color bool) []string {
	width, height = max(1, width), max(1, height)
	title := nodeDetailsTitle(n)
	wrapped := []string{title}
	if visibleWidth(title) > width {
		wrapped = wrapPaneText(title, width)
	}
	// Leave one cell for the scroll indicator so it never overwrites a box border.
	bars := nodeStatusBars(n, max(1, width-1), color)
	if !color {
		for i := range bars {
			bars[i] = sgrPattern.ReplaceAllString(bars[i], "")
		}
	}
	wrapped = append(wrapped, bars...)
	if n.Reason != "" {
		wrapped = append(wrapped, wrapPaneText("Reason: "+printable(n.Reason), width)...)
	}
	for _, job := range jobs {
		if job.State == "RUNNING" && jobRunsOnNode(job.nodes, n.Name) {
			wrapped = append(wrapped, wrapPaneText(fmt.Sprintf("Job %d  User: %s  Account: %s  GPU: %d  CPU: %d  MEM: %s GB", job.ID, job.User, job.Account, job.GPUs, job.CPUs, gb(job.MemoryMB)), width)...)
		}
	}
	maxScroll := max(0, len(wrapped)-height)
	scroll = min(max(0, scroll), maxScroll)
	thumbRow := 0
	if maxScroll > 0 && height > 1 {
		thumbRow = scroll * (height - 1) / maxScroll
	}
	lines := make([]string, height)
	for i := range lines {
		text := ""
		if i+scroll < len(wrapped) {
			text = wrapped[i+scroll]
		}
		lineWidth := width
		marker := ""
		if maxScroll > 0 && width > 1 {
			lineWidth = width - 1
			marker = "░"
			if i == thumbRow {
				marker = "█"
			}
		}
		if color {
			lines[i] = fitANSI(text, lineWidth) + marker
		} else {
			lines[i] = fit(text, lineWidth) + marker
		}
	}
	return lines
}

func clickedTableRow(y, headerY, height, scroll, count int) (int, bool) {
	if y <= headerY || y >= height {
		return 0, false
	}
	row := scroll + y - headerY - 1
	return row, row >= 0 && row < count
}

func userJobStats(s Snapshot, user string) (accounts, running, pending int) {
	seen := map[string]struct{}{}
	for _, job := range s.Jobs {
		if job.User != user {
			continue
		}
		if job.Account != "" {
			seen[job.Account] = struct{}{}
		}
		switch job.State {
		case "RUNNING":
			running++
		case "PENDING":
			pending++
		}
	}
	return len(seen), running, pending
}

func runTopUI(in, out *os.File, interval, timeout time.Duration, initialSort string) error {
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) || os.Getenv("TERM") == "dumb" {
		return fmt.Errorf("interactive mode requires terminal stdin and stdout")
	}
	root, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	load := func() (Snapshot, error) {
		ctx, cancel := context.WithTimeout(root, timeout)
		defer cancel()
		return collectTop(ctx)
	}
	s, err := load()
	if err != nil {
		return err
	}
	saved, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(in.Fd()), saved)
	// Alternate screen, hide cursor, enable SGR mouse + wheel. Always restore.
	if _, err = fmt.Fprint(out, "\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h"); err != nil {
		return err
	}
	defer fmt.Fprint(out, "\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l")
	events := make(chan uiEvent, 64)
	go readTopEvents(in, events)
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	pane, user, by, asc, selected, scroll, message := "cluster", "", initialSort, false, 0, 0, ""
	summaryBy, summaryAsc, returnUser := initialSort, false, ""
	nodeSelected, nodeScroll, detailScroll, hScroll, tableWidth := 0, 0, 0, 0, 0
	nodeDetailsOpen := false
	headerFocused, selectedHeader, headerActivated := false, 0, false
	width, height, menuStartY, headerY, rows := 80, 24, 2, 5, 1
	ids := []string{}
	columns := []headerColumn{}
	draw := func() error {
		w, h, e := term.GetSize(int(out.Fd()))
		if e != nil || w < 1 || h < 1 {
			w, h = 80, 24
		}
		if w < 30 {
			w = 30
		}
		if h < 9 {
			h = 9
		}
		width, height = w, h
		colorBars := os.Getenv("NO_COLOR") == "" && os.Getenv("CLICOLOR") != "0"
		bars := horizontalBars(s, width-1, colorBars)
		menu := viewMenu(paneMenuLabel(pane))
		menuStartY = len(bars) + 2
		headerY = len(bars) + len(menu) + 3 // 1-based row of column headings
		rows = height - headerY - 1
		if rows < 1 {
			rows = 1
		}
		isTable := pane == "cluster" || pane == "user"
		var headers, items []string
		if isTable {
			tableUser := ""
			if pane == "user" {
				tableUser = user
			}
			var rowIDs []string
			var currentColumns []headerColumn
			headers, items, rowIDs, currentColumns = topRows(s, tableUser, by, asc, colorBars)
			ids, columns = rowIDs, currentColumns
			tableWidth = utf8.RuneCountInString(sgrPattern.ReplaceAllString(headers[0], ""))
			for _, item := range items {
				tableWidth = max(tableWidth, utf8.RuneCountInString(sgrPattern.ReplaceAllString(item, "")))
			}
			if pane == "cluster" {
				hScroll = 0
			} else {
				hScroll = min(hScroll, max(0, tableWidth-(width-1)))
			}
			if selectedHeader >= len(columns) {
				selectedHeader = len(columns) - 1
			}
			if selectedHeader < 0 {
				selectedHeader = 0
			}
			if pane == "cluster" && returnUser != "" {
				for i, id := range rowIDs {
					if id == returnUser {
						selected = i
						break
					}
				}
				returnUser = ""
			}
			if len(items) == 0 {
				selected, scroll = 0, 0
			} else {
				if selected >= len(items) {
					selected = len(items) - 1
				}
				if selected < 0 {
					selected = 0
				}
				if scroll > selected {
					scroll = selected
				}
				if selected >= scroll+rows {
					scroll = selected - rows + 1
				}
			}
		}
		var b bytes.Buffer
		b.WriteString("\x1b[H\x1b[2J")
		title := fmt.Sprintf("slurm-top  %s  %d users / %d accounts / %d running / %d pending", s.UpdatedAt.Format("15:04:05"), len(s.Users), s.ActiveAccounts, s.RunningJobs, s.PendingJobs)
		if pane == "user" {
			accounts, running, pending := userJobStats(s, user)
			title = fmt.Sprintf("slurm-top  %s  1 user / %d accounts / %d running / %d pending  %s", s.UpdatedAt.Format("15:04:05"), accounts, running, pending, user)
		}
		fmt.Fprint(&b, fit(title, width-1), "\r\n")
		for _, line := range bars {
			if colorBars {
				fmt.Fprint(&b, fitANSI(line, width-1), "\r\n")
			} else {
				fmt.Fprint(&b, fit(line, width-1), "\r\n")
			}
		}
		for _, line := range menu {
			fmt.Fprint(&b, fitANSI(line, width-1), "\r\n")
		}
		fmt.Fprint(&b, strings.Repeat("─", width-1), "\r\n")
		if isTable {
			header := headers[0]
			if headerFocused {
				header = highlightHeader(header, columns[selectedHeader])
			}
			offset := 0
			if pane == "user" {
				offset = hScroll
			}
			fmt.Fprint(&b, cropANSI(header, offset, width-1), "\r\n")
			for i := scroll; i < len(items) && i < scroll+rows; i++ {
				line := cropANSI(items[i], offset, width-1)
				if !headerFocused && i == selected {
					line = "\x1b[7m" + line + "\x1b[0m"
				}
				fmt.Fprint(&b, line, "\r\n")
			}
		} else {
			visibleRows := max(1, height-headerY)
			if pane == "gpu" {
				nodeSelected = min(nodeSelected, max(0, len(s.nodes)-1))
			}
			{
				var body bytes.Buffer
				if pane == "gpu" {
					if err := renderGridSelected(&body, s.nodes, time.Now(), width-1, 0, 0, colorBars, nodeSelected); err != nil {
						return err
					}
				} else if err := renderDashboard(&body, s.nodes, time.Now(), width-1, colorBars); err != nil {
					return err
				}
				bodyLines := bytes.Split(bytes.TrimSuffix(body.Bytes(), []byte("\n")), []byte("\n"))
				if pane == "gpu" {
					for i, line := range bodyLines {
						if bytes.Contains(line, []byte("\x1b[7m")) {
							if i < nodeScroll {
								nodeScroll = i
							} else if i >= nodeScroll+visibleRows {
								nodeScroll = i - visibleRows + 1
							}
							break
						}
					}
				}
				nodeScroll = min(nodeScroll, max(0, len(bodyLines)-visibleRows))
				if pane == "gpu" && nodeDetailsOpen && len(s.nodes) > 0 {
					for _, line := range nodeDetails(s.nodes[nodeSelected], s.Jobs, width-1, visibleRows, detailScroll, colorBars) {
						if colorBars {
							fmt.Fprint(&b, fitANSI(line, width-1), "\r\n")
						} else {
							fmt.Fprint(&b, fit(line, width-1), "\r\n")
						}
					}
				} else {
					for i := nodeScroll; i < len(bodyLines) && i < nodeScroll+visibleRows; i++ {
						fmt.Fprint(&b, fitANSI(string(bodyLines[i]), width-1), "\r\n")
					}
				}
			}
		}
		footer := "Tab/Shift-Tab views  ↑ header/rows  ←/→ header columns  Enter sort  → user jobs  q quit"
		if pane == "user" {
			footer = "Jobs: " + user + "  Tab/Shift-Tab views  ←/→ scroll table (← users at left edge)  Esc users  q quit"
		} else if pane == "gpu" && nodeDetailsOpen {
			footer = "Node details  ↑/↓ scroll  Esc GPU grid  q quit"
		} else if pane == "gpu" {
			footer = "Tab/Shift-Tab views  ↑/↓ select node  Enter details  r refresh  q quit"
		} else if pane == "node" {
			footer = "Tab/Shift-Tab views  ↑/↓ scroll  r refresh  q quit"
		}
		if message != "" {
			footer += "  " + message
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s", height, fit(footer, width-1))
		_, e = out.Write(b.Bytes())
		return e
	}
	if err := draw(); err != nil {
		return err
	}
	for {
		select {
		case <-root.Done():
			return nil
		case <-resize:
			if err := draw(); err != nil {
				return err
			}
		case ev := <-events:
			tablePane := pane == "cluster" || pane == "user"
			switch ev.key {
			case "q", "Q", "\x03":
				return nil
			case "\t", "shift-tab":
				pane = cycleTopPane(pane, ev.key == "shift-tab")
				headerFocused, headerActivated = false, false
				nodeDetailsOpen, nodeScroll = false, 0
			case "n", " ":
				if pane == "gpu" && nodeDetailsOpen {
					detailScroll += 5
				} else if pane == "gpu" || pane == "node" {
					nodeScroll += 5
				} else {
					continue
				}
			case "p":
				if pane == "gpu" && nodeDetailsOpen {
					detailScroll = max(0, detailScroll-5)
				} else if pane == "gpu" || pane == "node" {
					nodeScroll = max(0, nodeScroll-5)
				} else {
					continue
				}
			case "down", "j":
				if !tablePane {
					if pane == "gpu" && nodeDetailsOpen {
						detailScroll++
					} else if pane == "gpu" {
						nodeSelected = min(nodeSelected+gridColumns(width-1), max(0, len(s.nodes)-1))
					} else {
						nodeScroll++
					}
					break
				}
				if ev.key == "j" && pane == "cluster" && !headerFocused {
					by = "jobs"
					asc = false
					selected = 0
					scroll = 0
				} else if headerFocused {
					headerFocused, headerActivated = false, false
					selected = 0
				} else {
					selected++
				}
			case "up", "k":
				if !tablePane {
					if pane == "gpu" && nodeDetailsOpen {
						detailScroll = max(0, detailScroll-1)
					} else if pane == "gpu" {
						nodeSelected = max(0, nodeSelected-gridColumns(width-1))
					} else {
						nodeScroll = max(0, nodeScroll-1)
					}
					break
				}
				if selected == 0 {
					if !headerFocused {
						headerActivated = false
					}
					headerFocused = true
				} else {
					selected--
				}
			case "right":
				if !tablePane {
					if pane == "gpu" && !nodeDetailsOpen {
						columns := gridColumns(width - 1)
						if nodeSelected%columns < columns-1 && nodeSelected+1 < len(s.nodes) {
							nodeSelected++
						}
					} else {
						continue
					}
				} else if headerFocused {
					if selectedHeader < len(columns)-1 {
						selectedHeader++
						headerActivated = false
					}
				} else if pane == "cluster" && selected < len(ids) {
					summaryBy, summaryAsc = by, asc
					user, pane = ids[selected], "user"
					by, asc = "elapsed", false
					selected, scroll, hScroll = 0, 0, 0
				} else if pane == "user" {
					hScroll = min(hScroll+max(1, (width-1)/2), max(0, tableWidth-(width-1)))
				}
			case "\r", "\n":
				if !tablePane {
					if pane == "gpu" && !nodeDetailsOpen && len(s.nodes) > 0 {
						nodeDetailsOpen, detailScroll = true, 0
					} else {
						continue
					}
					break
				}
				if headerFocused {
					field := columns[selectedHeader].field
					if headerActivated && field == by {
						asc = !asc
					} else {
						by, asc = field, false
					}
					headerActivated = true
					selected, scroll = 0, 0
				} else if pane == "cluster" && selected < len(ids) {
					summaryBy, summaryAsc = by, asc
					user, pane = ids[selected], "user"
					by, asc = "elapsed", false
					selected, scroll = 0, 0
				}
			case "escape":
				if pane == "gpu" && nodeDetailsOpen {
					nodeDetailsOpen, detailScroll = false, 0
				} else if pane == "user" {
					returnUser, pane = user, "cluster"
					by, asc = summaryBy, summaryAsc
					selected, scroll, hScroll = 0, 0, 0
				}
			case "left":
				if !tablePane {
					if pane == "gpu" && !nodeDetailsOpen {
						columns := gridColumns(width - 1)
						if nodeSelected%columns > 0 {
							nodeSelected--
						}
					} else {
						continue
					}
				} else if headerFocused && selectedHeader > 0 {
					selectedHeader--
					headerActivated = false
				} else if pane == "user" && hScroll > 0 {
					hScroll = max(0, hScroll-max(1, (width-1)/2))
				} else if pane == "user" {
					returnUser, pane = user, "cluster"
					by, asc = summaryBy, summaryAsc
					selected, scroll, hScroll = 0, 0, 0
				}
			case "g", "c", "m", "u":
				if pane == "cluster" {
					by = map[string]string{"g": "gpu", "c": "cpu", "m": "mem", "u": "user"}[ev.key]
					asc = false
					selected, scroll = 0, 0
				}
			case "r":
				updated, e := load()
				if root.Err() != nil {
					return nil
				}
				if e != nil {
					message = e.Error()
				} else {
					s = updated
					message = ""
				}
			case "click":
				if ev.y >= menuStartY && ev.y < menuStartY+len(viewMenu(paneMenuLabel(pane))) {
					for _, column := range menuColumns() {
						if ev.x >= column.start && ev.x <= column.end {
							pane = column.field
							headerFocused, headerActivated = false, false
							nodeDetailsOpen, nodeScroll = false, 0
							break
						}
					}
				} else if pane == "gpu" && !nodeDetailsOpen {
					if index := gridNodeAt(s.nodes, width-1, nodeScroll+ev.y-headerY, ev.x); index >= 0 {
						nodeSelected = index
						nodeDetailsOpen, detailScroll = true, 0
					} else {
						continue
					}
				} else if !tablePane {
					continue
				} else if ev.y == headerY {
					field := ""
					x := ev.x + hScroll
					for _, column := range columns {
						if x >= column.start && x <= column.end {
							field = column.field
							break
						}
					}
					if field != "" {
						for i, column := range columns {
							if column.field == field {
								selectedHeader = i
								break
							}
						}
						headerFocused, headerActivated = true, true
						if field == by {
							asc = !asc
						} else {
							by = field
							asc = false
						}
						selected = 0
						scroll = 0
					}
				}
				if tablePane {
					if row, ok := clickedTableRow(ev.y, headerY, height, scroll, len(ids)); ok {
						headerFocused, headerActivated = false, false
						selected = row
						if pane == "cluster" {
							summaryBy, summaryAsc = by, asc
							user, pane = ids[selected], "user"
							by, asc = "elapsed", false
							selected, scroll, hScroll = 0, 0, 0
						}
					}
				}
			default:
				if ev.wheel != 0 && tablePane {
					headerFocused, headerActivated = false, false
					selected += ev.wheel
				} else if ev.wheel != 0 && pane == "gpu" && nodeDetailsOpen {
					detailScroll = max(0, detailScroll+ev.wheel)
				} else if ev.wheel != 0 && pane == "node" {
					nodeScroll = max(0, nodeScroll+ev.wheel)
				} else {
					continue
				}
			}
			if selected < 0 {
				selected = 0
			}
			if len(ids) > 0 && selected >= len(ids) {
				selected = len(ids) - 1
			}
			if err := draw(); err != nil {
				return err
			}
		case <-ticker.C:
			id := ""
			if selected < len(ids) {
				id = ids[selected]
			}
			updated, e := load()
			if root.Err() != nil {
				return nil
			}
			if e != nil {
				message = e.Error()
			} else {
				s = updated
				message = ""
				tableUser := ""
				if pane == "user" {
					tableUser = user
				}
				_, _, next, _ := topRows(s, tableUser, by, asc)
				for i, item := range next {
					if item == id {
						selected = i
						break
					}
				}
			}
			if err := draw(); err != nil {
				return err
			}
		}
	}
}
