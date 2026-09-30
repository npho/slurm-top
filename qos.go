package main

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type qosGroup struct {
	name            string
	runningJobs     int
	pendingJobs     int
	totalJobs       int
	allocCPUs       int
	allocGPUs       int
	allocMemMB      int
	pendingCPUs     int
	pendingGPUs     int
	pendingMemMB    int
	minElapsedMin   int
	maxElapsedMin   int
	avgElapsedMin   int
	totalElapsedMin int
	avgProgress     int
	totalProgress   int
	users           map[string]int
	accounts        map[string]int
	partitions      map[string]int
	nodes           map[string]int
	jobs            []Job
	limits          QoSLimit
	hasLimits       bool
}

func collectQoS(snap Snapshot) []qosGroup {
	byQoS := make(map[string]*qosGroup)
	var order []string

	getOrCreate := func(name string) *qosGroup {
		group := byQoS[name]
		if group == nil {
			group = &qosGroup{
				name:          name,
				users:         make(map[string]int),
				accounts:      make(map[string]int),
				partitions:    make(map[string]int),
				nodes:         make(map[string]int),
				minElapsedMin: -1,
			}
			if lim, ok := snap.qosLimits[name]; ok {
				group.limits = lim
				group.hasLimits = true
			}
			byQoS[name] = group
			order = append(order, name)
		}
		return group
	}

	for name, lim := range snap.qosLimits {
		if name == "" {
			continue
		}
		g := getOrCreate(name)
		g.limits = lim
		g.hasLimits = true
	}

	for _, j := range snap.Jobs {
		qosName := j.QoS
		if qosName == "" {
			qosName = "(default)"
		}

		group := getOrCreate(qosName)
		group.totalJobs++
		group.jobs = append(group.jobs, j)
		if j.User != "" {
			group.users[j.User]++
		}
		if j.Account != "" {
			group.accounts[j.Account]++
		}
		if j.Partition != "" {
			group.partitions[j.Partition]++
		}

		if j.State == "RUNNING" {
			group.runningJobs++
			group.allocCPUs += j.CPUs
			group.allocGPUs += j.GPUs
			group.allocMemMB += j.MemoryMB
			if j.ElapsedMinutes >= 0 {
				if group.minElapsedMin < 0 || j.ElapsedMinutes < group.minElapsedMin {
					group.minElapsedMin = j.ElapsedMinutes
				}
				if j.ElapsedMinutes > group.maxElapsedMin {
					group.maxElapsedMin = j.ElapsedMinutes
				}
				group.totalElapsedMin += j.ElapsedMinutes
			}
			group.totalProgress += j.Progress

			nodeMatched := false
			for _, n := range snap.nodes {
				if jobRunsOnNode(j.nodes, n.Name) {
					group.nodes[n.Name]++
					nodeMatched = true
				}
			}
			if !nodeMatched && j.nodes != "" && j.nodes != "(none)" {
				group.nodes[j.nodes]++
			}
		} else {
			group.pendingJobs++
			group.pendingCPUs += j.CPUs
			group.pendingGPUs += j.GPUs
			group.pendingMemMB += j.MemoryMB
		}
	}

	slices.Sort(order)
	result := make([]qosGroup, 0, len(order))
	for _, name := range order {
		g := *byQoS[name]
		if g.runningJobs > 0 {
			g.avgElapsedMin = g.totalElapsedMin / g.runningJobs
			g.avgProgress = g.totalProgress / g.runningJobs
		} else {
			g.minElapsedMin = 0
		}
		// Sort jobs: running first (by elapsed desc), then pending (by id)
		slices.SortStableFunc(g.jobs, func(a, b Job) int {
			if a.State != b.State {
				if a.State == "RUNNING" {
					return -1
				}
				return 1
			}
			if a.State == "RUNNING" {
				return b.ElapsedMinutes - a.ElapsedMinutes
			}
			return a.ID - b.ID
		})
		result = append(result, g)
	}
	return result
}

