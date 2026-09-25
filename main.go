package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"golang.org/x/term"
)

// These are Slurm scheduler values, NOT sampled hardware utilization.
type Node struct {
	Name               string   `json:"name"`
	State              string   `json:"state"`
	GPUType            string   `json:"gpu_type,omitempty"`
	GPUTotal           int      `json:"gpu_total"`
	GPUAllocated       int      `json:"gpu_allocated"`
	CPUTotal           int      `json:"cpu_total"`
	CPUAllocated       int      `json:"cpu_allocated"`
	CPULoad            *float64 `json:"cpu_load,omitempty"` // Slurm-reported load average, not CPU percent.
	MemoryTotalMB      int      `json:"memory_total_mb"`
	MemoryAllocatedMB  int      `json:"memory_allocated_mb"`
	MemoryFreeMB       *int     `json:"memory_free_mb,omitempty"`        // Slurm-reported OS free memory, not unallocated memory.
	MemoryUsedApproxMB *int     `json:"memory_used_approx_mb,omitempty"` // RealMemory - FreeMem, when comparable.
	Reason             string   `json:"reason,omitempty"`
}

var fieldRE = regexp.MustCompile(`(?:^|\s)([A-Za-z][A-Za-z0-9_]*)=([^\s]+)`)
var gresRE = regexp.MustCompile(`^gpu(?::([^:,()]+))?:(\d+)(?:\([^)]*\))?$`)

func fields(line string) map[string]string {
	result := make(map[string]string)
	for _, m := range fieldRE.FindAllStringSubmatch(line, -1) {
		result[m[1]] = m[2]
	}
	return result
}
func number(s string) int { n, _ := strconv.Atoi(s); return n }

// gpuCount prefers the generic TRES (which already includes all types) to
// avoid double counting. Older Slurm installations may only expose Gres.
func gpuCount(tres, gres string) (int, string) {
	typed := 0
	typ := ""
	for _, item := range strings.Split(tres, ",") {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if key == "gres/gpu" {
			return number(value), gpuType(tres, gres)
		}
		if strings.HasPrefix(key, "gres/gpu:") {
			typed += number(value)
			if typ == "" {
				typ = strings.TrimPrefix(key, "gres/gpu:")
			} else if typ != strings.TrimPrefix(key, "gres/gpu:") {
				typ = "mixed"
			}
		}
	}
	if typed > 0 {
		return typed, typ
	}
	total := 0
	for _, item := range strings.Split(gres, ",") {
		m := gresRE.FindStringSubmatch(item)
		if m == nil {
			continue
		}
		total += number(m[2])
		if m[1] != "" {
			if typ == "" {
				typ = m[1]
			} else if typ != m[1] {
				typ = "mixed"
			}
		}
	}
	if total > 0 && typ == "" {
		typ = "gpu"
	}
	return total, typ
}
func gpuType(tres, gres string) string {
	typ := ""
	for _, item := range strings.Split(tres, ",") {
		key, _, ok := strings.Cut(item, "=")
		if !ok || !strings.HasPrefix(key, "gres/gpu:") {
			continue
		}
		t := strings.TrimPrefix(key, "gres/gpu:")
		if typ == "" {
			typ = t
		} else if typ != t {
			return "mixed"
		}
	}
	if typ != "" {
		return typ
	}
	for _, item := range strings.Split(gres, ",") {
		if m := gresRE.FindStringSubmatch(item); m != nil && m[1] != "" {
			return m[1]
		}
	}
	return "gpu"
}
func parseNodes(output string, all bool) []Node {
	nodes := []Node{}
	for _, line := range strings.Split(output, "\n") {
		f := fields(line)
		if f["NodeName"] == "" || f["State"] == "" {
			continue
		}
		total, typ := gpuCount(f["CfgTRES"], f["Gres"])
		if !all && total == 0 {
			continue
		}
		allocated, _ := gpuCount(f["AllocTRES"], "")
		if allocated > total {
			allocated = total
		}
		n := Node{Name: f["NodeName"], State: f["State"], GPUType: typ,
			GPUTotal: total, GPUAllocated: allocated, CPUTotal: number(f["CPUEfctv"]),
			CPUAllocated: number(f["CPUAlloc"]), MemoryTotalMB: number(f["RealMemory"]), MemoryAllocatedMB: number(f["AllocMem"])}
		if n.CPUTotal == 0 {
			n.CPUTotal = number(f["CPUTot"])
		}
		if v, err := strconv.ParseFloat(f["CPULoad"], 64); err == nil && v >= 0 {
			n.CPULoad = &v
		}
		if v, err := strconv.Atoi(f["FreeMem"]); err == nil && v >= 0 {
			n.MemoryFreeMB = &v
			if n.MemoryTotalMB > 0 && v <= n.MemoryTotalMB {
				used := n.MemoryTotalMB - v
				n.MemoryUsedApproxMB = &used
			}
		}
		if i := strings.Index(line, " Reason="); i >= 0 {
			n.Reason = strings.TrimSpace(line[i+len(" Reason="):])
		}
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return naturalLess(nodes[i].Name, nodes[j].Name) })
	return nodes
}

