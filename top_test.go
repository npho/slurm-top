package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestQueueAggregation(t *testing.T) {
	input := `{"jobs":[
 {"job_id":42,"account":"research","qos":"normal","partition":"gpu","name":"training","user_name":"alice","job_state":["RUNNING"],"start_time":{"set":true,"number":0},"end_time":{"set":true,"number":100},"time_limit":{"set":true,"number":100},"tres_alloc_str":"cpu=8,mem=16G,gres/gpu=2,gres/gpu:a100=2","tres_req_str":"cpu=99,mem=99G,gres/gpu=99"},
 {"user_name":"alice","job_state":["PENDING"],"tres_req_str":"cpu=4,mem=2048M,gres/gpu:a100=1"},
 {"user_name":"bob","job_state":["RUNNING"],"tres_alloc_str":"cpu=2,mem=512,gres/gpu=1"},
 {"user_name":"bob","job_state":["COMPLETED"],"tres_alloc_str":"cpu=100,mem=100G"}]}`
	nodes := []Node{{CPUTotal: 16, GPUTotal: 4, MemoryTotalMB: 32768}}
	s, e := parseQueue([]byte(input), nodes, time.Unix(0, 0))
	if e != nil {
		t.Fatal(e)
	}
	if s.RunningJobs != 2 || s.PendingJobs != 1 || s.ActiveAccounts != 1 || s.CapacityGPU != 4 || len(s.Users) != 2 || len(s.nodes) != 1 {
		t.Fatalf("snapshot: %+v", s)
	}
	sortUsers(s.Users, "gpu")
	a := s.Users[0]
	if a.User != "alice" || a.CPUs != 8 || a.GPUs != 2 || a.MemoryMB != 16384 || a.PendingCPUs != 4 || a.PendingGPUs != 1 || a.PendingMemoryMB != 2048 {
		t.Fatalf("alice: %+v", a)
	}
	if s.Users[1].MemoryMB != 512 {
		t.Fatalf("plain MiB: %+v", s.Users[1])
	}
	if title := topLines(s, "gpu", 0)[0]; !strings.Contains(title, "2 users / 1 accounts / 2 running / 1 pending") {
		t.Fatalf("title: %q", title)
	}
	if len(s.Jobs) != 3 || s.Jobs[0].ID != 42 || s.Jobs[0].Account != "research" || s.Jobs[0].QoS != "normal" || s.Jobs[0].Partition != "gpu" || s.Jobs[0].Progress != 0 || s.Jobs[0].Elapsed != "0% [0-00:00|0-01:40]" {
		t.Fatalf("jobs: %+v", s.Jobs)
	}
	sortUsers(s.Users, "user")
	if s.Users[0].User != "alice" {
		t.Fatal("sort")
	}
	var out bytes.Buffer
	if e := renderTop(&out, s, "json", 0); e != nil {
		t.Fatal(e)
	}
	var parsed Snapshot
	if e := json.Unmarshal(out.Bytes(), &parsed); e != nil || strings.Contains(out.String(), "memory_used_approx") {
		t.Fatalf("unexpected JSON: %s %v", out.String(), e)
	}
}
func TestTypesAndJobSorting(t *testing.T) {
	s, e := parseQueue([]byte(`{"jobs":[{"job_id":2,"user_name":"a","account":"z","job_state":["RUNNING"],"tres_alloc_str":"cpu=2,gres/gpu:h200=2"},{"job_id":1,"user_name":"a","account":"b","job_state":["RUNNING"],"tres_alloc_str":"cpu=1,gres/gpu:h200_1g.18gb=1"}]}`), []Node{{GPUType: "h200", GPUTotal: 8}, {GPUType: "h200_1g.18gb", GPUTotal: 56}}, time.Now())
	if e != nil || s.H200Capacity != 8 || s.MIGCapacity != 56 || s.H200Allocated != 2 || s.MIGAllocated != 1 {
		t.Fatalf("typed: %+v %v", s, e)
	}
	jobs := append([]Job(nil), s.Jobs...)
	sortJobs(jobs, "account", false)
	if jobs[0].Account != "z" {
		t.Fatal(jobs)
	}
	sortJobs(jobs, "account", true)
	if jobs[0].Account != "b" {
		t.Fatal(jobs)
	}
	_, lines, ids, _ := topRows(s, "a", "id", false)
	if len(lines) != 2 || ids[0] != "2/RUNNING" || !strings.Contains(lines[0], "z") {
		t.Fatalf("rows %v %v", lines, ids)
	}
	if headerSort(5, "") != "user" || headerSort(26, "") != "gpu" || headerSort(33, "") != "cpu" || headerSort(20, "a") != "account" {
		t.Fatal("header columns")
	}
	userColumns, jobColumns := headerColumns(""), headerColumns("a")
	if len(userColumns) != 13 || len(jobColumns) != 11 || userColumns[7].field != "pending-jobs" || jobColumns[10].field != "name" {
		t.Fatalf("columns: %v %v", userColumns, jobColumns)
	}
	if got := highlightHeader(userHeader("gpu", false), userColumns[1]); !strings.Contains(got, "\x1b[7mRUN") {
		t.Fatalf("highlight: %q", got)
	}
	// The arrow in an earlier field is one terminal cell but three UTF-8 bytes.
	// It must not move the highlighted range for a later column.
	if got := highlightHeader(userHeader("user", false), userColumns[1]); !strings.Contains(got, "\x1b[7mRUN   \x1b[27m") {
		t.Fatalf("highlight after arrow: %q", got)
	}
}
func TestUserJobStats(t *testing.T) {
	s := Snapshot{Jobs: []Job{{User: "alice", Account: "a", State: "RUNNING"}, {User: "alice", Account: "b", State: "PENDING"}, {User: "alice", Account: "a", State: "PENDING"}, {User: "bob", Account: "c", State: "RUNNING"}}}
	accounts, running, pending := userJobStats(s, "alice")
	if accounts != 2 || running != 1 || pending != 2 {
		t.Fatalf("stats = %d accounts / %d running / %d pending", accounts, running, pending)
	}
}