func sortQoSGroups(groups []qosGroup, by string) {
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		switch by {
		case "cpu-gpu":
			left, right := ratioSortValue(a.allocCPUs, a.allocGPUs), ratioSortValue(b.allocCPUs, b.allocGPUs)
			if left != right {
				return left > right
			}
		case "memory-cpu":
			left, right := ratioSortValue(a.allocMemMB, a.allocCPUs), ratioSortValue(b.allocMemMB, b.allocCPUs)
			if left != right {
				return left > right
			}
		case "pending-cpu-gpu":
			left, right := ratioSortValue(a.pendingCPUs, a.pendingGPUs), ratioSortValue(b.pendingCPUs, b.pendingGPUs)
			if left != right {
				return left > right
			}
		case "pending-memory-cpu":
			left, right := ratioSortValue(a.pendingMemMB, a.pendingCPUs), ratioSortValue(b.pendingMemMB, b.pendingCPUs)
			if left != right {
				return left > right
			}
		}
		var left, right int
		switch by {
		case "cpu":
			left, right = a.allocCPUs, b.allocCPUs
		case "mem":
			left, right = a.allocMemMB, b.allocMemMB
		case "jobs":
			left, right = a.runningJobs, b.runningJobs
		case "pending-cpu":
			left, right = a.pendingCPUs, b.pendingCPUs
		case "pending-mem":
			left, right = a.pendingMemMB, b.pendingMemMB
		case "pending-gpu":
			left, right = a.pendingGPUs, b.pendingGPUs
		case "pending-jobs":
			left, right = a.pendingJobs, b.pendingJobs
		case "cpu-gpu", "memory-cpu", "pending-cpu-gpu", "pending-memory-cpu":
			// Ratio ties fall back to QoS name below.
		case "qos", "account", "user":
			return a.name < b.name
		default:
			left, right = a.allocGPUs, b.allocGPUs
		}
		if left != right {
			return left > right
		}
		return a.name < b.name
	})
}

func qosRows(s Snapshot, by string, asc bool, color ...bool) ([]string, []string, []string, []headerColumn) {
	colorEnabled := len(color) > 0 && color[0]
	groups := collectQoS(s)
	sortQoSGroups(groups, by)
	if asc {
		for i, j := 0, len(groups)-1; i < j; i, j = i+1, j-1 {
			groups[i], groups[j] = groups[j], groups[i]
		}
	}
	columns := []tableColumn{
		{"qos", "QOS"},
		{"jobs", "RUN"},
		{"gpu", "GPU"},
		{"cpu", "CPU"},
		{"cpu-gpu", "C:G"},
		{"mem", "MEM"},
		{"memory-cpu", "M:C"},
		{"pending-jobs", "PEND"},
		{"pending-gpu", "GPU"},
		{"pending-cpu", "CPU"},
		{"pending-cpu-gpu", "C:G"},
		{"pending-mem", "MEM"},
		{"pending-memory-cpu", "M:C"},
	}
	rows := make([][]string, 0, len(groups))
	ids := make([]string, 0, len(groups))
	for _, q := range groups {
		ids = append(ids, q.name)
		rows = append(rows, []string{
			displayQoS(q.name, colorEnabled),
			strconv.Itoa(q.runningJobs),
			strconv.Itoa(q.allocGPUs),
			strconv.Itoa(q.allocCPUs),
			cpuGPU(q.allocCPUs, q.allocGPUs),
			gb(q.allocMemMB) + "G",
			memoryCPU(q.allocMemMB, q.allocCPUs),
			strconv.Itoa(q.pendingJobs),
			strconv.Itoa(q.pendingGPUs),
			strconv.Itoa(q.pendingCPUs),
			cpuGPU(q.pendingCPUs, q.pendingGPUs),
			gb(q.pendingMemMB) + "G",
			memoryCPU(q.pendingMemMB, q.pendingCPUs),
		})
	}
	header, lines, headerColumns := renderTable(columns, rows, by, asc)
	return []string{header}, lines, ids, headerColumns
}

func formatMapKeysWithCounts(m map[string]int, maxCount int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for i, k := range keys {
		if maxCount > 0 && i >= maxCount {
			parts = append(parts, fmt.Sprintf("+%d more", len(keys)-maxCount))
			break
		}
		if m[k] > 1 {
			parts = append(parts, fmt.Sprintf("%s (%d)", k, m[k]))
		} else {
			parts = append(parts, k)
		}
	}
	return strings.Join(parts, ", ")
}

func formatKeys(m map[string]int, maxCount int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for i, k := range keys {
		if maxCount > 0 && i >= maxCount {
			parts = append(parts, fmt.Sprintf("+%d more", len(keys)-maxCount))
			break
		}
		parts = append(parts, k)
	}
	return strings.Join(parts, ", ")
}

func formatQoSMem(mb int) string {
	if mb >= 1024*1024 {
		return tb(mb) + "T"
	}
	return gb(mb) + "G"
}