var suffixRE = regexp.MustCompile(`^(.*?)(\d+)$`)

func naturalLess(a, b string) bool {
	x, y := suffixRE.FindStringSubmatch(a), suffixRE.FindStringSubmatch(b)
	if x != nil && y != nil && x[1] == y[1] {
		i, ei := strconv.ParseUint(x[2], 10, 64)
		j, ej := strconv.ParseUint(y[2], 10, 64)
		if ei == nil && ej == nil && i != j {
			return i < j
		}
	}
	return a < b
}
func unavailable(state string) bool {
	s := strings.ToUpper(state)
	for _, word := range []string{"DOWN", "DRAIN", "FAIL", "NOT_RESPONDING", "NO_RESPOND", "MAINT", "INVAL", "RESERVED", "PLANNED"} {
		if strings.Contains(s, word) {
			return true
		}
	}
	return false
}
func collect(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "scontrol", "show", "node", "-o")
	data, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("scontrol: %s: %w", strings.TrimSpace(string(exit.Stderr)), err)
		}
		return nil, fmt.Errorf("scontrol: %w", err)
	}
	return data, nil
}
func ratio(a, b int) string {
	if b <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", a, b, float64(a)*100/float64(b))
}
func gib(mb int) string { return fmt.Sprintf("%.1f", float64(mb)/1024) }
func render(w io.Writer, nodes []Node, format string, at time.Time) error {
	if format == "grid" {
		return renderGrid(w, nodes, at, 100, 0, 0, false)
	}
	if format == "detail" {
		return renderDashboard(w, nodes, at, 100, false)
	}
	if format == "json" {
		return json.NewEncoder(w).Encode(struct {
			UpdatedAt time.Time `json:"updated_at"`
			Nodes     []Node    `json:"nodes"`
		}{at, nodes})
	}
	total, alloc, free := 0, 0, 0
	for _, n := range nodes {
		total += n.GPUTotal
		alloc += n.GPUAllocated
		if !unavailable(n.State) {
			free += n.GPUTotal - n.GPUAllocated
		}
	}
	fmt.Fprintf(w, "Slurm nodes  %s  |  GPUs %d allocated / %d total, %d on allocatable nodes\n", at.Format(time.RFC3339), alloc, total, free)
	t := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(t, "NODE\tSTATE\tGPU TYPE\tGPU ALLOC/TOTAL\tCPU ALLOC/TOTAL\tCPU LOAD\tMEM ALLOC GiB\tMEM USED ~\tMEM FREE GiB")
	for _, n := range nodes {
		load, mem, used := "-", "-", "-"
		if n.CPULoad != nil {
			load = fmt.Sprintf("%.2f", *n.CPULoad)
		}
		if n.MemoryFreeMB != nil {
			mem = gib(*n.MemoryFreeMB)
		}
		if n.MemoryUsedApproxMB != nil {
			used = fmt.Sprintf("%s GiB (%.0f%%)", gib(*n.MemoryUsedApproxMB), float64(*n.MemoryUsedApproxMB)*100/float64(n.MemoryTotalMB))
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s/%s\t%s\t%s\n", n.Name, n.State, n.GPUType, ratio(n.GPUAllocated, n.GPUTotal), ratio(n.CPUAllocated, n.CPUTotal), load, gib(n.MemoryAllocatedMB), gib(n.MemoryTotalMB), used, mem)
	}
	return t.Flush()
}
func run(args []string, out, errOut io.Writer) error {
	flags := pflag.NewFlagSet("slurm-top", pflag.ContinueOnError)
	flags.SetOutput(errOut)
	flags.Bool("all", false, "Include non-GPU nodes")
	flags.String("format", "top", "Output format: top, json, grid, detail or table")
	flags.String("sort", "gpu", "Sort users by gpu, cpu, mem, jobs or user")
	flags.Bool("interactive", false, "Use an alternate-screen, paged terminal UI")
	flags.Duration("watch", 0, "Refresh interval (e.g. 5s; zero prints once)")
	flags.Duration("timeout", 10*time.Second, "Timeout for each Slurm query")
	flags.BoolP("help", "h", false, "Show help")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	v := viper.New()
	if err := v.BindPFlags(flags); err != nil {
		return err
	}
	if v.GetBool("help") {
		fmt.Fprintln(out, "Usage: slurm-top [--format top|json|grid|detail|table] [--sort gpu|cpu|mem|jobs|user] [--interactive] [--watch 5s]")
		flags.SetOutput(out)
		flags.PrintDefaults()
		return nil
	}
	format, watch, timeout := v.GetString("format"), v.GetDuration("watch"), v.GetDuration("timeout")
	if format == "dashboard" {
		format = "grid"
	} // compatibility
	if format != "top" && format != "grid" && format != "detail" && format != "table" && format != "json" {
		return fmt.Errorf("invalid --format %q (use top, json, grid, detail or table)", format)
	}
	sortBy := v.GetString("sort")
	if sortBy != "gpu" && sortBy != "cpu" && sortBy != "mem" && sortBy != "jobs" && sortBy != "user" {
		return fmt.Errorf("invalid --sort %q", sortBy)
	}
	if watch < 0 || timeout <= 0 {
		return fmt.Errorf("--watch must be nonnegative and --timeout must be positive")
	}
	if format == "top" || format == "json" {
		return runTop(out, format, sortBy, v.GetBool("interactive"), watch, timeout)
	}
	if v.GetBool("interactive") {
		if format == "table" {
			return fmt.Errorf("--interactive requires --format grid or detail")
		}
		interval := watch
		if interval == 0 {
			interval = 5 * time.Second
		}
		return runInteractive(os.Stdin, os.Stdout, v.GetBool("all"), format, interval, timeout)
	}
	terminal, width := false, 100
	if f, ok := out.(*os.File); ok && term.IsTerminal(int(f.Fd())) && os.Getenv("TERM") != "dumb" {
		terminal = true
		if w, _, err := term.GetSize(int(f.Fd())); err == nil {
			width = w
		}
	}
	color := terminal && os.Getenv("NO_COLOR") == "" && os.Getenv("CLICOLOR") != "0"
	root, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	first := true
	for {
		ctx, cancel := context.WithTimeout(root, timeout)
		data, err := collect(ctx)
		cancel()
		if root.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		nodes := parseNodes(string(data), v.GetBool("all"))
		if len(nodes) == 0 {
			return errors.New("Slurm returned no matching nodes (try --all)")
		}
		if terminal && (format == "grid" || format == "detail") && watch > 0 && !first {
			if _, err := fmt.Fprint(out, "\x1b[H\x1b[2J"); err != nil {
				return err
			}
		}
		if format == "grid" || format == "detail" {
			if terminal {
				if f, ok := out.(*os.File); ok {
					if w, _, err := term.GetSize(int(f.Fd())); err == nil {
						width = w
					}
				}
			}
			var e error
			if format == "grid" {
				e = renderGrid(out, nodes, time.Now(), width, 0, 0, color)
			} else {
				e = renderDashboard(out, nodes, time.Now(), width, color)
			}
			if e != nil {
				return e
			}
		} else if err := render(out, nodes, format, time.Now()); err != nil {
			return err
		}
		if watch == 0 {
			return nil
		}
		if format != "json" && (!terminal || format == "table") {
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
		}
		first = false
		select {
		case <-root.Done():
			return nil
		case <-time.After(watch):
		}
	}
}
func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "slurm-top:", err)
		os.Exit(1)
	}
}