func TestJobProgressAndBar(t *testing.T) {
	start, end := slurmTime{Set: true, Number: 100}, slurmTime{Set: true, Number: 200}
	if got := jobProgress([]string{"RUNNING"}, start, end, time.Unix(150, 0)); got != 50 {
		t.Fatalf("progress = %d, want 50", got)
	}
	if got := jobProgress([]string{"PENDING"}, start, end, time.Unix(150, 0)); got != 0 {
		t.Fatalf("pending progress = %d, want 0", got)
	}
	elapsed, minutes := elapsedStatus([]string{"RUNNING"}, start, slurmTime{Set: true, Number: 2}, time.Unix(160, 0))
	if elapsed != "50% [0-00:01|0-00:02]" || minutes != 1 {
		t.Fatalf("elapsed = %q, %d", elapsed, minutes)
	}
	if got := displayQoS("urgent", true); got != "\x1b[1;31murgent\x1b[22;39m" {
		t.Fatalf("urgent QoS = %q", got)
	}
	if got := displayQoS("normal", true); got != "normal" {
		t.Fatalf("normal QoS = %q", got)
	}
	if got := progressBar(50, 10, true); got != "\x1b[32m█████\x1b[39m░░░░░" {
		t.Fatalf("bar = %q", got)
	}
	selected := "\x1b[7m" + fitANSI(progressBar(50, 10, true)+"  remainder", 30) + "\x1b[0m"
	if !strings.Contains(selected, "remainder") || strings.Contains(selected, "\x1b[0m░") {
		t.Fatalf("selected progress row = %q", selected)
	}
	_, lines, _ := renderTable([]tableColumn{{"progress", "PROGRESS"}, {"name", "NAME"}}, [][]string{{progressBar(50, 10, true), "remainder"}}, "", false)
	if strings.Contains(lines[0], "\x1b[0m") || !strings.Contains(lines[0], "remainder") {
		t.Fatalf("table row resets selection: %q", lines[0])
	}
}

func TestResourceColumnsAreRightJustified(t *testing.T) {
	for _, field := range []string{"jobs", "pending-jobs", "gpu", "cpu", "cpu-gpu", "mem", "memory-cpu", "pending-gpu", "pending-cpu", "pending-cpu-gpu", "pending-mem", "pending-memory-cpu"} {
		if !rightAlignedColumn(field) {
			t.Errorf("%s is not right-justified", field)
		}
	}
	header, rows, _ := renderTable([]tableColumn{{"memory-cpu", "M:C"}}, [][]string{{"1.0"}, {"10.0"}}, "", false)
	if !strings.HasPrefix(header, "\x1b[1m") || !strings.HasSuffix(header, "\x1b[22m") {
		t.Fatalf("header is not bold: %q", header)
	}
	if got := sgrPattern.ReplaceAllString(header, ""); got != "M:C  " {
		t.Fatalf("header = %q, want left-justified M:C", header)
	}
	if rows[0] != "  1.0" || rows[1] != " 10.0" {
		t.Fatalf("rows = %q, %q; want right-justified values", rows[0], rows[1])
	}
}