func renderQoSBox(q qosGroup, width int, color bool) []string {
	contentWidth := max(20, width-4)
	var content []string

	// State line
	stateLine := fmt.Sprintf("Jobs:        %d total  •  %d running  •  %d pending", q.totalJobs, q.runningJobs, q.pendingJobs)
	content = append(content, fitANSI(stateLine, contentWidth))

	// Allocations line (running)
	allocLine := fmt.Sprintf("Allocated:   %d GPUs  •  %d CPUs  •  %s memory", q.allocGPUs, q.allocCPUs, formatQoSMem(q.allocMemMB))
	content = append(content, fitANSI(allocLine, contentWidth))

	// Pending demand line
	reqLine := fmt.Sprintf("Pending Req: %d GPUs  •  %d CPUs  •  %s memory", q.pendingGPUs, q.pendingCPUs, formatQoSMem(q.pendingMemMB))
	content = append(content, fitANSI(reqLine, contentWidth))

	// Elapsed / Runtime line
	if q.runningJobs > 0 {
		elapsedLine := fmt.Sprintf("Elapsed:     Min: %s  •  Avg: %s  •  Max: %s  (across %d running)",
			formatElapsed(int64(q.minElapsedMin)*60),
			formatElapsed(int64(q.avgElapsedMin)*60),
			formatElapsed(int64(q.maxElapsedMin)*60),
			q.runningJobs)
		content = append(content, fitANSI(elapsedLine, contentWidth))

		progressLine := fmt.Sprintf("Progress:    Avg %d%%", q.avgProgress)
		content = append(content, fitANSI(progressLine, contentWidth))
	} else {
		content = append(content, fitANSI("Elapsed:     N/A (no running jobs)", contentWidth))
	}

	// Limits lines
	if q.hasLimits {
		var qosLimits []string
		if q.limits.GrpTRES != "" {
			qosLimits = append(qosLimits, "TRES: "+simplifyTRES(q.limits.GrpTRES))
		}
		if q.limits.GrpJobs != "" {
			qosLimits = append(qosLimits, "Max Jobs: "+q.limits.GrpJobs)
		}
		if q.limits.GrpSubmit != "" {
			qosLimits = append(qosLimits, "Max Submit: "+q.limits.GrpSubmit)
		}
		if q.limits.GrpWall != "" {
			qosLimits = append(qosLimits, "Max Wall: "+q.limits.GrpWall)
		}
		content = append(content, fitANSI(formatLimitLine("QoS Limits:  ", qosLimits), contentWidth))

		var acctLimits []string
		if q.limits.MaxTRESPA != "" {
			acctLimits = append(acctLimits, "Max TRES: "+simplifyTRES(q.limits.MaxTRESPA))
		}
		if q.limits.MaxJobsPA != "" {
			acctLimits = append(acctLimits, "Max Jobs: "+q.limits.MaxJobsPA)
		}
		if q.limits.MaxSubmitPA != "" {
			acctLimits = append(acctLimits, "Max Submit: "+q.limits.MaxSubmitPA)
		}
		content = append(content, fitANSI(formatLimitLine("Acct Limits: ", acctLimits), contentWidth))

		var userLimits []string
		if q.limits.MaxTRESPU != "" {
			userLimits = append(userLimits, "Max TRES: "+simplifyTRES(q.limits.MaxTRESPU))
		}
		if q.limits.MaxJobsPU != "" {
			userLimits = append(userLimits, "Max Jobs: "+q.limits.MaxJobsPU)
		}
		if q.limits.MaxSubmitPU != "" {
			userLimits = append(userLimits, "Max Submit: "+q.limits.MaxSubmitPU)
		}
		content = append(content, fitANSI(formatLimitLine("User Limits: ", userLimits), contentWidth))

		var jobLimits []string
		if q.limits.MaxTRES != "" {
			jobLimits = append(jobLimits, "Max TRES: "+simplifyTRES(q.limits.MaxTRES))
		}
		if q.limits.MinTRES != "" {
			jobLimits = append(jobLimits, "Min TRES: "+simplifyTRES(q.limits.MinTRES))
		}
		if q.limits.MaxTRESPerNode != "" {
			jobLimits = append(jobLimits, "Max/Node: "+simplifyTRES(q.limits.MaxTRESPerNode))
		}
		if q.limits.MaxWall != "" {
			jobLimits = append(jobLimits, "Max Wall: "+q.limits.MaxWall)
		}
		content = append(content, fitANSI(formatLimitLine("Job Limits:  ", jobLimits), contentWidth))
	}

	// Users line
	if len(q.users) > 0 {
		userLine := fmt.Sprintf("Users (%d):   %s", len(q.users), formatMapKeysWithCounts(q.users, 8))
		content = append(content, fitANSI(userLine, contentWidth))
	}

	// Accounts line
	if len(q.accounts) > 0 {
		acctLine := fmt.Sprintf("Accounts (%d):%s", len(q.accounts), " "+formatKeys(q.accounts, 8))
		content = append(content, fitANSI(acctLine, contentWidth))
	}

	// Partitions line
	if len(q.partitions) > 0 {
		partLine := fmt.Sprintf("Partitions:  %s", formatKeys(q.partitions, 8))
		content = append(content, fitANSI(partLine, contentWidth))
	}

	// Node line
	if len(q.nodes) > 0 {
		var nodeNames []string
		for k := range q.nodes {
			nodeNames = append(nodeNames, k)
		}
		nodeLine := fmt.Sprintf("Node:        %s", condenseNodes(nodeNames))
		content = append(content, fitANSI(nodeLine, contentWidth))
	} else if q.runningJobs > 0 {
		content = append(content, fitANSI("Node:        none assigned", contentWidth))
	} else {
		content = append(content, fitANSI("Node:        none", contentWidth))
	}

	// Active / Pending jobs list
	maxJobs := 8
	if len(q.jobs) > 0 {
		content = append(content, "")
		hdr := fmt.Sprintf("  %-7s %-12s %-8s %-22s %-10s %-4s %-4s %-6s %s",
			"JOB ID", "USER", "STATE", "ELAPSED/LIMIT", "PROGRESS", "GPU", "CPU", "MEM", "NODES")
		content = append(content, fitANSI(hdr, contentWidth))

		shown := min(len(q.jobs), maxJobs)
		for i := 0; i < shown; i++ {
			j := q.jobs[i]
			nodes := j.nodes
			if nodes == "" {
				nodes = "-"
			}
			stateStr := j.State
			if color {
				if j.State == "RUNNING" {
					stateStr = "\x1b[32mRUNNING\x1b[39m"
				} else if j.State == "PENDING" {
					stateStr = "\x1b[33mPENDING\x1b[39m"
				}
			}
			memStr := gb(j.MemoryMB) + "G"
			prog := progressBar(j.Progress, 8, color)
			row := fmt.Sprintf("  %-7d %-12s %-8s %-22s %-10s %-4d %-4d %-6s %s",
				j.ID, fit(j.User, 12), stateStr, fit(j.Elapsed, 22), prog, j.GPUs, j.CPUs, memStr, fit(nodes, 15))
			content = append(content, fitANSI(row, contentWidth))
		}
		if len(q.jobs) > maxJobs {
			content = append(content, fitANSI(fmt.Sprintf("  ... and %d more jobs", len(q.jobs)-maxJobs), contentWidth))
		}
	}

	boxLabel := displayQoS(q.name, color)
	box := statBox(boxLabel, content...)
	return widenStatBox(box, width)
}

