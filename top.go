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
	Partition string    `json:"partition"`
	Name      string    `json:"name"`
	Nodes     string    `json:"nodes"`
	User      string    `json:"user_name"`
	State     []string  `json:"job_state"`
	StartTime slurmTime `json:"start_time"`
	EndTime   slurmTime `json:"end_time"`
	TimeLimit slurmTime `json:"time_limit"`
	Alloc     string    `json:"tres_alloc_str"`
	Requested string    `json:"tres_req_str"`
}
type queueResponse struct {
	Jobs []jobRecord `json:"jobs"`
}
type Job struct {
	ID             int    `json:"id"`
	User           string `json:"user"`
	Account        string `json:"account"`
	QoS            string `json:"qos"`
	Partition      string `json:"partition"`
	Name           string `json:"name"`
	State          string `json:"state"`
	Progress       int    `json:"progress_percent"`
	Elapsed        string `json:"elapsed"`
	ElapsedMinutes int    `json:"elapsed_minutes"`
	CPUs           int    `json:"cpus"`
	GPUs           int    `json:"gpus"`
	MemoryMB       int    `json:"memory_mb"`
	nodes          string
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
	UpdatedAt               time.Time `json:"updated_at"`
	Users                   []Usage   `json:"users"`
	RunningJobs             int       `json:"running_jobs"`
	PendingJobs             int       `json:"pending_jobs"`
	ActiveAccounts          int       `json:"active_accounts"`
	CapacityCPU             int       `json:"capacity_cpu"`
	CapacityGPU             int       `json:"capacity_gpu"`
	CapacityMemoryMB        int       `json:"capacity_memory_mb"`
	AllocatableCPU          int       `json:"allocatable_cpu"`
	AllocatableGPU          int       `json:"allocatable_gpu"`
	AllocatableMemoryMB     int       `json:"allocatable_memory_mb"`
	AllocatableCPUUsed      int       `json:"allocatable_cpu_used"`
	AllocatableGPUUsed      int       `json:"allocatable_gpu_used"`
	AllocatableMemoryUsedMB int       `json:"allocatable_memory_used_mb"`
	H200Capacity            int       `json:"h200_capacity"`
	AllocatableH200         int       `json:"allocatable_h200"`
	AllocatableH200Used     int       `json:"allocatable_h200_used"`
	H200Allocated           int       `json:"h200_allocated"`
	MIGCapacity             int       `json:"mig_capacity"`
	AllocatableMIG          int       `json:"allocatable_mig"`
	AllocatableMIGUsed      int       `json:"allocatable_mig_used"`
	MIGAllocated            int       `json:"mig_allocated"`
	OtherGPUCapacity        int       `json:"other_gpu_capacity"`
	OtherGPUAllocated       int       `json:"other_gpu_allocated"`
	Jobs                    []Job     `json:"jobs"`
	nodes                   []Node
	qosLimits               map[string]QoSLimit
	accounts                []AccountUsage
}

type AccountUsage struct {
	Account         string `json:"account"`
	RunningJobs     int    `json:"running_jobs"`
	PendingJobs     int    `json:"pending_jobs"`
	CPUs            int    `json:"cpus_allocated"`
	GPUs            int    `json:"gpus_allocated"`
	MemoryMB        int    `json:"memory_allocated_mb"`
	PendingCPUs     int    `json:"cpus_pending_requested"`
	PendingGPUs     int    `json:"gpus_pending_requested"`
	PendingMemoryMB int    `json:"memory_pending_requested_mb"`
}