func TestResourceRatios(t *testing.T) {
	if got := cpuGPU(8, 2); got != "4.0" {
		t.Fatalf("CPU/GPU = %s", got)
	}
	if got := memoryCPU(16384, 8); got != "2.1" {
		t.Fatalf("memory/CPU = %s", got)
	}
	if got := cpuGPU(8, 0); got != "-" {
		t.Fatalf("CPU/GPU with zero GPUs = %s", got)
	}
}

func TestSortPendingUsersAndHeaderArrows(t *testing.T) {
	users := []Usage{{User: "alice", PendingGPUs: 1}, {User: "bob", PendingGPUs: 3}}
	sortUsers(users, "pending-gpu")
	if users[0].User != "bob" {
		t.Fatalf("pending GPU sort: %v", users)
	}
	header := userHeader("pending-gpu", false)
	if !strings.Contains(header, "GPU ↓") {
		t.Fatalf("missing sort arrow: %q", header)
	}
}

func TestMouseAndArrows(t *testing.T) {
	in, out, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	defer in.Close()
	events := make(chan uiEvent, 10)
	go readTopEvents(in, events)
	_, e = out.Write([]byte("\x1b[B\x1b[Z\x1b[<0;40;5M\x1b[<64;40;5Mq"))
	out.Close()
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"down", "shift-tab", "click", "wheel", "q"} {
		ev := <-events
		if want == "wheel" {
			if ev.wheel != -1 {
				t.Fatal(ev)
			}
			continue
		}
		if ev.key != want {
			t.Fatalf("got %+v want %s", ev, want)
		}
	}
}
func TestWheelBurstIsOneStep(t *testing.T) {
	in, out, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	defer in.Close()
	events := make(chan uiEvent, 10)
	go readTopEvents(in, events)
	// One notch as several terminals report it: four reports at once.
	_, e = out.Write([]byte(strings.Repeat("\x1b[<65;40;5M", 4) + "q"))
	out.Close()
	if e != nil {
		t.Fatal(e)
	}
	if ev := <-events; ev.wheel != 1 {
		t.Fatalf("first event = %+v, want wheel down", ev)
	}
	if ev := <-events; ev.key != "q" {
		t.Fatalf("second event = %+v, want q (burst not coalesced)", ev)
	}
}

func TestWheelThrottle(t *testing.T) {
	type wheelStep struct {
		dir int
		at  time.Duration
	}
	start := time.Unix(1000, 0)
	tests := []struct {
		name  string
		steps []wheelStep
		want  []bool
	}{
		{"burst within window", []wheelStep{{1, 0}, {1, time.Millisecond}, {1, 2 * time.Millisecond}}, []bool{true, false, false}},
		{"separate notches", []wheelStep{{1, 0}, {1, wheelBurst}, {1, 3 * wheelBurst}}, []bool{true, true, true}},
		{"direction change", []wheelStep{{1, 0}, {-1, time.Millisecond}, {1, 2 * time.Millisecond}}, []bool{true, true, true}},
		{"continuous stream throttled", []wheelStep{{-1, 0}, {-1, 10 * time.Millisecond}, {-1, 20 * time.Millisecond}, {-1, 30 * time.Millisecond}}, []bool{true, false, false, true}},
	}
	for _, tc := range tests {
		var w wheelThrottle
		for i, step := range tc.steps {
			if got := w.accept(step.dir, start.Add(step.at)); got != tc.want[i] {
				t.Errorf("%s: step %d accept = %t, want %t", tc.name, i, got, tc.want[i])
			}
		}
	}
}

func TestScrollMarker(t *testing.T) {
	if got := scrollMarker(0, 3, 0, 0); got != "" {
		t.Fatalf("marker without overflow = %q", got)
	}
	if got := scrollMarker(0, 3, 0, 4); got != "█" {
		t.Fatalf("top marker = %q", got)
	}
	if got := scrollMarker(2, 3, 4, 4); got != "█" {
		t.Fatalf("bottom marker = %q", got)
	}
	if got := scrollMarker(1, 3, 0, 4); got != "░" {
		t.Fatalf("track marker = %q", got)
	}
}

func TestClickedTableRow(t *testing.T) {
	if row, ok := clickedTableRow(7, 5, 24, 3, 10); !ok || row != 4 {
		t.Fatalf("clicked row = %d, %t; want 4, true", row, ok)
	}
	for _, y := range []int{5, 24} {
		if _, ok := clickedTableRow(y, 5, 24, 0, 10); ok {
			t.Fatalf("row at y=%d should not be selectable", y)
		}
	}
	if _, ok := clickedTableRow(20, 5, 24, 0, 2); ok {
		t.Fatal("blank table area should not select a row")
	}
}

