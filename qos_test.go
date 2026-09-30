package main

import (
	"strings"
	"testing"
	"time"
)

func TestCollectQoS(t *testing.T) {
	// Empty snapshot
	empty := Snapshot{}
	if groups := collectQoS(empty); len(groups) != 0 {
		t.Fatalf("expected 0 groups, got %d", len(groups))
	}

	snap := Snapshot{
		UpdatedAt: time.Unix(1000, 0),
		nodes: []Node{
			{Name: "g001"},
			{Name: "g002"},
		},
		Jobs: []Job{
			{
				ID:             1,
				User:           "alice",
				Account:        "lab-a",
				QoS:            "urgent",
				Partition:      "gpu",
				State:          "RUNNING",
				Progress:       50,
				Elapsed:        "50% [0-01:00|0-02:00]",
				ElapsedMinutes: 60,
				CPUs:           8,
				GPUs:           2,
				MemoryMB:       16384,
				nodes:          "g001",
			},
			{
				ID:             2,
				User:           "bob",
				Account:        "lab-a",
				QoS:            "urgent",
				Partition:      "gpu",
				State:          "RUNNING",
				Progress:       25,
				Elapsed:        "25% [0-00:30|0-02:00]",
				ElapsedMinutes: 30,
				CPUs:           4,
				GPUs:           1,
				MemoryMB:       8192,
				nodes:          "g002",
			},
			{
				ID:             3,
				User:           "charlie",
				Account:        "lab-b",
				QoS:            "urgent",
				Partition:      "gpu",
				State:          "PENDING",
				ElapsedMinutes: 0,
				CPUs:           16,
				GPUs:           4,
				MemoryMB:       32768,
			},
			{
				ID:             4,
				User:           "dave",
				Account:        "lab-c",
				QoS:            "normal",
				Partition:      "batch",
				State:          "RUNNING",
				Progress:       10,
				ElapsedMinutes: 120,
				CPUs:           2,
				GPUs:           0,
				MemoryMB:       4096,
				nodes:          "g001",
			},
			{
				ID:        5,
				User:      "eve",
				Account:   "lab-d",
				QoS:       "", // empty qos -> (default)
				Partition: "batch",
				State:     "PENDING",
				CPUs:      1,
				GPUs:      0,
				MemoryMB:  2048,
			},
		},
	}

	groups := collectQoS(snap)
	if len(groups) != 3 {
		t.Fatalf("expected 3 QoS groups, got %d", len(groups))
	}

	// Alphabetical order: "(default)", "normal", "urgent"
	if groups[0].name != "(default)" || groups[1].name != "normal" || groups[2].name != "urgent" {
		t.Fatalf("unexpected order: %s, %s, %s", groups[0].name, groups[1].name, groups[2].name)
	}

	// Inspect "urgent"
	urgent := groups[2]
	if urgent.totalJobs != 3 {
		t.Errorf("urgent totalJobs = %d, want 3", urgent.totalJobs)
	}
	if urgent.runningJobs != 2 {
		t.Errorf("urgent runningJobs = %d, want 2", urgent.runningJobs)
	}
	if urgent.pendingJobs != 1 {
		t.Errorf("urgent pendingJobs = %d, want 1", urgent.pendingJobs)
	}
	// Running allocations
	if urgent.allocCPUs != 12 || urgent.allocGPUs != 3 || urgent.allocMemMB != 24576 {
		t.Errorf("urgent alloc = %d CPUs, %d GPUs, %d MB; want 12, 3, 24576", urgent.allocCPUs, urgent.allocGPUs, urgent.allocMemMB)
	}
	// Pending demand must remain separate from running allocations
	if urgent.pendingCPUs != 16 || urgent.pendingGPUs != 4 || urgent.pendingMemMB != 32768 {
		t.Errorf("urgent pending = %d CPUs, %d GPUs, %d MB; want 16, 4, 32768", urgent.pendingCPUs, urgent.pendingGPUs, urgent.pendingMemMB)
	}
	// Runtime stats
	if urgent.minElapsedMin != 30 || urgent.maxElapsedMin != 60 || urgent.avgElapsedMin != 45 {
		t.Errorf("urgent elapsed: min=%d, max=%d, avg=%d; want 30, 60, 45", urgent.minElapsedMin, urgent.maxElapsedMin, urgent.avgElapsedMin)
	}
	if urgent.avgProgress != 37 { // (50 + 25) / 2
		t.Errorf("urgent avgProgress = %d, want 37", urgent.avgProgress)
	}
	// Users, accounts, partitions, nodes
	if len(urgent.users) != 3 {
		t.Errorf("urgent users count = %d, want 3", len(urgent.users))
	}
	if len(urgent.accounts) != 2 {
		t.Errorf("urgent accounts count = %d, want 2", len(urgent.accounts))
	}
	if len(urgent.partitions) != 1 {
		t.Errorf("urgent partitions count = %d, want 1", len(urgent.partitions))
	}
	if len(urgent.nodes) != 2 || urgent.nodes["g001"] != 1 || urgent.nodes["g002"] != 1 {
		t.Errorf("urgent nodes = %v, want g001 and g002", urgent.nodes)
	}
}