type QoSLimit struct {
	Name           string
	GrpTRES        string
	GrpJobs        string
	GrpSubmit      string
	GrpWall        string
	MaxTRESPA      string
	MaxJobsPA      string
	MaxSubmitPA    string
	MaxTRESPU      string
	MaxJobsPU      string
	MaxSubmitPU    string
	MaxTRES        string
	MaxTRESPerNode string
	MinTRES        string
	MaxWall        string
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

func formatElapsed(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	minutes := seconds / 60
	return fmt.Sprintf("%d-%02d:%02d", minutes/(24*60), (minutes/60)%24, minutes%60)
}

func elapsedStatus(state []string, start, limit slurmTime, at time.Time) (string, int) {
	if !limit.Set || limit.Infinite || limit.Number <= 0 {
		return "0% [0-00:00|-]", 0
	}
	elapsed := int64(0)
	if slices.Contains(state, "RUNNING") && start.Set && !start.Infinite {
		elapsed = max(int64(0), at.Unix()-start.Number)
	}
	percent := min(100, int(elapsed*100/(limit.Number*60)))
	return fmt.Sprintf("%d%% [%s|%s]", percent, formatElapsed(elapsed), formatElapsed(limit.Number*60)), int(elapsed / 60)
}

func parseQueue(data []byte, nodes []Node, at time.Time) (Snapshot, error) {
	var response queueResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return Snapshot{}, fmt.Errorf("decode squeue JSON: %w", err)
	}
	snap := Snapshot{UpdatedAt: at, Users: []Usage{}, Jobs: []Job{}, nodes: append([]Node(nil), nodes...)}
	ignoredOther := 0
	for _, n := range nodes {
		snap.CapacityCPU += n.CPUTotal
		snap.CapacityGPU += n.GPUTotal
		classifyGPU(n.GPUType, n.GPUTotal, &snap.H200Capacity, &snap.MIGCapacity, &snap.OtherGPUCapacity)
		snap.CapacityMemoryMB += n.MemoryTotalMB
		if !unavailable(n.State) {
			snap.AllocatableCPU += n.CPUTotal
			snap.AllocatableGPU += n.GPUTotal
			snap.AllocatableMemoryMB += n.MemoryTotalMB
			snap.AllocatableCPUUsed += n.CPUAllocated
			snap.AllocatableGPUUsed += n.GPUAllocated
			snap.AllocatableMemoryUsedMB += n.MemoryAllocatedMB
			classifyGPU(n.GPUType, n.GPUTotal, &snap.AllocatableH200, &snap.AllocatableMIG, &ignoredOther)
			classifyGPU(n.GPUType, n.GPUAllocated, &snap.AllocatableH200Used, &snap.AllocatableMIGUsed, &ignoredOther)
		}
	}
	users := map[string]*Usage{}
	accMap := map[string]*AccountUsage{}
	accounts := map[string]struct{}{}
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
		if j.Account != "" {
			accounts[j.Account] = struct{}{}
		}
		j.QoS = printable(j.QoS)
		j.Partition = printable(j.Partition)
		j.Name = printable(j.Name)
		j.Nodes = printable(j.Nodes)
		u := users[j.User]
		if u == nil {
			u = &Usage{User: j.User}
			users[j.User] = u
		}
		accName := j.Account
		if accName == "" {
			accName = "(none)"
		}
		a := accMap[accName]
		if a == nil {
			a = &AccountUsage{Account: accName}
			accMap[accName] = a
		}
		if running {
			elapsed, elapsedMinutes := elapsedStatus(j.State, j.StartTime, j.TimeLimit, at)
			// AllocTRES is authoritative for running jobs. Never mix pending demand
			// into live allocations or silently substitute requested resources.
			u.RunningJobs++
			a.RunningJobs++
			snap.RunningJobs++
			cpus := number(tresValue(j.Alloc, "cpu"))
			gpus := gpuFromTRES(j.Alloc)
			mem := memoryMB(tresValue(j.Alloc, "mem"))
			u.CPUs += cpus
			a.CPUs += cpus
			u.GPUs += gpus
			a.GPUs += gpus
			u.MemoryMB += mem
			a.MemoryMB += mem
			gpuTypeCounts(j.Alloc, &snap.H200Allocated, &snap.MIGAllocated, &snap.OtherGPUAllocated)
			snap.Jobs = append(snap.Jobs, Job{ID: j.ID, User: j.User, Account: j.Account, QoS: j.QoS, Partition: j.Partition, Name: j.Name, State: "RUNNING", Progress: jobProgress(j.State, j.StartTime, j.EndTime, at), Elapsed: elapsed, ElapsedMinutes: elapsedMinutes, CPUs: cpus, GPUs: gpus, MemoryMB: mem, nodes: j.Nodes})
		} else {
			u.PendingJobs++
			a.PendingJobs++
			snap.PendingJobs++
			cpus := number(tresValue(j.Requested, "cpu"))
			gpus := gpuFromTRES(j.Requested)
			mem := memoryMB(tresValue(j.Requested, "mem"))
			u.PendingCPUs += cpus
			a.PendingCPUs += cpus
			u.PendingGPUs += gpus
			a.PendingGPUs += gpus
			u.PendingMemoryMB += mem
			a.PendingMemoryMB += mem
			elapsed, elapsedMinutes := elapsedStatus(j.State, j.StartTime, j.TimeLimit, at)
			snap.Jobs = append(snap.Jobs, Job{ID: j.ID, User: j.User, Account: j.Account, QoS: j.QoS, Partition: j.Partition, Name: j.Name, State: "PENDING", Elapsed: elapsed, ElapsedMinutes: elapsedMinutes, CPUs: cpus, GPUs: gpus, MemoryMB: mem, nodes: j.Nodes})
		}
	}
	for _, u := range users {
		snap.Users = append(snap.Users, *u)
	}
	for _, a := range accMap {
		snap.accounts = append(snap.accounts, *a)
	}
	snap.ActiveAccounts = len(accounts)
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
func ratioSortValue(numerator, denominator int) float64 {
	if denominator <= 0 {
		return -1
	}
	return float64(numerator) / float64(denominator)
}