func TestEscapeEvent(t *testing.T) {
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer out.Close()
	events := make(chan uiEvent, 1)
	go readTopEvents(in, events)
	if _, err := out.Write([]byte("\x1b")); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.key != "escape" {
			t.Fatalf("got %+v, want escape", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for escape event")
	}
}

func TestBarColors(t *testing.T) {
	tests := []struct {
		n, total int
		rgb      string
	}{{0, 10, "0;200;0"}, {5, 10, "255;200;0"}, {10, 10, "255;0;0"}, {15, 10, "255;0;0"}, {0, 0, "128;128;128"}}
	for _, tc := range tests {
		got := coloredBar(tc.n, tc.total, 8, true)
		if !strings.Contains(got, tc.rgb+"m") {
			t.Errorf("%d/%d: %q", tc.n, tc.total, got)
		}
		if strings.Contains(coloredBar(tc.n, tc.total, 8, false), "\x1b[") {
			t.Fatal("color disabled")
		}
	}
}
func TestUsagePairAlignment(t *testing.T) {
	primary, unavailable := usagePair(usageValueFor(1, 3, "1", "3", true), usageValueFor(20, 100, "20", "100", false))
	primary, unavailable = sgrPattern.ReplaceAllString(primary, ""), sgrPattern.ReplaceAllString(unavailable, "")
	if strings.IndexByte(primary, '%') != strings.IndexByte(unavailable, '%') || strings.IndexByte(primary, '/') != strings.IndexByte(unavailable, '/') {
		t.Fatalf("misaligned usage rows: %q / %q", primary, unavailable)
	}
}

func TestTopPaneCycling(t *testing.T) {
	columns := menuColumns()
	if len(columns) != 4 || columns[0].field != "cluster" || columns[1].field != "account" || columns[2].field != "qos" || columns[3].field != "gpu" {
		t.Fatalf("menu columns = %v", columns)
	}
	if columns[0].end >= columns[1].start || columns[1].end >= columns[2].start || columns[2].end >= columns[3].start {
		t.Fatalf("overlapping menu columns = %v", columns)
	}
	if got := cycleTopPane("cluster", false); got != "account" {
		t.Fatalf("Tab from cluster = %q", got)
	}
	if got := cycleTopPane("account", false); got != "qos" {
		t.Fatalf("Tab from account = %q", got)
	}
	if got := cycleTopPane("qos", false); got != "gpu" {
		t.Fatalf("Tab from qos = %q", got)
	}
	if got := cycleTopPane("gpu", false); got != "cluster" {
		t.Fatalf("Tab from gpu = %q", got)
	}
	if got := cycleTopPane("cluster", true); got != "gpu" {
		t.Fatalf("Shift-Tab from cluster = %q", got)
	}
	if got := cycleTopPane("gpu", true); got != "qos" {
		t.Fatalf("Shift-Tab from gpu = %q", got)
	}
	if got := cycleTopPane("qos", true); got != "account" {
		t.Fatalf("Shift-Tab from qos = %q", got)
	}
	if got := cycleTopPane("account", true); got != "cluster" {
		t.Fatalf("Shift-Tab from account = %q", got)
	}
	if got := cycleTopPane("user", false); got != "account" {
		t.Fatalf("Tab from user = %q", got)
	}
	if got := cycleTopPane("user", true); got != "cluster" {
		t.Fatalf("Shift-Tab from user = %q", got)
	}
	if got := cycleTopPane("account-jobs", false); got != "qos" {
		t.Fatalf("Tab from account-jobs = %q", got)
	}
	if got := cycleTopPane("account-jobs", true); got != "account" {
		t.Fatalf("Shift-Tab from account-jobs = %q", got)
	}
	if got := cycleTopPane("qos-details", false); got != "gpu" {
		t.Fatalf("Tab from qos-details = %q", got)
	}
	if got := cycleTopPane("qos-details", true); got != "qos" {
		t.Fatalf("Shift-Tab from qos-details = %q", got)
	}
}

func TestNodeDetails(t *testing.T) {
	if !jobRunsOnNode("g[001-002],g010", "g002") || jobRunsOnNode("g[001-002]", "g003") {
		t.Fatal("hostlist matching")
	}
	wrapped := wrapPaneText("Reason: this message wraps without truncating", 12)
	if len(wrapped) < 2 || !strings.Contains(strings.Join(wrapped, " "), "without truncating") {
		t.Fatalf("wrapped details text = %q", wrapped)
	}
	free, used := 1024, 32768-1024
	jobs := []Job{{ID: 42, User: "alice", Account: "research", State: "RUNNING", GPUs: 2, CPUs: 8, MemoryMB: 16384, nodes: "g[001-002]"}}
	node := Node{Name: "g001", State: "MIXED", GPUType: "h200-mig", GPUTotal: 12, GPUAllocated: 10, migTotal: 12, migAllocated: 10, gpuAllocationTyped: true, CPUTotal: 64, CPUAllocated: 32, MemoryTotalMB: 32768, MemoryAllocatedMB: 16384, MemoryFreeMB: &free, MemoryUsedApproxMB: &used, bootTime: time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC), Reason: "maintenance"}
	lines := nodeDetails(node, jobs, 80, 20, 0, false)
	contents := strings.Join(lines, "\n")
	for _, want := range []string{"Node: g001    State: MIXED    Booted: 03:04:05 on January 02, 2025", "╭─ ALLOCATED", "╭─ UTILIZED", "GPU", "GPU MEM", "H200-MIG", "CPU", "MEM", "32/64", "10/12", "17/34G", "?/10", "?/32", "33/17G", "maintenance", "╭─ 42 • alice • research", "GPU MEM", "?/2", "?/8", "?/17G"} {
		if !strings.Contains(contents, want) {
			t.Errorf("details missing %q: %q", want, lines)
		}
	}
	if strings.Contains(contents, "Free memory:") {
		t.Fatalf("free-memory row remains: %q", lines)
	}
	utilized := nodeUtilizationBars(node, 80, false)
	if len(utilized) != 5 || strings.TrimSpace(utilized[3]) == "" {
		t.Fatalf("utilization box contains a blank row: %q", utilized)
	}
	groups := [][]string{nodeStatusBars(node, 80, false), utilized, nodeJobUtilizationBars(jobs[0], false)}
	widest := 0
	for _, group := range groups {
		for _, line := range group {
			widest = max(widest, visibleWidth(line))
		}
	}
	for _, group := range groups {
		for _, line := range widenStatBox(group, widest) {
			if visibleWidth(line) != widest {
				t.Fatalf("box width = %d, want %d: %q", visibleWidth(line), widest, line)
			}
		}
	}
	idleUtilized := strings.Join(nodeUtilizationBars(Node{GPUTotal: 4, CPUTotal: 64, MemoryTotalMB: 32768}, 80, false), "\n")
	if strings.Contains(idleUtilized, "?") || strings.Count(idleUtilized, "0%  0/0") != 4 || !strings.Contains(idleUtilized, "░") {
		t.Fatalf("zero allocations should have empty 0%% bars: %q", idleUtilized)
	}
	for _, removed := range []string{"╭─ GPU", "╭─ CPU", "╭─ MEM"} {
		if strings.Contains(contents, removed) {
			t.Fatalf("individual resource box remains: %q", lines)
		}
	}
	if strings.Contains(contents, "\x1b[") {
		t.Fatalf("uncolored details contain ANSI: %q", lines)
	}
	if visibleWidth(lines[0]) != 80 || strings.Contains(lines[0], "╭") {
		t.Fatalf("details should fill the pane, got %q", lines[0])
	}
	colored := nodeDetails(Node{GPUTotal: 4, GPUAllocated: 2}, nil, 80, 5, 0, true)
	if !strings.Contains(strings.Join(colored, ""), "\x1b[38;2;") {
		t.Fatalf("missing colored allocation bar: %q", colored)
	}
	scrolling := nodeDetails(Node{Name: "g001", Reason: strings.Repeat("long detail ", 12)}, nil, 30, 5, 0, false)
	if !strings.Contains(strings.Join(scrolling, ""), "█") || !strings.Contains(strings.Join(scrolling, ""), "░") {
		t.Fatalf("missing details scroll indicator: %q", scrolling)
	}
}

