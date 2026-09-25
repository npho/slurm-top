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
		case "progress":
			cmp = a.Progress - b.Progress
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

func topRows(s Snapshot, user, by string, asc bool, color ...bool) ([]string, []string, []string) {
	colorEnabled := len(color) > 0 && color[0]
	if user == "" {
		users := append([]Usage(nil), s.Users...)
		sortUsers(users, by)
		if asc {
			for i, j := 0, len(users)-1; i < j; i, j = i+1, j-1 {
				users[i], users[j] = users[j], users[i]
			}
		}
		lines := make([]string, 0, len(users))
		ids := make([]string, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.User)
			lines = append(lines, fmt.Sprintf("%-16.16s %6d %6d %6s %6d  %6d %6d %6s %6d", u.User, u.RunningJobs, u.CPUs, gb(u.MemoryMB), u.GPUs, u.PendingJobs, u.PendingCPUs, gb(u.PendingMemoryMB), u.PendingGPUs))
		}
		return []string{userHeader(by, asc)}, lines, ids
	}
	jobs := []Job{}
	for _, j := range s.Jobs {
		if j.User == user {
			jobs = append(jobs, j)
		}
	}
	sortJobs(jobs, by, asc)
	lines := make([]string, 0, len(jobs))
	ids := make([]string, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, strconv.Itoa(j.ID)+"/"+j.State)
		lines = append(lines, fmt.Sprintf("%-11d %-14.14s %-10.10s %-10s %8d %8d %10s  %s", j.ID, j.Account, j.QoS, progressBar(j.Progress, 10, colorEnabled), j.CPUs, j.GPUs, gb(j.MemoryMB), j.Name))
	}
	header := strings.Join([]string{
		leftHeader("JOB ID", "id", by, asc, 11),
		leftHeader("ACCOUNT", "account", by, asc, 14),
		leftHeader("QOS", "qos", by, asc, 10),
		leftHeader("PROGRESS", "progress", by, asc, 10),
		leftHeader("CPU", "cpu", by, asc, 8),
		leftHeader("GPU", "gpu", by, asc, 8),
		leftHeader("MEM", "mem", by, asc, 10),
		leftHeader("NAME", "name", by, asc, 20),
	}, " ")
	return []string{header}, lines, ids
}

type headerColumn struct {
	field      string
	start, end int // 1-based, inclusive terminal columns
}

func headerColumns(user string) []headerColumn {
	if user == "" {
		return []headerColumn{
			{"user", 1, 16}, {"jobs", 18, 23}, {"cpu", 25, 30}, {"mem", 32, 37},
			{"gpu", 39, 44}, {"pending-jobs", 48, 53}, {"pending-cpu", 55, 60},
			{"pending-mem", 62, 67}, {"pending-gpu", 69, 74},
		}
	}
	return []headerColumn{
		{"id", 1, 11}, {"account", 13, 26}, {"qos", 28, 37}, {"progress", 39, 48},
		{"cpu", 50, 57}, {"gpu", 59, 66}, {"mem", 68, 77}, {"name", 80, 9999},
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

func horizontalBars(s Snapshot, width int, colored ...bool) []string {
	cpu, mem := 0, 0
	for _, u := range s.Users {
		cpu += u.CPUs
		mem += u.MemoryMB
	}
	enabled := len(colored) > 0 && colored[0]
	segment := func(label string, n, total int) string {
		return fmt.Sprintf("%s %s %d/%d", label, coloredBar(n, total, 8, enabled), n, total)
	}
	c := segment("CPU", cpu, s.CapacityCPU)
	m := fmt.Sprintf("MEM %s %s/%s TB", coloredBar(mem, s.CapacityMemoryMB, 8, enabled), tb(mem), tb(s.CapacityMemoryMB))
	h := segment("H200", s.H200Allocated, s.H200Capacity)
	mig := segment("H200-MIG", s.MIGAllocated, s.MIGCapacity)
	visible := func(v string) int { return utf8.RuneCountInString(sgrPattern.ReplaceAllString(v, "")) }
	if width >= visible(c)+visible(m)+visible(h)+visible(mig)+6 {
		return []string{c + "  " + m + "  " + h + "  " + mig}
	}
	return []string{c + "  " + m, h + "  " + mig}
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
	headerFocused, headerColumn, headerActivated := false, 0, false
	width, height, headerY, rows := 80, 24, 5, 1
	ids := []string{}
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
		headers, items, rowIDs := topRows(s, user, by, asc, colorBars)
		ids = rowIDs
		columns := headerColumns(user)
		if headerColumn >= len(columns) {
			headerColumn = len(columns) - 1
		}
		if headerColumn < 0 {
			headerColumn = 0
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
		title := fmt.Sprintf("slurm-top  %s  %d running / %d pending jobs", s.UpdatedAt.Format("15:04:05"), s.RunningJobs, s.PendingJobs)
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
			header = highlightHeader(header, columns[headerColumn])
			fmt.Fprint(&b, fitANSI(header, width-1), "\r\n")
		} else {
			fmt.Fprint(&b, fit(header, width-1), "\r\n")
		}
		for i := scroll; i < len(items) && i < scroll+rows; i++ {
			// Job progress bars contain ANSI color codes; pad by terminal cells,
			// not bytes, so they do not truncate the rest of a selected row.
			line := fitANSI(items[i], width-1)
			if !headerFocused && i == selected {
				line = "\x1b[7m" + line + "\x1b[0m"
			}
			fmt.Fprint(&b, line, "\r\n")
		}
		footer := "↑ header/rows  ←/→ header columns  Enter sort  → jobs  ← users  click header sort  q quit"
		if user != "" {
			footer = "Jobs: " + user + "  ↑ header/rows  ←/→ header columns  Enter sort  ← users  q quit"
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
					if headerColumn < len(headerColumns(user))-1 {
						headerColumn++
						headerActivated = false
					}
				} else if user == "" && selected < len(ids) {
					user = ids[selected]
					by = "id"
					asc = false
					selected = 0
					scroll = 0
				}
			case "\r", "\n":
				if headerFocused {
					field := headerColumns(user)[headerColumn].field
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
					user = ids[selected]
					by = "id"
					asc = false
					selected = 0
					scroll = 0
				}
			case "left":
				if headerFocused {
					if headerColumn > 0 {
						headerColumn--
						headerActivated = false
					}
				} else if user != "" {
					user = ""
					by = initialSort
					selected = 0
					scroll = 0
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
					field := headerSort(ev.x, user)
					if field != "" {
						for i, column := range headerColumns(user) {
							if column.field == field {
								headerColumn = i
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
				_, _, next := topRows(s, user, by, asc)
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