func TestRenderQoSBox(t *testing.T) {
	group := qosGroup{
		name:          "urgent",
		totalJobs:     2,
		runningJobs:   1,
		pendingJobs:   1,
		allocCPUs:     8,
		allocGPUs:     2,
		allocMemMB:    16384,
		pendingCPUs:   4,
		pendingGPUs:   1,
		pendingMemMB:  8192,
		minElapsedMin: 45,
		maxElapsedMin: 45,
		avgElapsedMin: 45,
		avgProgress:   50,
		users:         map[string]int{"alice": 1, "bob": 1},
		accounts:      map[string]int{"lab": 2},
		partitions:    map[string]int{"gpu": 2},
		nodes:         map[string]int{"g001": 1},
		hasLimits:     true,
		limits: QoSLimit{
			Name:        "urgent",
			GrpTRES:     "cpu=128,mem=512G,gres/gpu=16",
			GrpJobs:     "10",
			GrpSubmit:   "20",
			GrpWall:     "7-00:00:00",
			MaxTRESPA:   "gres/gpu=8",
			MaxJobsPA:   "5",
			MaxSubmitPA: "10",
			MaxTRESPU:   "cpu=32,gres/gpu=4",
			MaxJobsPU:   "2",
			MaxSubmitPU: "5",
			MaxTRES:     "gres/gpu=2",
			MinTRES:     "cpu=1",
			MaxWall:     "1-00:00:00",
		},
		jobs: []Job{
			{
				ID:       42,
				User:     "alice",
				State:    "RUNNING",
				Elapsed:  "50% [0-00:45|0-01:30]",
				Progress: 50,
				GPUs:     2,
				CPUs:     8,
				MemoryMB: 16384,
				nodes:    "g001",
			},
		},
	}

	width := 120
	box := renderQoSBox(group, width, false)
	if len(box) == 0 {
		t.Fatal("rendered empty box")
	}

	for i, line := range box {
		clean := sgrPattern.ReplaceAllString(line, "")
		if visibleWidth(clean) != width {
			t.Errorf("line %d visible width = %d, want %d: %q", i, visibleWidth(clean), width, clean)
		}
	}

	// Verify top and bottom borders
	if !strings.HasPrefix(box[0], "╭─ urgent") || !strings.HasSuffix(box[0], "╮") {
		t.Errorf("invalid box header: %q", box[0])
	}
	if !strings.HasPrefix(box[len(box)-1], "╰") || !strings.HasSuffix(box[len(box)-1], "╯") {
		t.Errorf("invalid box footer: %q", box[len(box)-1])
	}

	text := strings.Join(box, "\n")
	if !strings.Contains(text, "1 running  •  1 pending") {
		t.Errorf("missing state info: %s", text)
	}
	if !strings.Contains(text, "2 GPUs  •  8 CPUs") {
		t.Errorf("missing allocation info: %s", text)
	}
	if !strings.Contains(text, "Elapsed:") || !strings.Contains(text, "Min: 0-00:45") {
		t.Errorf("missing elapsed info: %s", text)
	}
	if !strings.Contains(text, "QoS Limits:  TRES: cpu=128, mem=512G, gpu=16  •  Max Jobs: 10  •  Max Submit: 20  •  Max Wall: 7-00:00:00") {
		t.Errorf("missing or malformed QoS Limits line: %s", text)
	}
	if !strings.Contains(text, "Acct Limits: Max TRES: gpu=8  •  Max Jobs: 5  •  Max Submit: 10") {
		t.Errorf("missing or malformed Acct Limits line: %s", text)
	}
	if !strings.Contains(text, "User Limits: Max TRES: cpu=32, gpu=4  •  Max Jobs: 2  •  Max Submit: 5") {
		t.Errorf("missing or malformed User Limits line: %s", text)
	}
	if !strings.Contains(text, "Job Limits:  Max TRES: gpu=2  •  Min TRES: cpu=1  •  Max Wall: 1-00:00:00") {
		t.Errorf("missing or malformed Job Limits line: %s", text)
	}
	if !strings.Contains(text, "Node:        g001") {
		t.Errorf("missing or malformed Node line: %s", text)
	}

	// Verify empty limits render as "none"
	emptyLimitsGroup := qosGroup{
		name:      "nolimits",
		hasLimits: true,
	}
	emptyBoxText := strings.Join(renderQoSBox(emptyLimitsGroup, width, false), "\n")
	if !strings.Contains(emptyBoxText, "QoS Limits:  none") {
		t.Errorf("expected 'QoS Limits:  none', got: %s", emptyBoxText)
	}
	if !strings.Contains(emptyBoxText, "Acct Limits: none") {
		t.Errorf("expected 'Acct Limits: none', got: %s", emptyBoxText)
	}
	if !strings.Contains(emptyBoxText, "User Limits: none") {
		t.Errorf("expected 'User Limits: none', got: %s", emptyBoxText)
	}
	if !strings.Contains(emptyBoxText, "Job Limits:  none") {
		t.Errorf("expected 'Job Limits:  none', got: %s", emptyBoxText)
	}
}