func TestNodeGPUStatusBars(t *testing.T) {
	hybrid := Node{GPUTotal: 68, GPUAllocated: 14, h200Total: 8, migTotal: 60, h200Allocated: 2, migAllocated: 12, gpuAllocationTyped: true}
	lines := strings.Join(nodeStatusBars(hybrid, 200, false), "\n")
	for _, want := range []string{"╭─ ALLOCATED", "H200 ", "H200-MIG ", "2/8", "12/60"} {
		if !strings.Contains(lines, want) {
			t.Errorf("hybrid status bars missing %q: %q", want, lines)
		}
	}
	heading := sgrPattern.ReplaceAllString(nodeStatusBars(hybrid, 200, false)[1], "")
	for _, label := range []string{"GPU", "CPU", "MEM"} {
		if !strings.Contains(heading, label) {
			t.Errorf("missing %s heading in %q", label, heading)
		}
	}
	single := strings.Join(nodeStatusBars(Node{GPUTotal: 8, h200Total: 8, GPUType: "h200"}, 200, false), "\n")
	if !strings.Contains(single, "H200 ") || strings.Contains(single, "H200-MIG") {
		t.Fatalf("single-type status bars = %q", single)
	}
	noGPU := strings.Join(nodeStatusBars(Node{CPUTotal: 64, MemoryTotalMB: 32768}, 200, false), "\n")
	if strings.Contains(noGPU, "GPU ") {
		t.Fatalf("GPU bar shown without GPUs: %q", noGPU)
	}
	down := nodeStatusBars(Node{State: "DOWN", GPUTotal: 8, CPUTotal: 64, MemoryTotalMB: 32768}, 200, false)
	if strings.Contains(strings.Join(down, "\n"), "?") || !strings.Contains(down[2], "░") {
		t.Fatalf("zero allocated unavailable node should have empty bars: %q", down)
	}
	memoryInfo := sgrPattern.ReplaceAllString(down[3], "")
	memoryUnavailable := sgrPattern.ReplaceAllString(down[4], "")
	if strings.Index(memoryInfo, "G") != strings.Index(memoryUnavailable, "G") || strings.LastIndex(memoryInfo[:strings.Index(memoryInfo, "G")], "%") != strings.LastIndex(memoryUnavailable[:strings.Index(memoryUnavailable, "G")], "%") {
		t.Fatalf("memory allocation rows are not aligned: %q / %q", memoryInfo, memoryUnavailable)
	}
}

