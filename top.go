package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type slurmTime struct {
	Set      bool  `json:"set"`
	Infinite bool  `json:"infinite"`
	Number   int64 `json:"number"`
}

type jobRecord struct {
	ID        int       `json:"job_id"`
	Account   string    `json:"account"`
	QoS       string    `json:"qos"`
	Name      string    `json:"name"`
	User      string    `json:"user_name"`
	State     []string  `json:"job_state"`
	StartTime slurmTime `json:"start_time"`
	EndTime   slurmTime `json:"end_time"`
	Alloc     string    `json:"tres_alloc_str"`
	Requested string    `json:"tres_req_str"`
}
type queueResponse struct {
	Jobs []jobRecord `json:"jobs"`
}
type Job struct {
	ID       int    `json:"id"`
	User     string `json:"user"`
	Account  string `json:"account"`
	QoS      string `json:"qos"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Progress int    `json:"progress_percent"`
	CPUs     int    `json:"cpus"`
	GPUs     int    `json:"gpus"`
	MemoryMB int    `json:"memory_mb"`
}
type Usage struct {
	User            string `json:"user"`
	RunningJobs     int    `json:"running_jobs"`
	PendingJobs     int    `json:"pending_jobs"`
	CPUs            int    `json:"cpus_allocated"`
	GPUs            int    `json:"gpus_allocated"`
	MemoryMB        int    `json:"memory_allocated_mb"`
	PendingCPUs     int    `json:"cpus_pending_requested"`
	PendingGPUs     int    `json:"gpus_pending_requested"`
	PendingMemoryMB int    `json:"memory_pending_requested_mb"`
	// There is deliberately no observed utilization here: Slurm allocations
	// are not hardware usage and sstat denies access to other users' steps.
}
type Snapshot struct {
	UpdatedAt         time.Time `json:"updated_at"`
	Users             []Usage   `json:"users"`
	RunningJobs       int       `json:"running_jobs"`
	PendingJobs       int       `json:"pending_jobs"`
	CapacityCPU       int       `json:"capacity_cpu"`
	CapacityGPU       int       `json:"capacity_gpu"`
	CapacityMemoryMB  int       `json:"capacity_memory_mb"`
	H200Capacity      int       `json:"h200_capacity"`
	H200Allocated     int       `json:"h200_allocated"`
	MIGCapacity       int       `json:"mig_capacity"`
	MIGAllocated      int       `json:"mig_allocated"`
	OtherGPUCapacity  int       `json:"other_gpu_capacity"`
	OtherGPUAllocated int       `json:"other_gpu_allocated"`
	Jobs              []Job     `json:"jobs"`
}

func tresValue(tres, key string) string {
	for _, entry := range strings.Split(tres, ",") {
		k, v, ok := strings.Cut(entry, "=")
		if ok && k == key {
			return v
		}
	}
	return ""
}
func memoryMB(s string) int {
	if s == "" {
		return 0
	}
	unit := s[len(s)-1]
	mult := float64(1)
	switch unit {
	case 'K', 'k':
		mult = 1.0 / 1024
	case 'M', 'm':
		mult = 1
	case 'G', 'g':
		mult = 1024
	case 'T', 't':
		mult = 1024 * 1024
	default: // plain numeric Slurm memory value is MiB
	}
	if strings.ContainsRune("KkMmGgTt", rune(unit)) {
		s = s[:len(s)-1]
	}
	v, e := strconv.ParseFloat(s, 64)
	if e != nil || v < 0 {
		return 0
	}
	return int(v*mult + 0.5)
}
func jobProgress(state []string, start, end slurmTime, at time.Time) int {
	if !slices.Contains(state, "RUNNING") || !start.Set || !end.Set || start.Infinite || end.Infinite || end.Number <= start.Number {
		return 0
	}
	progress := int((at.Unix() - start.Number) * 100 / (end.Number - start.Number))
	return min(100, max(0, progress))
}

func parseQueue(data []byte, nodes []Node, at time.Time) (Snapshot, error) {
	var response queueResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return Snapshot{}, fmt.Errorf("decode squeue JSON: %w", err)
	}
	snap := Snapshot{UpdatedAt: at, Users: []Usage{}, Jobs: []Job{}}
	for _, n := range nodes {
		snap.CapacityCPU += n.CPUTotal
		snap.CapacityGPU += n.GPUTotal
		classifyGPU(n.GPUType, n.GPUTotal, &snap.H200Capacity, &snap.MIGCapacity, &snap.OtherGPUCapacity)
		snap.CapacityMemoryMB += n.MemoryTotalMB
	}
	users := map[string]*Usage{}
	for _, j := range response.Jobs {
		if j.User == "" {
			continue
		}
		running, pending := false, false
		for _, s := range j.State {
			if s == "RUNNING" {
				running = true
			}
			if s == "PENDING" {
				pending = true
			}
		}
		if !running && !pending {
			continue
		}
		j.User = printable(j.User)
		j.Account = printable(j.Account)
		j.QoS = printable(j.QoS)
		j.Name = printable(j.Name)
		u := users[j.User]
		if u == nil {
			u = &Usage{User: j.User}
			users[j.User] = u
		}
		if running {
			// AllocTRES is authoritative for running jobs. Never mix pending demand
			// into live allocations or silently substitute requested resources.
			u.RunningJobs++
			snap.RunningJobs++
			u.CPUs += number(tresValue(j.Alloc, "cpu"))
			u.GPUs += gpuFromTRES(j.Alloc)
			u.MemoryMB += memoryMB(tresValue(j.Alloc, "mem"))
			gpuTypeCounts(j.Alloc, &snap.H200Allocated, &snap.MIGAllocated, &snap.OtherGPUAllocated)
			snap.Jobs = append(snap.Jobs, Job{ID: j.ID, User: j.User, Account: j.Account, QoS: j.QoS, Name: j.Name, State: "RUNNING", Progress: jobProgress(j.State, j.StartTime, j.EndTime, at), CPUs: number(tresValue(j.Alloc, "cpu")), GPUs: gpuFromTRES(j.Alloc), MemoryMB: memoryMB(tresValue(j.Alloc, "mem"))})
		} else {
			u.PendingJobs++
			snap.PendingJobs++
			u.PendingCPUs += number(tresValue(j.Requested, "cpu"))
			u.PendingGPUs += gpuFromTRES(j.Requested)
			u.PendingMemoryMB += memoryMB(tresValue(j.Requested, "mem"))
			snap.Jobs = append(snap.Jobs, Job{ID: j.ID, User: j.User, Account: j.Account, QoS: j.QoS, Name: j.Name, State: "PENDING", CPUs: number(tresValue(j.Requested, "cpu")), GPUs: gpuFromTRES(j.Requested), MemoryMB: memoryMB(tresValue(j.Requested, "mem"))})
		}
	}
	for _, u := range users {
		snap.Users = append(snap.Users, *u)
	}
	return snap, nil
}
func gpuFromTRES(s string) int { n, _ := gpuCount(s, ""); return n }
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) || r == 27 {
			return -1
		}
		return r
	}, s)
}
func classifyGPU(typ string, n int, h200, mig, other *int) {
	switch {
	case typ == "h200":
		*h200 += n
	case strings.Contains(strings.ToLower(typ), "h200_") || strings.Contains(strings.ToLower(typ), "mig"):
		*mig += n
	default:
		*other += n
	}
}
func gpuTypeCounts(tres string, h200, mig, other *int) {
	typed := 0
	for _, part := range strings.Split(tres, ",") {
		key, val, ok := strings.Cut(part, "=")
		if !ok || !strings.HasPrefix(key, "gres/gpu:") {
			continue
		}
		n := number(val)
		typed += n
		classifyGPU(strings.TrimPrefix(key, "gres/gpu:"), n, h200, mig, other)
	}
	// Generic-only allocations must not be mislabelled as H200 or MIG.
	if typed == 0 {
		*other += gpuFromTRES(tres)
	}
}
func sortUsers(users []Usage, by string) {
	sort.Slice(users, func(i, j int) bool {
		a, b := users[i], users[j]
		var left, right int
		switch by {
		case "cpu":
			left, right = a.CPUs, b.CPUs
		case "mem":
			left, right = a.MemoryMB, b.MemoryMB
		case "jobs":
			left, right = a.RunningJobs, b.RunningJobs
		case "pending-cpu":
			left, right = a.PendingCPUs, b.PendingCPUs
		case "pending-mem":
			left, right = a.PendingMemoryMB, b.PendingMemoryMB
		case "pending-gpu":
			left, right = a.PendingGPUs, b.PendingGPUs
		case "pending-jobs":
			left, right = a.PendingJobs, b.PendingJobs
		case "user":
			return a.User < b.User
		default:
			left, right = a.GPUs, b.GPUs
		}
		if left != right {
			return left > right
		}
		return a.User < b.User
	})
}

func sortArrow(field, by string, asc bool) string {
	if field != by {
		return ""
	}
	if asc {
		return "↑"
	}
	return "↓"
}

func leftHeader(label, field, by string, asc bool, width int) string {
	if arrow := sortArrow(field, by, asc); arrow != "" {
		label += " " + arrow
	}
	padding := width - utf8.RuneCountInString(label)
	if padding <= 0 {
		return label
	}
	return label + strings.Repeat(" ", padding)
}

func userHeader(by string, asc bool) string {
	return strings.Join([]string{
		leftHeader("USER", "user", by, asc, 16),
		leftHeader("RUN", "jobs", by, asc, 6),
		leftHeader("CPU", "cpu", by, asc, 6),
		leftHeader("MEM", "mem", by, asc, 6),
		leftHeader("GPU", "gpu", by, asc, 6),
		" ",
		leftHeader("PEND", "pending-jobs", by, asc, 6),
		leftHeader("CPU", "pending-cpu", by, asc, 6),
		leftHeader("MEM", "pending-mem", by, asc, 6),
		leftHeader("GPU", "pending-gpu", by, asc, 6),
	}, " ")
}
func collectTop(ctx context.Context) (Snapshot, error) {
	// Sequential calls avoid doubling controller load when many people use watch.
	data, e := collect(ctx)
	if e != nil {
		return Snapshot{}, e
	}
	cmd := exec.CommandContext(ctx, "squeue", "--json")
	raw, e := cmd.Output()
	if e != nil {
		if x, ok := e.(*exec.ExitError); ok {
			return Snapshot{}, fmt.Errorf("squeue: %s: %w", strings.TrimSpace(string(x.Stderr)), e)
		}
		return Snapshot{}, fmt.Errorf("squeue: %w", e)
	}
	return parseQueue(raw, parseNodes(string(data), true), time.Now())
}
func percentBar(n, total, width int) string {
	if total == 0 {
		return strings.Repeat("?", width)
	}
	return bar(n, total, width)
}
func topLines(s Snapshot, by string, width int) []string {
	users := append([]Usage(nil), s.Users...)
	sortUsers(users, by)
	c, g, m := 0, 0, 0
	for _, u := range users {
		c += u.CPUs
		g += u.GPUs
		m += u.MemoryMB
	}
	lines := []string{
		fmt.Sprintf("slurm-top  %s    %d users / %d running / %d pending", s.UpdatedAt.Format("15:04:05"), len(s.Users), s.RunningJobs, s.PendingJobs),
		fmt.Sprintf("GPU allocated  %s %d/%d", percentBar(g, s.CapacityGPU, 20), g, s.CapacityGPU),
		fmt.Sprintf("CPU allocated  %s %d/%d", percentBar(c, s.CapacityCPU, 20), c, s.CapacityCPU),
		fmt.Sprintf("MEM allocated  %s %s/%s TB", percentBar(m, s.CapacityMemoryMB, 20), tb(m), tb(s.CapacityMemoryMB)),
		"Bars = allocations / cluster capacity. Not measured utilization.",
		userHeader(by, false),
	}
	for _, u := range users {
		lines = append(lines, fmt.Sprintf("%-16.16s %6d %6d %6s %6d  %6d %6d %6s %6d", u.User, u.RunningJobs, u.CPUs, gb(u.MemoryMB), u.GPUs, u.PendingJobs, u.PendingCPUs, gb(u.PendingMemoryMB), u.PendingGPUs))
	}
	if len(users) == 0 {
		lines = append(lines, "No running or pending jobs.")
	}
	// Keep a compact readable snapshot when writing to a narrow screen.
	if width > 0 {
		for i, line := range lines {
			lines[i] = strings.TrimRight(fit(line, width), " ")
		}
	}
	return lines
}
func renderTop(w io.Writer, s Snapshot, by string, width int) error {
	if by == "json" {
		return json.NewEncoder(w).Encode(s)
	}
	for _, line := range topLines(s, by, width) {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
