package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestDashboard(t *testing.T) {
	nodes := parseNodes("NodeName=g002 State=MIXED CfgTRES=gres/gpu=4,gres/gpu:h200=4 AllocTRES=gres/gpu=2 CPUAlloc=8 CPUTot=16 RealMemory=10240 AllocMem=5120 FreeMem=2560\nNodeName=g003 State=DOWN Gres=gpu:a100:2 CPUTot=16 RealMemory=10240", false)
	var out bytes.Buffer
	if err := renderDashboard(&out, nodes, time.Unix(0, 0), 90, false); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"2 nodes", "2/6 GPUs allocated", "2 free on usable nodes", "1 unavailable/restricted", "●●○○", "CPU alloc", "Mem alloc", "Mem used~", "× unavailable"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "\x1b[") {
		t.Fatal("ANSI in uncolored output")
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "╭") || strings.HasPrefix(line, "│") || strings.HasPrefix(line, "╰") {
			if utf8.RuneCountInString(line) != 89 {
				t.Errorf("card row width %d: %q", utf8.RuneCountInString(line), line)
			}
		}
	}
	out.Reset()
	if err := renderDashboard(&out, nodes, time.Unix(0, 0), 32, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\x1b[") {
		t.Fatal("missing terminal color")
	}
}
func TestDashboardWrapsGPUCircles(t *testing.T) {
	lines := card(Node{Name: "mig", State: "IDLE", GPUType: "h200-mig", GPUTotal: 12, GPUAllocated: 10}, 44, false)
	gpuLines := 0
	for _, line := range lines {
		if strings.Contains(line, "●") || strings.Contains(line, "○") {
			gpuLines++
		}
	}
	if gpuLines != 2 {
		t.Fatalf("GPU lines = %d, want 2: %q", gpuLines, lines)
	}
}

func TestBarMissingAndClamp(t *testing.T) {
	if bar(0, 0, 8) != "????????" || bar(30, 10, 8) != "████████" || bar(-1, 10, 8) != "░░░░░░░░" {
		t.Fatal("bad bar")
	}
}