func TestViewMenu(t *testing.T) {
	menu := viewMenu("USER")
	if len(menu) != 1 {
		t.Fatalf("menu lines = %d, want 1", len(menu))
	}
	if got := sgrPattern.ReplaceAllString(menu[0], ""); got != " USER    ACCOUNT    QOS    NODES " {
		t.Fatalf("menu labels = %q", got)
	}
	if !strings.Contains(menu[0], "\x1b[7m USER \x1b[27m") {
		t.Fatalf("cluster/user is not highlighted: %q", menu)
	}
	if acctMenu := viewMenu(paneMenuLabel("account")); !strings.Contains(acctMenu[0], "\x1b[7m ACCOUNT \x1b[27m") {
		t.Fatalf("account view is not highlighted as ACCOUNT: %q", acctMenu)
	}
	if acctJobsMenu := viewMenu(paneMenuLabel("account-jobs")); !strings.Contains(acctJobsMenu[0], "\x1b[7m ACCOUNT \x1b[27m") {
		t.Fatalf("account jobs view is not highlighted as ACCOUNT: %q", acctJobsMenu)
	}
	if qosMenu := viewMenu(paneMenuLabel("qos")); !strings.Contains(qosMenu[0], "\x1b[7m QOS \x1b[27m") {
		t.Fatalf("qos view is not highlighted as QOS: %q", qosMenu)
	}
	if qosDetailsMenu := viewMenu(paneMenuLabel("qos-details")); !strings.Contains(qosDetailsMenu[0], "\x1b[7m QOS \x1b[27m") {
		t.Fatalf("qos details view is not highlighted as QOS: %q", qosDetailsMenu)
	}
	if userMenu := viewMenu(paneMenuLabel("user")); !strings.Contains(userMenu[0], "\x1b[7m USER \x1b[27m") {
		t.Fatalf("user jobs view is not highlighted as USER: %q", userMenu)
	}
}

