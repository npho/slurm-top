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
 {"job_id":42,"account":"research","qos":"normal","name":"training","user_name":"alice","job_state":["RUNNING"],"start_time":{"set":true,"number":0},"end_time":{"set":true,"number":100},"tres_alloc_str":"cpu=8,mem=16G,gres/gpu=2,gres/gpu:a100=2","tres_req_str":"cpu=99,mem=99G,gres/gpu=99"},
 {"user_name":"alice","job_state":["PENDING"],"tres_req_str":"cpu=4,mem=2048M,gres/gpu:a100=1"},
 {"user_name":"bob","job_state":["RUNNING"],"tres_alloc_str":"cpu=2,mem=512,gres/gpu=1"},
 {"user_name":"bob","job_state":["COMPLETED"],"tres_alloc_str":"cpu=100,mem=100G"}]}`
	nodes := []Node{{CPUTotal: 16, GPUTotal: 4, MemoryTotalMB: 32768}}
	s, e := parseQueue([]byte(input), nodes, time.Unix(0, 0))
	if e != nil {
		t.Fatal(e)
	}
	if s.RunningJobs != 2 || s.PendingJobs != 1 || s.CapacityGPU != 4 || len(s.Users) != 2 {
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
	if title := topLines(s, "gpu", 0)[0]; !strings.Contains(title, "2 users / 2 running / 1 pending") {
		t.Fatalf("title: %q", title)
	}
	if len(s.Jobs) != 3 || s.Jobs[0].ID != 42 || s.Jobs[0].Account != "research" || s.Jobs[0].QoS != "normal" || s.Jobs[0].Progress != 0 {
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
	if e := json.Unmarshal(out.Bytes(), &parsed); e != nil || strings.Contains(out.String(), "memory_used") {
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
	_, lines, ids := topRows(s, "a", "id", false)
	if len(lines) != 2 || ids[0] != "2/RUNNING" || !strings.Contains(lines[0], "z") {
		t.Fatalf("rows %v %v", lines, ids)
	}
	if headerSort(5, "") != "user" || headerSort(26, "") != "gpu" || headerSort(33, "") != "cpu" || headerSort(20, "a") != "account" {
		t.Fatal("header columns")
	}
	userColumns, jobColumns := headerColumns(""), headerColumns("a")
	if len(userColumns) != 13 || len(jobColumns) != 10 || userColumns[7].field != "pending-jobs" || jobColumns[9].field != "name" {
		t.Fatalf("columns: %v %v", userColumns, jobColumns)
	}
	if got := highlightHeader(userHeader("gpu", false), userColumns[1]); !strings.Contains(got, "\x1b[7mRUN") {
		t.Fatalf("highlight: %q", got)
	}
	// The arrow in an earlier field is one terminal cell but three UTF-8 bytes.
	// It must not move the highlighted range for a later column.
	if got := highlightHeader(userHeader("user", false), userColumns[1]); !strings.Contains(got, "\x1b[7mRUN   \x1b[0m") {
		t.Fatalf("highlight after arrow: %q", got)
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
	if got := progressBar(50, 10, true); got != "\x1b[32m█████\x1b[39m░░░░░" {
		t.Fatalf("bar = %q", got)
	}
	selected := "\x1b[7m" + fitANSI(progressBar(50, 10, true)+"  remainder", 30) + "\x1b[0m"
	if !strings.Contains(selected, "remainder") || strings.Contains(selected, "\x1b[0m░") {
		t.Fatalf("selected progress row = %q", selected)
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
	_, e = out.Write([]byte("\x1b[B\x1b[<0;40;5M\x1b[<64;40;5Mq"))
	out.Close()
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"down", "click", "wheel", "q"} {
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
func TestHorizontalBars(t *testing.T) {
	s := Snapshot{CapacityCPU: 64, CapacityMemoryMB: 65536, H200Capacity: 8, MIGCapacity: 56, H200Allocated: 4, MIGAllocated: 12, Users: []Usage{{CPUs: 32, MemoryMB: 32768}}}
	wide := horizontalBars(s, 180)
	if len(wide) != 1 || !strings.Contains(wide[0], "CPU") || !strings.Contains(wide[0], "MEM") || !strings.Contains(wide[0], "H200-MIG") {
		t.Fatalf("wide: %v", wide)
	}
	narrow := horizontalBars(s, 80)
	if len(narrow) != 2 {
		t.Fatalf("narrow: %v", narrow)
	}
	colored := horizontalBars(s, 80, true)
	if len(colored) != len(narrow) || !strings.Contains(colored[0], "\x1b[38;2;") {
		t.Fatalf("colored: %v", colored)
	}
	for i, line := range colored {
		if plain := sgrPattern.ReplaceAllString(line, ""); plain != narrow[i] {
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