func sortUsers(users []Usage, by string) {
	sort.Slice(users, func(i, j int) bool {
		a, b := users[i], users[j]
		switch by {
		case "cpu-gpu":
			left, right := ratioSortValue(a.CPUs, a.GPUs), ratioSortValue(b.CPUs, b.GPUs)
			if left != right {
				return left > right
			}
		case "memory-cpu":
			left, right := ratioSortValue(a.MemoryMB, a.CPUs), ratioSortValue(b.MemoryMB, b.CPUs)
			if left != right {
				return left > right
			}
		case "pending-cpu-gpu":
			left, right := ratioSortValue(a.PendingCPUs, a.PendingGPUs), ratioSortValue(b.PendingCPUs, b.PendingGPUs)
			if left != right {
				return left > right
			}
		case "pending-memory-cpu":
			left, right := ratioSortValue(a.PendingMemoryMB, a.PendingCPUs), ratioSortValue(b.PendingMemoryMB, b.PendingCPUs)
			if left != right {
				return left > right
			}
		}
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
		case "cpu-gpu", "memory-cpu", "pending-cpu-gpu", "pending-memory-cpu":
			// Ratio ties fall back to username below.
		case "user", "account":
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

func sortAccounts(accounts []AccountUsage, by string) {
	sort.Slice(accounts, func(i, j int) bool {
		a, b := accounts[i], accounts[j]
		switch by {
		case "cpu-gpu":
			left, right := ratioSortValue(a.CPUs, a.GPUs), ratioSortValue(b.CPUs, b.GPUs)
			if left != right {
				return left > right
			}
		case "memory-cpu":
			left, right := ratioSortValue(a.MemoryMB, a.CPUs), ratioSortValue(b.MemoryMB, b.CPUs)
			if left != right {
				return left > right
			}
		case "pending-cpu-gpu":
			left, right := ratioSortValue(a.PendingCPUs, a.PendingGPUs), ratioSortValue(b.PendingCPUs, b.PendingGPUs)
			if left != right {
				return left > right
			}
		case "pending-memory-cpu":
			left, right := ratioSortValue(a.PendingMemoryMB, a.PendingCPUs), ratioSortValue(b.PendingMemoryMB, b.PendingCPUs)
			if left != right {
				return left > right
			}
		}
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
		case "cpu-gpu", "memory-cpu", "pending-cpu-gpu", "pending-memory-cpu":
			// Ratio ties fall back to account name below.
		case "account", "user":
			return a.Account < b.Account
		default:
			left, right = a.GPUs, b.GPUs
		}
		if left != right {
			return left > right
		}
		return a.Account < b.Account
	})
}

func collectAccounts(snap Snapshot) []AccountUsage {
	if len(snap.accounts) > 0 {
		return append([]AccountUsage(nil), snap.accounts...)
	}
	accMap := make(map[string]*AccountUsage)
	for _, j := range snap.Jobs {
		accName := j.Account
		if accName == "" {
			accName = "(none)"
		}
		a := accMap[accName]
		if a == nil {
			a = &AccountUsage{Account: accName}
			accMap[accName] = a
		}
		if j.State == "RUNNING" {
			a.RunningJobs++
			a.CPUs += j.CPUs
			a.GPUs += j.GPUs
			a.MemoryMB += j.MemoryMB
		} else if j.State == "PENDING" {
			a.PendingJobs++
			a.PendingCPUs += j.CPUs
			a.PendingGPUs += j.GPUs
			a.PendingMemoryMB += j.MemoryMB
		}
	}
	result := make([]AccountUsage, 0, len(accMap))
	for _, a := range accMap {
		result = append(result, *a)
	}
	return result
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

func ratioDecimal(numerator float64, denominator int) string {
	if denominator <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", numerator/float64(denominator))
}

func cpuGPU(cpus, gpus int) string { return ratioDecimal(float64(cpus), gpus) }
func memoryCPU(memoryMiB, cpus int) string {
	return ratioDecimal(float64(memoryMiB)*1_048_576/1_000_000_000, cpus)
}

func userHeader(by string, asc bool) string {
	return "\x1b[1m" + strings.Join([]string{
		leftHeader("USER", "user", by, asc, 16),
		leftHeader("RUN", "jobs", by, asc, 6),
		leftHeader("GPU", "gpu", by, asc, 6),
		leftHeader("CPU", "cpu", by, asc, 6),
		leftHeader("C:G", "cpu-gpu", by, asc, 6),
		leftHeader("MEM", "mem", by, asc, 6),
		leftHeader("M:C", "memory-cpu", by, asc, 6),
		" ",
		leftHeader("PEND", "pending-jobs", by, asc, 6),
		leftHeader("GPU", "pending-gpu", by, asc, 6),
		leftHeader("CPU", "pending-cpu", by, asc, 6),
		leftHeader("C:G", "pending-cpu-gpu", by, asc, 6),
		leftHeader("MEM", "pending-mem", by, asc, 6),
		leftHeader("M:C", "pending-memory-cpu", by, asc, 6),
	}, " ") + "\x1b[22m"
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
	snap, err := parseQueue(raw, parseNodes(string(data), true), time.Now())
	if err != nil {
		return Snapshot{}, err
	}
	qosCmd := exec.CommandContext(ctx, "sacctmgr", "show", "qos", "-p")
	if qosRaw, err := qosCmd.Output(); err == nil {
		snap.qosLimits = parseQoSLimits(qosRaw)
	}
	return snap, nil
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
		fmt.Sprintf("slurm-top  %s    %d users / %d accounts / %d running / %d pending", s.UpdatedAt.Format("15:04:05"), len(s.Users), s.ActiveAccounts, s.RunningJobs, s.PendingJobs),
		fmt.Sprintf("GPU allocated  %s %d/%d", percentBar(g, s.CapacityGPU, 20), g, s.CapacityGPU),
		fmt.Sprintf("CPU allocated  %s %d/%d", percentBar(c, s.CapacityCPU, 20), c, s.CapacityCPU),
		fmt.Sprintf("MEM allocated  %s %s/%sT", percentBar(m, s.CapacityMemoryMB, 20), tb(m), tb(s.CapacityMemoryMB)),
		"Bars = allocations / cluster capacity. Not measured utilization.",
		userHeader(by, false),
	}
	for _, u := range users {
		lines = append(lines, fmt.Sprintf("%-16.16s %6d %6d %6d %6s %6s %6s  %6d %6d %6d %6s %6s %6s", u.User, u.RunningJobs, u.GPUs, u.CPUs, cpuGPU(u.CPUs, u.GPUs), gb(u.MemoryMB)+"G", memoryCPU(u.MemoryMB, u.CPUs), u.PendingJobs, u.PendingGPUs, u.PendingCPUs, cpuGPU(u.PendingCPUs, u.PendingGPUs), gb(u.PendingMemoryMB)+"G", memoryCPU(u.PendingMemoryMB, u.PendingCPUs)))
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
