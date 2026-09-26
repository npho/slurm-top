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
	var buf [1]byte
	state := 0
	var seq strings.Builder
	send := func(e uiEvent) {
		select {
		case events <- e:
		default:
		}
	}
	for {
		if _, err := in.Read(buf[:]); err != nil {
			return
		}
		b := buf[0]
		switch state {
		case 0:
			if b == 27 {
				state = 1
			} else {
				send(uiEvent{key: string(b)})
			}
		case 1:
			if b == '[' {
				state = 2
				seq.Reset()
			} else {
				state = 0
			}
		case 2:
			if b == 'A' || b == 'B' || b == 'C' || b == 'D' {
				send(uiEvent{key: map[byte]string{'A': "up", 'B': "down", 'C': "right", 'D': "left"}[b]})
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
			if columns[j].field == "elapsed" {
				parts[j] = padding + value
			} else {
				parts[j] = value + padding
			}
		}
		lines[i] = strings.Join(parts, "  ")
	}
	return strings.Join(headerParts, "  "), lines, headerColumns
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
		rows = append(rows, []string{strconv.Itoa(j.ID), j.Account, j.QoS, progressBar(j.Progress, 10, colorEnabled), j.Elapsed, j.Partition, strconv.Itoa(j.GPUs), strconv.Itoa(j.CPUs), cpuGPU(j.CPUs, j.GPUs), gb(j.MemoryMB), memoryCPU(j.MemoryMB, j.CPUs), j.Name})
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
	return header[:start] + "\x1b[7m" + header[start:end] + "\x1b[0m" + header[end:]
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
	all := joinStatBoxes(cpuBox, memBox, gpuBox)
	if width >= visibleWidth(all[0]) {
		return all
	}
	return append(joinStatBoxes(cpuBox, memBox), gpuBox...)
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
	user, by, asc, selected, scroll, message := "", initialSort, false, 0, 0, ""
	summaryBy, summaryAsc, returnUser := initialSort, false, ""
	hScroll, tableWidth := 0, 0
	headerFocused, selectedHeader, headerActivated := false, 0, false
	width, height, headerY, rows := 80, 24, 5, 1
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
		headerY = 3 + len(bars) // 1-based row of column headings
		rows = height - headerY - 1
		if rows < 1 {
			rows = 1
		}
		headers, items, rowIDs, currentColumns := topRows(s, user, by, asc, colorBars)
		ids, columns = rowIDs, currentColumns
		tableWidth = utf8.RuneCountInString(sgrPattern.ReplaceAllString(headers[0], ""))
		for _, item := range items {
			tableWidth = max(tableWidth, utf8.RuneCountInString(sgrPattern.ReplaceAllString(item, "")))
		}
		if user == "" {
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
		if user == "" && returnUser != "" {
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
		var b bytes.Buffer
		b.WriteString("\x1b[H\x1b[2J")
		title := fmt.Sprintf("slurm-top  %s  %d users / %d accounts / %d running / %d pending", s.UpdatedAt.Format("15:04:05"), len(s.Users), s.ActiveAccounts, s.RunningJobs, s.PendingJobs)
		if user != "" {
			title += "  " + user
		}
		fmt.Fprint(&b, fit(title, width-1), "\r\n")
		for _, line := range bars {
			if colorBars {
				fmt.Fprint(&b, fitANSI(line, width-1), "\r\n")
			} else {
				fmt.Fprint(&b, fit(line, width-1), "\r\n")
			}
		}
		fmt.Fprint(&b, "\r\n")
		header := headers[0]
		if headerFocused {
			header = highlightHeader(header, columns[selectedHeader])
		}
		offset := 0
		if user != "" {
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
		footer := "↑ header/rows  ←/→ header columns  Enter sort  → jobs  ← users  click header sort  q quit"
		if user != "" {
			footer = "Jobs: " + user + "  ↑ header/rows  ←/→ scroll table (← users at left edge)  Enter sort  q quit"
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
			switch ev.key {
			case "q", "Q", "\x03":
				return nil
			case "down", "j":
				if ev.key == "j" && user == "" && !headerFocused {
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
				if selected == 0 {
					if !headerFocused {
						headerActivated = false
					}
					headerFocused = true
				} else {
					selected--
				}
			case "right":
				if headerFocused {
					if selectedHeader < len(columns)-1 {
						selectedHeader++
						headerActivated = false
					}
				} else if user == "" && selected < len(ids) {
					summaryBy, summaryAsc = by, asc
					user = ids[selected]
					by = "elapsed"
					asc = false
					selected, scroll, hScroll = 0, 0, 0
				} else if user != "" {
					hScroll = min(hScroll+max(1, (width-1)/2), max(0, tableWidth-(width-1)))
				}
			case "\r", "\n":
				if headerFocused {
					field := columns[selectedHeader].field
					if headerActivated && field == by {
						asc = !asc
					} else {
						by = field
						asc = false
					}
					headerActivated = true
					selected = 0
					scroll = 0
				} else if user == "" && selected < len(ids) {
					summaryBy, summaryAsc = by, asc
					user = ids[selected]
					by = "elapsed"
					asc = false
					selected = 0
					scroll = 0
				}
			case "left":
				if headerFocused && selectedHeader > 0 {
					selectedHeader--
					headerActivated = false
				} else if user != "" && hScroll > 0 {
					hScroll = max(0, hScroll-max(1, (width-1)/2))
				} else if user != "" {
					returnUser = user
					user = ""
					by, asc = summaryBy, summaryAsc
					selected, scroll, hScroll = 0, 0, 0
				}
			case "g", "c", "m", "u":
				if user == "" {
					by = map[string]string{"g": "gpu", "c": "cpu", "m": "mem", "u": "user"}[ev.key]
					asc = false
					selected = 0
					scroll = 0
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
				if ev.y == headerY {
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
				if ev.y > headerY && ev.y < height {
					headerFocused, headerActivated = false, false
					selected = scroll + ev.y - headerY - 1
				}
			default:
				if ev.wheel != 0 {
					headerFocused, headerActivated = false, false
					selected += ev.wheel
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
				_, _, next, _ := topRows(s, user, by, asc)
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