func TestSimplifyTRES(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: "",
			want:  "",
		},
		{
			input: "cpu=128,mem=512G,gres/gpu=8",
			want:  "cpu=128, mem=512G, gpu=8",
		},
		{
			// When typed GPU is present, generic gres/gpu should be stripped
			input: "cpu=64,gres/gpu=4,gres/gpu:h200=4",
			want:  "cpu=64, gpu:h200=4",
		},
		{
			// Only typed GPU
			input: "gres/gpu:a100=2",
			want:  "gpu:a100=2",
		},
		{
			// Generic only
			input: "gres/gpu=1",
			want:  "gpu=1",
		},
	}

	for _, tc := range tests {
		got := simplifyTRES(tc.input)
		if got != tc.want {
			t.Errorf("simplifyTRES(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseQoSLimits(t *testing.T) {
	fixture := []byte(`Name|Priority|GraceTime|Preempt|PreemptExemptTime|PreemptMode|Flags|UsageThres|UsageFactor|GrpTRES|GrpTRESMins|GrpTRESRunMins|GrpJobs|GrpSubmit|GrpWall|MaxTRES|MaxTRESPerNode|MaxTRESMins|MaxWall|MaxTRESPU|MaxJobsPU|MaxSubmitPU|MaxTRESPA|MaxTRESRunMinsPA|MaxTRESRunMinsPU|MaxJobsPA|MaxSubmitPA|MinTRES|
normal|0|00:00:00|cluster||cluster|||1.000000|||||||cpu=128,mem=1000G,node=1|||14-00:00:00||||||||||
debug|100|00:00:00|cluster||cluster|||1.000000|cpu=32,mem=128G|||4|10|02:00:00|cpu=8,mem=32G|||01:00:00|cpu=16,mem=64G|2|4|cpu=16,mem=64G|||2|4|cpu=1|
`)

	limits := parseQoSLimits(fixture)
	if len(limits) != 2 {
		t.Fatalf("expected 2 QoS limits, got %d", len(limits))
	}

	debug, ok := limits["debug"]
	if !ok {
		t.Fatal("missing 'debug' QoS in parsed limits")
	}
	if debug.Name != "debug" {
		t.Errorf("debug.Name = %q, want 'debug'", debug.Name)
	}
	if debug.GrpTRES != "cpu=32,mem=128G" {
		t.Errorf("debug.GrpTRES = %q, want 'cpu=32,mem=128G'", debug.GrpTRES)
	}
	if debug.GrpJobs != "4" {
		t.Errorf("debug.GrpJobs = %q, want '4'", debug.GrpJobs)
	}
	if debug.GrpSubmit != "10" {
		t.Errorf("debug.GrpSubmit = %q, want '10'", debug.GrpSubmit)
	}
	if debug.GrpWall != "02:00:00" {
		t.Errorf("debug.GrpWall = %q, want '02:00:00'", debug.GrpWall)
	}
	if debug.MaxTRES != "cpu=8,mem=32G" {
		t.Errorf("debug.MaxTRES = %q, want 'cpu=8,mem=32G'", debug.MaxTRES)
	}
	if debug.MaxWall != "01:00:00" {
		t.Errorf("debug.MaxWall = %q, want '01:00:00'", debug.MaxWall)
	}
	if debug.MaxTRESPU != "cpu=16,mem=64G" {
		t.Errorf("debug.MaxTRESPU = %q, want 'cpu=16,mem=64G'", debug.MaxTRESPU)
	}
	if debug.MaxJobsPU != "2" {
		t.Errorf("debug.MaxJobsPU = %q, want '2'", debug.MaxJobsPU)
	}
	if debug.MaxSubmitPU != "4" {
		t.Errorf("debug.MaxSubmitPU = %q, want '4'", debug.MaxSubmitPU)
	}
	if debug.MaxTRESPA != "cpu=16,mem=64G" {
		t.Errorf("debug.MaxTRESPA = %q, want 'cpu=16,mem=64G'", debug.MaxTRESPA)
	}
	if debug.MaxJobsPA != "2" {
		t.Errorf("debug.MaxJobsPA = %q, want '2'", debug.MaxJobsPA)
	}
	if debug.MaxSubmitPA != "4" {
		t.Errorf("debug.MaxSubmitPA = %q, want '4'", debug.MaxSubmitPA)
	}
	if debug.MinTRES != "cpu=1" {
		t.Errorf("debug.MinTRES = %q, want 'cpu=1'", debug.MinTRES)
	}

	normal, ok := limits["normal"]
	if !ok {
		t.Fatal("missing 'normal' QoS in parsed limits")
	}
	if normal.MaxTRES != "cpu=128,mem=1000G,node=1" {
		t.Errorf("normal.MaxTRES = %q, want 'cpu=128,mem=1000G,node=1'", normal.MaxTRES)
	}
	if normal.MaxWall != "14-00:00:00" {
		t.Errorf("normal.MaxWall = %q, want '14-00:00:00'", normal.MaxWall)
	}
	if normal.GrpTRES != "" {
		t.Errorf("normal.GrpTRES = %q, want empty", normal.GrpTRES)
	}
}

func TestCondenseNodes(t *testing.T) {
	tests := []struct {
		name  string
		nodes []string
		want  string
	}{
		{
			name:  "contiguous pair",
			nodes: []string{"g022", "g023"},
			want:  "g[022-023]",
		},
		{
			name:  "unsorted contiguous pair",
			nodes: []string{"g023", "g022"},
			want:  "g[022-023]",
		},
		{
			name:  "contiguous triple",
			nodes: []string{"g001", "g002", "g003"},
			want:  "g[001-003]",
		},
		{
			name:  "contiguous and isolated",
			nodes: []string{"g001", "g002", "g005"},
			want:  "g[001-002], g005",
		},
		{
			name:  "non-contiguous",
			nodes: []string{"g001", "g003", "g005"},
			want:  "g001, g003, g005",
		},
		{
			name:  "single node",
			nodes: []string{"g022"},
			want:  "g022",
		},
		{
			name:  "multiple prefixes with contiguous ranges",
			nodes: []string{"node1", "node2", "worker1"},
			want:  "node[1-2], worker1",
		},
		{
			name:  "mixed prefixes sorted",
			nodes: []string{"g023", "g022", "node02", "node01"},
			want:  "g[022-023], node[01-02]",
		},
		{
			name:  "nodes with suffix",
			nodes: []string{"gpu01-ib", "gpu02-ib"},
			want:  "gpu[01-02]-ib",
		},
		{
			name:  "hostlist input expanded and recondensed",
			nodes: []string{"g[022-023]", "g024"},
			want:  "g[022-024]",
		},
		{
			name:  "empty",
			nodes: []string{},
			want:  "none",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := condenseNodes(tc.nodes)
			if got != tc.want {
				t.Errorf("condenseNodes(%v) = %q, want %q", tc.nodes, got, tc.want)
			}
		})
	}
}

func TestQoSView(t *testing.T) {
	// Empty snapshot
	emptyLines := qosView(Snapshot{}, 80, 10, 0, false)
	if len(emptyLines) != 10 {
		t.Fatalf("expected 10 lines, got %d", len(emptyLines))
	}
	joined := strings.Join(emptyLines, "\n")
	if !strings.Contains(joined, "No active or pending jobs found") {
		t.Errorf("missing empty notice: %s", joined)
	}

	// Snapshot with jobs and scrolling
	snap := Snapshot{
		Jobs: []Job{
			{
				ID:             1,
				User:           "alice",
				QoS:            "normal",
				State:          "RUNNING",
				ElapsedMinutes: 10,
				CPUs:           4,
				GPUs:           1,
				MemoryMB:       4096,
				nodes:          "g022,g023",
			},
		},
	}
	lines := qosView(snap, 80, 5, 0, false)
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines for height=5, got %d", len(lines))
	}

	// Scrolled lines should have scrollbar marker on the right
	hasScrollMarker := false
	for _, l := range lines {
		if strings.Contains(l, "█") || strings.Contains(l, "░") {
			hasScrollMarker = true
			break
		}
	}
	if !hasScrollMarker {
		t.Error("expected scrollbar marker when content exceeds height")
	}

	// In wide terminal, box width should align with clusterBoxesWidth
	wideLines := qosView(snap, 120, 20, 0, false)
	expectedWidth := clusterBoxesWidth(snap)
	if visibleWidth(wideLines[0]) != expectedWidth {
		t.Errorf("QoS box width in wide terminal = %d, want clusterBoxesWidth = %d", visibleWidth(wideLines[0]), expectedWidth)
	}
}