func TestAccountAggregationAndRows(t *testing.T) {
	snap := Snapshot{
		Jobs: []Job{
			{
				ID:             1,
				User:           "alice",
				Account:        "ai-lab",
				State:          "RUNNING",
				CPUs:           8,
				GPUs:           2,
				MemoryMB:       16384,
				Elapsed:        "50% [0-01:00|0-02:00]",
				ElapsedMinutes: 60,
			},
			{
				ID:             2,
				User:           "bob",
				Account:        "ai-lab",
				State:          "RUNNING",
				CPUs:           4,
				GPUs:           1,
				MemoryMB:       8192,
				Elapsed:        "25% [0-00:30|0-02:00]",
				ElapsedMinutes: 30,
			},
			{
				ID:       3,
				User:     "charlie",
				Account:  "ai-lab",
				State:    "PENDING",
				CPUs:     16,
				GPUs:     4,
				MemoryMB: 32768,
			},
			{
				ID:             4,
				User:           "dave",
				Account:        "stats-lab",
				State:          "RUNNING",
				CPUs:           2,
				GPUs:           0,
				MemoryMB:       4096,
				Elapsed:        "10% [0-00:10|0-01:40]",
				ElapsedMinutes: 10,
			},
		},
	}

	accounts := collectAccounts(snap)
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
	sortAccounts(accounts, "account")
	if accounts[0].Account != "ai-lab" || accounts[1].Account != "stats-lab" {
		t.Fatalf("unexpected account sort order: %+v", accounts)
	}
	ai := accounts[0]
	if ai.RunningJobs != 2 || ai.PendingJobs != 1 {
		t.Errorf("ai-lab jobs: running=%d, pending=%d", ai.RunningJobs, ai.PendingJobs)
	}
	if ai.CPUs != 12 || ai.GPUs != 3 || ai.MemoryMB != 24576 {
		t.Errorf("ai-lab running alloc: %d CPUs, %d GPUs, %d MB", ai.CPUs, ai.GPUs, ai.MemoryMB)
	}
	if ai.PendingCPUs != 16 || ai.PendingGPUs != 4 || ai.PendingMemoryMB != 32768 {
		t.Errorf("ai-lab pending demand: %d CPUs, %d GPUs, %d MB", ai.PendingCPUs, ai.PendingGPUs, ai.PendingMemoryMB)
	}

	// Test accountRows overview (account == "")
	headers, lines, ids, columns := accountRows(snap, "", "account", false)
	if len(headers) != 1 || len(lines) != 2 || len(ids) != 2 {
		t.Fatalf("accountRows output count mismatch: headers=%d, lines=%d, ids=%d", len(headers), len(lines), len(ids))
	}
	cleanHeader := sgrPattern.ReplaceAllString(headers[0], "")
	if !strings.HasPrefix(cleanHeader, "ACCOUNT") {
		t.Fatalf("first column is not ACCOUNT: %q", cleanHeader)
	}
	if columns[0].field != "account" {
		t.Fatalf("columns[0].field = %q, want 'account'", columns[0].field)
	}
	if !strings.Contains(lines[0], "ai-lab") || !strings.Contains(lines[1], "stats-lab") {
		t.Fatalf("lines missing accounts: %v", lines)
	}

	// Test accountRows drill-down into account jobs (account == "ai-lab")
	jobHeaders, jobLines, jobIDs, jobCols := accountRows(snap, "ai-lab", "id", false)
	if len(jobHeaders) != 1 || len(jobLines) != 3 || len(jobIDs) != 3 {
		t.Fatalf("account job lines mismatch: lines=%d", len(jobLines))
	}
	cleanJobHdr := sgrPattern.ReplaceAllString(jobHeaders[0], "")
	if !strings.Contains(cleanJobHdr, "USER") {
		t.Fatalf("account job header missing USER column: %q", cleanJobHdr)
	}
	if jobCols[1].field != "user" {
		t.Fatalf("jobCols[1].field = %q, want 'user'", jobCols[1].field)
	}
	if !strings.Contains(jobLines[0], "charlie") || !strings.Contains(jobLines[1], "bob") || !strings.Contains(jobLines[2], "alice") {
		t.Fatalf("job lines missing users: %v", jobLines)
	}

	// Test accountJobStats
	uCount, runCount, pendCount := accountJobStats(snap, "ai-lab")
	if uCount != 3 || runCount != 2 || pendCount != 1 {
		t.Errorf("accountJobStats: users=%d, running=%d, pending=%d; want 3, 2, 1", uCount, runCount, pendCount)
	}
}

func TestUsageValuePercentRoundsToWholeNumber(t *testing.T) {
	for _, test := range []struct {
		used, total int
		want        string
	}{
		{1, 3, "33%"},
		{2, 3, "67%"},
		{1, 8, "12%"},
		{7, 8, "88%"},
		{0, 0, "0%"},
	} {
		if got := usageValueFor(test.used, test.total, "", "", false).percent; got != test.want {
			t.Errorf("usageValueFor(%d, %d) = %q, want %q", test.used, test.total, got, test.want)
		}
	}
}

