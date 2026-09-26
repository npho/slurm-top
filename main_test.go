package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseNodes(t *testing.T) {
	input := `NodeName=g010 State=MIXED+RESERVED CfgTRES=cpu=64,gres/gpu=8,gres/gpu:h200=8 AllocTRES=cpu=16,gres/gpu=3 Gres=gpu:h200:8(S:0-1) CPUEfctv=64 CPUAlloc=16 CPULoad=2.50 RealMemory=102400 AllocMem=10240 FreeMem=80000 BootTime=2025-01-02T03:04:05 Reason=reserved for maintenance
NodeName=g002 State=IDLE Gres=gpu:a100:4(S:0-1) CPUTot=32 CPUAlloc=0 RealMemory=64000 FreeMem=50000
NodeName=c001 State=IDLE CPUTot=32 RealMemory=64000`
	nodes := parseNodes(input, false)
	if len(nodes) != 2 || nodes[0].Name != "g002" || nodes[1].Name != "g010" {
		t.Fatalf("nodes: %+v", nodes)
	}
	n := nodes[1]
	if n.GPUTotal != 8 || n.GPUAllocated != 3 || n.GPUType != "h200" || n.h200Total != 8 || n.gpuAllocationTyped || n.bootTime.IsZero() || n.CPUTotal != 64 || n.CPUAllocated != 16 || n.CPULoad == nil || *n.CPULoad != 2.5 || n.MemoryFreeMB == nil || *n.MemoryFreeMB != 80000 || n.Reason != "reserved for maintenance" {
		t.Fatalf("node: %+v", n)
	}
	if !unavailable(n.State) {
		t.Fatal("reserved nodes should not count as available")
	}
	if len(parseNodes(input, true)) != 3 {
		t.Fatal("--all must include CPU nodes")
	}
}
func TestTypedCountsAndMissingMetrics(t *testing.T) {
	nodes := parseNodes("NodeName=x State=IDLE CfgTRES=gres/gpu:a100=2,gres/gpu:h200=4 AllocTRES=gres/gpu:a100=1,gres/gpu:h200=2 CPULoad=N/A FreeMem=N/A", false)
	if len(nodes) != 1 || nodes[0].GPUTotal != 6 || nodes[0].GPUAllocated != 3 || nodes[0].GPUType != "mixed" || nodes[0].h200Total != 4 || nodes[0].h200Allocated != 2 || !nodes[0].gpuAllocationTyped || nodes[0].CPULoad != nil || nodes[0].MemoryFreeMB != nil {
		t.Fatalf("nodes: %+v", nodes)
	}
}
func TestTBFormatsDecimalTerabytes(t *testing.T) {
	if got := tb(1_000_000); got != "1.0" {
		t.Fatalf("tb(1000000) = %s, want 1.0", got)
	}
}

func TestGBRoundsMiBToNearestDecimalGB(t *testing.T) {
	for mib, want := range map[int]string{0: "0", 476: "0", 477: "1", 1024: "1", 1536: "2"} {
		if got := gb(mib); got != want {
			t.Errorf("gb(%d) = %s, want %s", mib, got, want)
		}
	}
}

func TestRenderAndArgs(t *testing.T) {
	var out bytes.Buffer
	n := parseNodes("NodeName=x State=IDLE Gres=gpu:2 CPUTot=4", false)
	if err := render(&out, n, "json", time.Unix(0, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Nodes []Node `json:"nodes"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Nodes) != 1 {
		t.Fatalf("JSON: %s: %v", out.String(), err)
	}
	for _, args := range [][]string{{"--format", "xml"}, {"--timeout", "0s"}, {"extra"}} {
		if err := run(args, &out, &out); err == nil {
			t.Fatalf("expected error for %v", args)
		}
	}
	out.Reset()
	if err := run([]string{"--help"}, &out, &out); err != nil || !strings.Contains(out.String(), "--watch") {
		t.Fatalf("help: %s: %v", out.String(), err)
	}
}