func TestFormatQoSMem(t *testing.T) {
	if got := formatQoSMem(16384); got != "17G" {
		t.Errorf("formatQoSMem(16384) = %q, want '17G'", got)
	}
	if got := formatQoSMem(1024 * 1024); got != "1.1T" {
		t.Errorf("formatQoSMem(1024*1024) = %q, want '1.1T'", got)
	}
}

func TestQoSRowsAndSorting(t *testing.T) {
	snap := Snapshot{
		Jobs: []Job{
			{
				ID:       1,
				User:     "alice",
				QoS:      "urgent",
				State:    "RUNNING",
				GPUs:     4,
				CPUs:     16,
				MemoryMB: 32768,
			},
			{
				ID:       2,
				User:     "bob",
				QoS:      "normal",
				State:    "RUNNING",
				GPUs:     1,
				CPUs:     4,
				MemoryMB: 8192,
			},
			{
				ID:       3,
				User:     "charlie",
				QoS:      "batch",
				State:    "PENDING",
				GPUs:     8,
				CPUs:     32,
				MemoryMB: 65536,
			},
		},
	}

	headers, rows, ids, cols := qosRows(snap, "gpu", false)
	if len(headers) != 1 || len(rows) != 3 || len(ids) != 3 || len(cols) != 13 {
		t.Fatalf("unexpected dimensions: headers=%d, rows=%d, ids=%d, cols=%d", len(headers), len(rows), len(ids), len(cols))
	}

	// First column must be "qos"
	if cols[0].field != "qos" {
		t.Fatalf("first column field = %q, want 'qos'", cols[0].field)
	}

	// Sorted by GPU desc: urgent (4), normal (1), batch (0)
	if ids[0] != "urgent" || ids[1] != "normal" || ids[2] != "batch" {
		t.Fatalf("gpu desc sort order = %v, want [urgent normal batch]", ids)
	}

	// Sorted by QoS default (A-Z)
	_, _, idsQoS, _ := qosRows(snap, "qos", false)
	if idsQoS[0] != "batch" || idsQoS[1] != "normal" || idsQoS[2] != "urgent" {
		t.Fatalf("qos default sort order = %v, want [batch normal urgent]", idsQoS)
	}

	// Sorted by QoS reversed (Z-A)
	_, _, idsQoSRev, _ := qosRows(snap, "qos", true)
	if idsQoSRev[0] != "urgent" || idsQoSRev[1] != "normal" || idsQoSRev[2] != "batch" {
		t.Fatalf("qos reversed sort order = %v, want [urgent normal batch]", idsQoSRev)
	}

	// Sorted by pending-gpu desc: batch (8), urgent (0), normal (0)
	_, _, idsPendingGPU, _ := qosRows(snap, "pending-gpu", false)
	if idsPendingGPU[0] != "batch" {
		t.Fatalf("pending-gpu desc first = %s, want batch", idsPendingGPU[0])
	}

	// Check MEM column format has 'G' suffix
	if !strings.Contains(rows[0], "34G") {
		t.Errorf("expected urgent row to have 34G mem: %q", rows[0])
	}
}