func TestHorizontalBars(t *testing.T) {
	s := Snapshot{CapacityCPU: 64, CapacityMemoryMB: 65536, AllocatableCPU: 64, AllocatableMemoryMB: 65536, AllocatableCPUUsed: 32, AllocatableMemoryUsedMB: 32768, H200Capacity: 8, AllocatableH200: 8, AllocatableH200Used: 4, MIGCapacity: 56, AllocatableMIG: 56, AllocatableMIGUsed: 12, H200Allocated: 4, MIGAllocated: 12, Users: []Usage{{CPUs: 32, MemoryMB: 32768}}}
	wide := horizontalBars(s, 180)
	if len(wide) != 5 || !strings.Contains(wide[0], "CPU") || !strings.Contains(wide[0], "MEM") || !strings.Contains(wide[0], "GPU") || !strings.Contains(wide[1], "H200-MIG") || !strings.HasPrefix(wide[0], "╭─ GPU") {
		t.Fatalf("wide: %v", wide)
	}
	narrow := horizontalBars(s, 40)
	if len(narrow) != 10 {
		t.Fatalf("narrow: %v", narrow)
	}
	colored := horizontalBars(s, 40, true)
	if len(colored) != len(narrow) || !strings.Contains(strings.Join(colored, ""), "\x1b[38;2;") {
		t.Fatalf("colored: %v", colored)
	}
	for i, line := range colored {
		if plain := sgrPattern.ReplaceAllString(line, ""); plain != sgrPattern.ReplaceAllString(narrow[i], "") {
			t.Fatalf("layout differs: %q != %q", plain, narrow[i])
		}
	}
	if visible := utf8.RuneCountInString(sgrPattern.ReplaceAllString(fitANSI(colored[0], 45), "")); visible != 45 {
		t.Fatalf("width: %d", visible)
	}
}
func TestMemoryUnits(t *testing.T) {
	for s, want := range map[string]int{"1G": 1024, "2T": 2 * 1024 * 1024, "2048M": 2048, "512": 512, "-": 0, "0": 0} {
		if got := memoryMB(s); got != want {
			t.Errorf("%q: got %d want %d", s, got, want)
		}
	}
}

func TestMemoryUnitDisplayAndCondensing(t *testing.T) {
	snap := Snapshot{
		CapacityCPU:         32,
		CapacityGPU:         8,
		CapacityMemoryMB:    65536,
		AllocatableCPU:      32,
		AllocatableGPU:      8,
		AllocatableMemoryMB: 65536,
		Users: []Usage{
			{
				User:            "alice",
				RunningJobs:     1,
				CPUs:            8,
				GPUs:            2,
				MemoryMB:        16384,
				PendingJobs:     1,
				PendingCPUs:     4,
				PendingGPUs:     1,
				PendingMemoryMB: 8192,
			},
		},
		Jobs: []Job{
			{
				ID:       101,
				User:     "alice",
				Account:  "ai-lab",
				State:    "RUNNING",
				CPUs:     8,
				GPUs:     2,
				MemoryMB: 16384,
			},
		},
	}

	// 1. topLines
	lines := topLines(snap, "user", 0)
	topOutput := strings.Join(lines, "\n")
	if strings.Contains(topOutput, " TB") {
		t.Errorf("topLines contains ' TB': %s", topOutput)
	}
	if !strings.Contains(topOutput, "MEM allocated") || !strings.Contains(lines[3], "T") {
		t.Errorf("topLines MEM allocated missing 'T': %q", lines[3])
	}
	if !strings.Contains(topOutput, "17G") || !strings.Contains(topOutput, "9G") {
		t.Errorf("topLines user row missing '17G' or '9G': %s", topOutput)
	}

	// 2. topRows overview (cluster)
	_, userLines, _, _ := topRows(snap, "", "user", false)
	if len(userLines) != 1 || !strings.Contains(userLines[0], "17G") || !strings.Contains(userLines[0], "9G") {
		t.Errorf("topRows user overview missing G unit: %v", userLines)
	}

	// 3. topRows user jobs
	_, jobLines, _, _ := topRows(snap, "alice", "id", false)
	if len(jobLines) != 1 || !strings.Contains(jobLines[0], "17G") {
		t.Errorf("topRows user jobs missing G unit: %v", jobLines)
	}

	// 4. accountRows overview
	_, acctLines, _, _ := accountRows(snap, "", "account", false)
	if len(acctLines) != 1 || !strings.Contains(acctLines[0], "17G") {
		t.Errorf("accountRows overview missing G unit: %v", acctLines)
	}

	// 5. accountRows account jobs
	_, acctJobLines, _, _ := accountRows(snap, "ai-lab", "id", false)
	if len(acctJobLines) != 1 || !strings.Contains(acctJobLines[0], "17G") {
		t.Errorf("accountRows account jobs missing G unit: %v", acctJobLines)
	}

	// 6. horizontalBars
	bars := horizontalBars(snap, 120, false)
	barsText := strings.Join(bars, "\n")
	if strings.Contains(barsText, " TB") {
		t.Errorf("horizontalBars contains ' TB': %s", barsText)
	}
	if !strings.Contains(barsText, "T") {
		t.Errorf("horizontalBars missing 'T': %s", barsText)
	}
}