func qosView(s Snapshot, width, height, scroll int, color bool) []string {
	width, height = max(10, width), max(1, height)

	targetWidth := clusterBoxesWidth(s)
	boxWidth := min(targetWidth, width)

	groups := collectQoS(s)
	var allLines []string

	renderAll := func(bw int) []string {
		var lines []string
		if len(groups) == 0 {
			box := statBox("QOS Overview", "No active or pending jobs found in queue.")
			lines = widenStatBox(box, bw)
		} else {
			for i, g := range groups {
				if i > 0 {
					lines = append(lines, "")
				}
				lines = append(lines, renderQoSBox(g, bw, color)...)
			}
		}
		return lines
	}

	allLines = renderAll(boxWidth)

	maxScroll := max(0, len(allLines)-height)
	if maxScroll > 0 && boxWidth >= width {
		boxWidth = max(10, width-1)
		allLines = renderAll(boxWidth)
		maxScroll = max(0, len(allLines)-height)
	}

	scroll = min(max(0, scroll), maxScroll)
	thumbRow := 0
	if maxScroll > 0 && height > 1 {
		thumbRow = scroll * (height - 1) / maxScroll
	}

	lines := make([]string, height)
	for i := range lines {
		text := ""
		if i+scroll < len(allLines) {
			text = allLines[i+scroll]
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
		if marker != "" {
			if visibleWidth(text) < lineWidth {
				text = text + strings.Repeat(" ", lineWidth-visibleWidth(text))
			} else {
				text = fitANSI(text, lineWidth)
			}
			lines[i] = text + marker
		} else {
			lines[i] = text
		}
	}
	return lines
}

func qosDetailView(s Snapshot, qosName string, width, height, scroll int, color bool) []string {
	width, height = max(10, width), max(1, height)

	targetWidth := clusterBoxesWidth(s)
	boxWidth := min(targetWidth, width)

	groups := collectQoS(s)
	var target *qosGroup
	for i := range groups {
		if groups[i].name == qosName {
			target = &groups[i]
			break
		}
	}

	renderAll := func(bw int) []string {
		if target == nil {
			box := statBox("QOS: "+qosName, "QoS not found.")
			return widenStatBox(box, bw)
		}
		return renderQoSBox(*target, bw, color)
	}

	allLines := renderAll(boxWidth)

	maxScroll := max(0, len(allLines)-height)
	if maxScroll > 0 && boxWidth >= width {
		boxWidth = max(10, width-1)
		allLines = renderAll(boxWidth)
		maxScroll = max(0, len(allLines)-height)
	}

	scroll = min(max(0, scroll), maxScroll)
	thumbRow := 0
	if maxScroll > 0 && height > 1 {
		thumbRow = scroll * (height - 1) / maxScroll
	}

	lines := make([]string, height)
	for i := range lines {
		text := ""
		if i+scroll < len(allLines) {
			text = allLines[i+scroll]
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
		if marker != "" {
			if visibleWidth(text) < lineWidth {
				text = text + strings.Repeat(" ", lineWidth-visibleWidth(text))
			} else {
				text = fitANSI(text, lineWidth)
			}
			lines[i] = text + marker
		} else {
			lines[i] = text
		}
	}
	return lines
}

type nodeComponent struct {
	original string
	prefix   string
	num      int
	width    int
	hasNum   bool
	suffix   string
}

func parseNodeComponent(name string) nodeComponent {
	lastStart, lastEnd := -1, -1
	inDigits := false
	start := -1

	for i, r := range name {
		if unicode.IsDigit(r) {
			if !inDigits {
				inDigits = true
				start = i
			}
		} else {
			if inDigits {
				inDigits = false
				lastStart, lastEnd = start, i
			}
		}
	}
	if inDigits {
		lastStart, lastEnd = start, len(name)
	}

	if lastStart < 0 {
		return nodeComponent{
			original: name,
			prefix:   name,
		}
	}

	numStr := name[lastStart:lastEnd]
	val, err := strconv.Atoi(numStr)
	if err != nil {
		return nodeComponent{
			original: name,
			prefix:   name,
		}
	}

	return nodeComponent{
		original: name,
		prefix:   name[:lastStart],
		num:      val,
		width:    len(numStr),
		hasNum:   true,
		suffix:   name[lastEnd:],
	}
}

func expandNodeList(raw string) []string {
	var result []string
	for len(raw) > 0 {
		part, rest, found := strings.Cut(raw, ",")
		if open := strings.IndexByte(part, '['); open >= 0 {
			if close := strings.IndexByte(raw[open:], ']'); close >= 0 {
				end := open + close
				part = raw[:end+1]
				if end+1 < len(raw) && raw[end+1] == ',' {
					rest = raw[end+2:]
				} else {
					rest = raw[end+1:]
				}
				found = true
			}
		}
		part = strings.TrimSpace(part)
		if open := strings.IndexByte(part, '['); open >= 0 && strings.HasSuffix(part, "]") {
			prefix := part[:open]
			inside := part[open+1 : len(part)-1]
			for _, span := range strings.Split(inside, ",") {
				first, last, ranged := strings.Cut(span, "-")
				first = strings.TrimSpace(first)
				last = strings.TrimSpace(last)
				if !ranged {
					result = append(result, prefix+first)
				} else {
					start, e1 := strconv.Atoi(first)
					end, e2 := strconv.Atoi(last)
					if e1 == nil && e2 == nil && end >= start {
						padding := max(len(first), len(last))
						for n := start; n <= end; n++ {
							result = append(result, prefix+fmt.Sprintf("%0*d", padding, n))
						}
					} else {
						result = append(result, part)
					}
				}
			}
		} else if part != "" {
			result = append(result, part)
		}
		if !found {
			break
		}
		raw = rest
	}
	return result
}

func condenseNodes(names []string) string {
	if len(names) == 0 {
		return "none"
	}

	seen := make(map[string]bool)
	var expanded []string
	for _, raw := range names {
		for _, node := range expandNodeList(raw) {
			if !seen[node] && node != "" {
				seen[node] = true
				expanded = append(expanded, node)
			}
		}
	}

	if len(expanded) == 0 {
		return "none"
	}

	items := make([]nodeComponent, len(expanded))
	for i, name := range expanded {
		items[i] = parseNodeComponent(name)
	}

	slices.SortFunc(items, func(a, b nodeComponent) int {
		if a.prefix != b.prefix {
			return strings.Compare(a.prefix, b.prefix)
		}
		if a.suffix != b.suffix {
			return strings.Compare(a.suffix, b.suffix)
		}
		if a.hasNum != b.hasNum {
			if a.hasNum {
				return 1
			}
			return -1
		}
		if a.hasNum {
			if a.num != b.num {
				return a.num - b.num
			}
			if a.width != b.width {
				return a.width - b.width
			}
		}
		return strings.Compare(a.original, b.original)
	})

	var blocks []string
	i := 0
	for i < len(items) {
		curr := items[i]
		if !curr.hasNum {
			blocks = append(blocks, curr.original)
			i++
			continue
		}

		startNum := curr.num
		endNum := curr.num
		j := i + 1
		for j < len(items) {
			next := items[j]
			if next.hasNum && next.prefix == curr.prefix && next.suffix == curr.suffix && next.width == curr.width && next.num == endNum+1 {
				endNum = next.num
				j++
			} else {
				break
			}
		}

		if j-i == 1 {
			blocks = append(blocks, curr.original)
		} else {
			condensed := fmt.Sprintf("%s[%0*d-%0*d]%s", curr.prefix, curr.width, startNum, curr.width, endNum, curr.suffix)
			blocks = append(blocks, condensed)
		}
		i = j
	}

	return strings.Join(blocks, ", ")
}

func formatLimitLine(label string, parts []string) string {
	if len(parts) == 0 {
		return label + "none"
	}
	return label + strings.Join(parts, "  •  ")
}

func simplifyTRES(raw string) string {
	if raw == "" {
		return ""
	}
	var typedGPUs []string
	entries := strings.Split(raw, ",")
	for _, entry := range entries {
		k, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(k, "gres/gpu:") {
			typedGPUs = append(typedGPUs, k)
		}
	}

	var parts []string
	for _, entry := range entries {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if (k == "gres/gpu" || k == "gpu") && len(typedGPUs) > 0 {
			continue
		}
		k = strings.TrimPrefix(k, "gres/")
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ", ")
}

func parseQoSLimits(data []byte) map[string]QoSLimit {
	result := make(map[string]QoSLimit)
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return result
	}

	headerLine := strings.TrimSpace(lines[0])
	if headerLine == "" {
		return result
	}

	headers := strings.Split(headerLine, "|")
	colMap := make(map[string]int)
	for i, h := range headers {
		colMap[strings.ToLower(strings.TrimSpace(h))] = i
	}

	get := func(cols []string, name string) string {
		idx, ok := colMap[name]
		if !ok || idx >= len(cols) {
			return ""
		}
		return strings.TrimSpace(cols[idx])
	}

	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cols := strings.Split(line, "|")
		name := get(cols, "name")
		if name == "" {
			continue
		}

		result[name] = QoSLimit{
			Name:           name,
			GrpTRES:        get(cols, "grptres"),
			GrpJobs:        get(cols, "grpjobs"),
			GrpSubmit:      get(cols, "grpsubmit"),
			GrpWall:        get(cols, "grpwall"),
			MaxTRESPA:      get(cols, "maxtrespa"),
			MaxJobsPA:      get(cols, "maxjobspa"),
			MaxSubmitPA:    get(cols, "maxsubmitpa"),
			MaxTRESPU:      get(cols, "maxtrespu"),
			MaxJobsPU:      get(cols, "maxjobspu"),
			MaxSubmitPU:    get(cols, "maxsubmitpu"),
			MaxTRES:        get(cols, "maxtres"),
			MaxTRESPerNode: get(cols, "maxtrespernode"),
			MinTRES:        get(cols, "mintres"),
			MaxWall:        get(cols, "maxwall"),
		}
	}

	return result
}