func TestQoSDetailView(t *testing.T) {
	snap := Snapshot{
		Jobs: []Job{
			{
				ID:             10,
				User:           "alice",
				QoS:            "high",
				State:          "RUNNING",
				GPUs:           2,
				CPUs:           8,
				MemoryMB:       16384,
				ElapsedMinutes: 45,
				Progress:       50,
			},
		},
	}

	// Existing QoS
	lines := qosDetailView(snap, "high", 80, 15, 0, false)
	if len(lines) != 15 {
		t.Fatalf("expected 15 lines, got %d", len(lines))
	}
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "high") || !strings.Contains(text, "Jobs:") || !strings.Contains(text, "Allocated:") {
		t.Errorf("qosDetailView missing expected content: %s", text)
	}

	// Non-existing QoS
	missing := qosDetailView(snap, "nonexistent", 80, 10, 0, false)
	if len(missing) != 10 {
		t.Fatalf("expected 10 lines for missing QoS, got %d", len(missing))
	}
	missingText := strings.Join(missing, "\n")
	if !strings.Contains(missingText, "QoS not found") {
		t.Errorf("expected 'QoS not found', got: %s", missingText)
	}

	// Scrolling with height smaller than content
	scrollLines := qosDetailView(snap, "high", 80, 4, 0, false)
	hasScrollMarker := false
	for _, l := range scrollLines {
		if strings.Contains(l, "█") || strings.Contains(l, "░") {
			hasScrollMarker = true
			break
		}
	}
	if !hasScrollMarker {
		t.Error("expected scrollbar marker when content exceeds height")
	}
}
