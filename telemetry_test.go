package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestParseGresIndices(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []int
	}{
		{
			name:  "single index",
			input: []string{"gpu:1(IDX:3)"},
			want:  []int{3},
		},
		{
			name:  "range of indices",
			input: []string{"gpu:h200:4(IDX:0-3)"},
			want:  []int{0, 1, 2, 3},
		},
		{
			name:  "comma separated",
			input: []string{"gpu:2(IDX:0,2)"},
			want:  []int{0, 2},
		},
		{
			name:  "compound ranges and singles",
			input: []string{"gpu:4(IDX:0-1,4-5)"},
			want:  []int{0, 1, 4, 5},
		},
		{
			name:  "no idx specified",
			input: []string{"gpu:1", "gpu:h200:2"},
			want:  nil,
		},
		{
			name:  "empty",
			input: nil,
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGresIndices(tc.input)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseGresIndices(%v) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestParsePrometheusResponse(t *testing.T) {
	raw := []byte(`{
		"status": "success",
		"data": {
			"resultType": "vector",
			"result": [
				{
					"metric": {"gpu": "0", "Hostname": "g001"},
					"value": [1790808543.0, "95.5"]
				},
				{
					"metric": {"gpu": "1", "Hostname": "g001"},
					"value": [1790808543.0, "0"]
				}
			]
		}
	}`)

	series, err := parsePrometheusResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}
	if series[0].Metric["gpu"] != "0" || series[0].Value != 95.5 {
		t.Errorf("series[0] mismatch: %+v", series[0])
	}
	if series[1].Metric["gpu"] != "1" || series[1].Value != 0.0 {
		t.Errorf("series[1] mismatch: %+v", series[1])
	}

	// Error response
	errRaw := []byte(`{"status": "error", "error": "bad query"}`)
	if _, err := parsePrometheusResponse(errRaw); err == nil {
		t.Error("expected error for status=error")
	}

	// Malformed JSON
	if _, err := parsePrometheusResponse([]byte(`{bad json`)); err == nil {
		t.Error("expected error for invalid json")
	}
}

func TestNodeTelemetryCalculations(t *testing.T) {
	activeCores := 8.5
	usedMem := 32768
	totalMem := 65536
	telem := &NodeTelemetry{
		NodeName: "g001",
		GPUUtils: map[int]float64{
			0: 80.0,
			1: 0.0,
			2: 50.0,
			3: 0.0,
		},
		GPUMemUsedMB: map[int]int{
			0: 30000,
			1: 0,
			2: 20000,
			3: 0,
		},
		GPUMemTotalMB: map[int]int{
			0: 141000,
			1: 141000,
			2: 141000,
			3: 141000,
		},
		NodeCPUActiveCores: &activeCores,
		NodeMemUsedMB:      &usedMem,
		NodeMemTotalMB:     &totalMem,
	}

	if telem.TotalGPUMemUsedMB() != 50000 {
		t.Errorf("TotalGPUMemUsedMB() = %d, want 50000", telem.TotalGPUMemUsedMB())
	}
	if telem.TotalGPUMemMB() != 564000 {
		t.Errorf("TotalGPUMemMB() = %d, want 564000", telem.TotalGPUMemMB())
	}
	if telem.ActiveGPUCount() != 2 {
		t.Errorf("ActiveGPUCount() = %d, want 2", telem.ActiveGPUCount())
	}
	if telem.ActiveCPUCores() != 8.5 {
		t.Errorf("ActiveCPUCores() = %f, want 8.5", telem.ActiveCPUCores())
	}
}

func TestFetchNodeTelemetryWithMockServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(q, "DCGM_FI_DEV_GPU_UTIL"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"gpu":"0","Hostname":"testnode"},"value":[1000,"85.0"]},
				{"metric":{"gpu":"1","Hostname":"testnode"},"value":[1000,"0.0"]}
			]}}`)
		case strings.HasPrefix(q, "DCGM_FI_DEV_FB_USED"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"gpu":"0","Hostname":"testnode"},"value":[1000,"32000"]},
				{"metric":{"gpu":"1","Hostname":"testnode"},"value":[1000,"500"]}
			]}}`)
		case strings.HasPrefix(q, "DCGM_FI_DEV_FB_FREE"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"gpu":"0","Hostname":"testnode"},"value":[1000,"109000"]},
				{"metric":{"gpu":"1","Hostname":"testnode"},"value":[1000,"140500"]}
			]}}`)
		case strings.HasPrefix(q, "rate(cgroup_cpu_total_seconds"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"jobid":"101","instance":"testnode.local:9306"},"value":[1000,"3.45"]}
			]}}`)
		case strings.HasPrefix(q, "cgroup_memory_rss_bytes"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"jobid":"101","instance":"testnode.local:9306"},"value":[1000,"17179869184"]}
			]}}`)
		case strings.HasPrefix(q, "cgroup_memory_used_bytes"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"jobid":"101","instance":"testnode.local:9306"},"value":[1000,"21474836480"]}
			]}}`)
		case strings.HasPrefix(q, "sum(rate(node_cpu_seconds_total"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{},"value":[1000,"6.8"]}
			]}}`)
		case strings.HasPrefix(q, "(node_memory_MemTotal_bytes"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{},"value":[1000,"65536"]}
			]}}`)
		default:
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
		}
	}))
	defer server.Close()

	cfg := PrometheusConfig{
		URL:      server.URL,
		Username: "user",
		Password: "pass",
	}

	telem, err := fetchNodeTelemetry(context.Background(), cfg, "testnode")
	if err != nil {
		t.Fatalf("fetchNodeTelemetry error: %v", err)
	}
	if telem == nil {
		t.Fatal("expected non-nil telemetry")
	}

	if telem.GPUUtils[0] != 85.0 || telem.GPUUtils[1] != 0.0 {
		t.Errorf("unexpected GPUUtils: %v", telem.GPUUtils)
	}
	if telem.GPUMemUsedMB[0] != 32000 || telem.GPUMemTotalMB[0] != 141000 {
		t.Errorf("unexpected GPUMem for GPU 0: used=%d, total=%d", telem.GPUMemUsedMB[0], telem.GPUMemTotalMB[0])
	}
	if telem.NodeCPUActiveCores == nil || *telem.NodeCPUActiveCores != 6.8 {
		t.Errorf("unexpected NodeCPUActiveCores: %v", telem.NodeCPUActiveCores)
	}
	if telem.NodeMemUsedMB == nil || *telem.NodeMemUsedMB != 65536 {
		t.Errorf("unexpected NodeMemUsedMB: %v", telem.NodeMemUsedMB)
	}
	if telem.JobCPUCores[101] != 3.45 {
		t.Errorf("unexpected JobCPUCores[101]: %f", telem.JobCPUCores[101])
	}
	if telem.JobMemRSSMB[101] != 16384 {
		t.Errorf("unexpected JobMemRSSMB[101]: %d", telem.JobMemRSSMB[101])
	}
	if telem.JobMemUsedMB[101] != 20480 {
		t.Errorf("unexpected JobMemUsedMB[101]: %d", telem.JobMemUsedMB[101])
	}
}

func TestNodeAndJobUtilizationBarsWithTelemetry(t *testing.T) {
	node := Node{
		Name:              "g001",
		State:             "ALLOCATED",
		GPUType:           "h200",
		GPUTotal:          8,
		GPUAllocated:      8,
		h200Total:         8,
		h200Allocated:     8,
		CPUTotal:          64,
		CPUAllocated:      64,
		MemoryTotalMB:     2048000,
		MemoryAllocatedMB: 1536000,
	}

	job := Job{
		ID:         101,
		User:       "alice",
		Account:    "lab",
		State:      "RUNNING",
		CPUs:       32,
		GPUs:       4,
		MemoryMB:   800000,
		nodes:      "g001",
		gpuIndices: []int{0, 1, 2, 3},
	}

	activeCores := 12.0
	nodeMemUsed := 400000
	telem := &NodeTelemetry{
		NodeName: "g001",
		GPUUtils: map[int]float64{
			0: 90.0,
			1: 85.0,
			2: 50.0,
			3: 10.0,
			4: 0.0,
			5: 0.0,
			6: 0.0,
			7: 0.0,
		},
		GPUMemUsedMB: map[int]int{
			0: 30000,
			1: 30000,
			2: 30000,
			3: 30000,
			4: 0,
			5: 0,
			6: 0,
			7: 0,
		},
		GPUMemTotalMB: map[int]int{
			0: 141000,
			1: 141000,
			2: 141000,
			3: 141000,
			4: 141000,
			5: 141000,
			6: 141000,
			7: 141000,
		},
		NodeCPUActiveCores: &activeCores,
		NodeMemUsedMB:      &nodeMemUsed,
		JobCPUCores: map[int]float64{
			101: 5.2,
		},
		JobMemUsedMB: map[int]int{
			101: 150000,
		},
	}

	// 1. Without telemetry: UTILIZED box shows question marks for unmeasured fields
	utilizedWithout := strings.Join(nodeUtilizationBars(node, 100, false), "\n")
	if !strings.Contains(utilizedWithout, "?") {
		t.Errorf("expected placeholder '?' when telemetry is nil: %s", utilizedWithout)
	}

	// 2. With telemetry: UTILIZED box shows observed numbers
	utilizedWith := strings.Join(nodeUtilizationBars(node, 100, false, telem), "\n")
	if strings.Contains(utilizedWith, "?  ?/64") {
		t.Errorf("CPU should not have '?' with telemetry: %s", utilizedWith)
	}
	if !strings.Contains(utilizedWith, "12/64") {
		t.Errorf("expected CPU to show '12/64': %s", utilizedWith)
	}
	if !strings.Contains(utilizedWith, "4/8") {
		t.Errorf("expected H200 to show '4/8' active GPUs: %s", utilizedWith)
	}

	// 3. Per-job without telemetry
	jobWithout := strings.Join(nodeJobUtilizationBars(job, false), "\n")
	if !strings.Contains(jobWithout, "?  ?/4") {
		t.Errorf("expected job GPU placeholder '?  ?/4': %s", jobWithout)
	}

	// 4. Per-job with telemetry:
	// CPU: 5.2 active cores on 32 allocated cores -> ceiling 6 -> 6/32 (19%)
	// GPU: 90+85+50+10 = 235% compute util on 4 GPUs -> ceiling 2.35 = 3 -> 3/4 (59% saturation)
	// GPU Heading: strictly "GPU" (not "GPU (59%)")
	jobWith := strings.Join(nodeJobUtilizationBars(job, false, telem), "\n")
	if !strings.Contains(jobWith, "3/4") {
		t.Errorf("expected job GPU to show '3/4': %s", jobWith)
	}
	if !strings.Contains(jobWith, "59%") {
		t.Errorf("expected job GPU to show '59%%': %s", jobWith)
	}
	if strings.Contains(jobWith, "GPU (") {
		t.Errorf("expected job GPU heading strictly 'GPU', got: %s", jobWith)
	}
	if !strings.Contains(jobWith, "6/32") {
		t.Errorf("expected job CPU to show '6/32': %s", jobWith)
	}
	if !strings.Contains(jobWith, "19%") {
		t.Errorf("expected job CPU to show '19%%': %s", jobWith)
	}
	if !strings.Contains(jobWith, "157/839G") {
		t.Errorf("expected job MEM to show '157/839G': %s", jobWith)
	}
	if !strings.Contains(jobWith, "126/591G") {
		t.Errorf("expected job GPU MEM to show '126/591G': %s", jobWith)
	}
}

func TestNodeDetailsWithTelemetry(t *testing.T) {
	node := Node{
		Name:              "g001",
		State:             "ALLOCATED",
		GPUType:           "h200",
		GPUTotal:          8,
		GPUAllocated:      8,
		h200Total:         8,
		h200Allocated:     8,
		CPUTotal:          64,
		CPUAllocated:      64,
		MemoryTotalMB:     2048000,
		MemoryAllocatedMB: 1536000,
	}
	jobs := []Job{
		{
			ID:         338067,
			User:       "yandabao",
			Account:    "weirdlab",
			State:      "RUNNING",
			CPUs:       32,
			GPUs:       4,
			MemoryMB:   800000,
			nodes:      "g001",
			gpuIndices: []int{0, 1, 2, 3},
		},
	}
	activeCores := 8.5
	nodeMem := 350000
	telem := &NodeTelemetry{
		NodeName:           "g001",
		GPUUtils:           map[int]float64{0: 5, 1: 6, 2: 90, 3: 84, 4: 100, 5: 0, 6: 0, 7: 100},
		GPUMemUsedMB:       map[int]int{0: 30000, 1: 30000, 2: 30000, 3: 30000, 4: 130000, 5: 5000, 6: 0, 7: 50000},
		GPUMemTotalMB:      map[int]int{0: 141000, 1: 141000, 2: 141000, 3: 141000, 4: 141000, 5: 141000, 6: 141000, 7: 141000},
		NodeCPUActiveCores: &activeCores,
		NodeMemUsedMB:      &nodeMem,
		JobCPUCores:        map[int]float64{338067: 5.14},
		JobMemUsedMB:       map[int]int{338067: 153545},
	}
	lines := nodeDetails(node, jobs, 80, 25, 0, false, telem)
	output := strings.Join(lines, "\n")
	if !strings.Contains(output, "yandabao") {
		t.Fatalf("missing job user yandabao in nodeDetails output:\n%s", output)
	}
	if !strings.Contains(output, "weirdlab") {
		t.Fatalf("missing job account weirdlab in nodeDetails output:\n%s", output)
	}
	if !strings.Contains(output, "UTILIZED") {
		t.Fatalf("missing UTILIZED box:\n%s", output)
	}
	// 5.14 cores on 32 allocated cores -> ceiling 6 -> 6/32
	if !strings.Contains(output, "6/32") {
		t.Fatalf("missing measured CPU 6/32:\n%s", output)
	}
	// 5+6+90+84 = 185% compute util on 4 GPUs -> ceiling 1.85 = 2 -> 2/4 (46% saturation)
	if !strings.Contains(output, "2/4") {
		t.Fatalf("missing measured GPU 2/4:\n%s", output)
	}
	if !strings.Contains(output, "46%") {
		t.Fatalf("missing measured GPU 46%%:\n%s", output)
	}
	if strings.Contains(output, "GPU (") {
		t.Fatalf("expected job GPU heading strictly 'GPU', got:\n%s", output)
	}
}

func TestJobBoxesVerticalAlignment(t *testing.T) {
	node := Node{
		Name:              "g001",
		State:             "ALLOCATED",
		GPUType:           "h200",
		GPUTotal:          8,
		GPUAllocated:      8,
		h200Total:         8,
		h200Allocated:     8,
		CPUTotal:          64,
		CPUAllocated:      64,
		MemoryTotalMB:     2048000,
		MemoryAllocatedMB: 1536000,
	}
	jobs := []Job{
		{
			ID:         101,
			User:       "alice",
			Account:    "lab",
			State:      "RUNNING",
			CPUs:       32,
			GPUs:       4,
			MemoryMB:   800000,
			nodes:      "g001",
			gpuIndices: []int{0, 1, 2, 3},
		},
		{
			ID:         102,
			User:       "bob",
			Account:    "weirdlab",
			State:      "RUNNING",
			CPUs:       16,
			GPUs:       2,
			MemoryMB:   400000,
			nodes:      "g001",
			gpuIndices: []int{4, 5},
		},
	}
	telem := &NodeTelemetry{
		NodeName: "g001",
		GPUUtils: map[int]float64{
			0: 90.0, 1: 85.0, 2: 50.0, 3: 10.0,
			4: 100.0, 5: 100.0,
		},
		GPUMemUsedMB: map[int]int{
			0: 30000, 1: 30000, 2: 30000, 3: 30000,
			4: 50000, 5: 50000,
		},
		GPUMemTotalMB: map[int]int{
			0: 141000, 1: 141000, 2: 141000, 3: 141000,
			4: 141000, 5: 141000,
		},
		JobCPUCores: map[int]float64{
			101: 5.2,
			102: 14.1,
		},
		JobMemUsedMB: map[int]int{
			101: 150000,
			102: 100000,
		},
	}
	lines := nodeDetails(node, jobs, 100, 30, 0, false, telem)
	output := strings.Join(lines, "\n")

	// Find ALLOCATED and UTILIZED boxes
	allocIdx := -1
	utilIdx := -1
	aliceIdx := -1
	bobIdx := -1
	for i, line := range lines {
		if strings.Contains(line, "╭─ ALLOCATED") {
			allocIdx = i
		}
		if strings.Contains(line, "╭─ UTILIZED") {
			utilIdx = i
		}
		if strings.Contains(line, "101 • alice") {
			aliceIdx = i
		}
		if strings.Contains(line, "102 • bob") {
			bobIdx = i
		}
	}
	if allocIdx == -1 || utilIdx == -1 || aliceIdx == -1 || bobIdx == -1 {
		t.Fatalf("could not find all boxes in output:\n%s", output)
	}

	allocHeadings := lines[allocIdx+1]
	utilHeadings := lines[utilIdx+1]
	aliceHeadings := lines[aliceIdx+1]
	bobHeadings := lines[bobIdx+1]

	// Verify GPU, CPU, and MEM headings align across ALL groups (ALLOCATED, UTILIZED, Alice, Bob)
	for _, col := range []string{"GPU", "CPU", "MEM"} {
		var allocPos, utilPos, alicePos, bobPos int
		if col == "MEM" {
			allocPos = strings.LastIndex(allocHeadings, col)
			utilPos = strings.LastIndex(utilHeadings, col)
			alicePos = strings.LastIndex(aliceHeadings, col)
			bobPos = strings.LastIndex(bobHeadings, col)
		} else {
			allocPos = strings.Index(allocHeadings, col)
			utilPos = strings.Index(utilHeadings, col)
			alicePos = strings.Index(aliceHeadings, col)
			bobPos = strings.Index(bobHeadings, col)
		}
		if allocPos != utilPos || utilPos != alicePos || alicePos != bobPos {
			t.Errorf("heading %q misaligned: alloc=%d, util=%d, alice=%d, bob=%d", col, allocPos, utilPos, alicePos, bobPos)
		}
	}

	// Verify GPU MEM aligns between UTILIZED and Jobs (ALLOCATED has a blank gap)
	if strings.Contains(allocHeadings, "GPU MEM") {
		t.Errorf("ALLOCATED should have a gap for GPU MEM, but found heading:\n%s", allocHeadings)
	}
	utilGPUMEMPos := strings.Index(utilHeadings, "GPU MEM")
	aliceGPUMEMPos := strings.Index(aliceHeadings, "GPU MEM")
	bobGPUMEMPos := strings.Index(bobHeadings, "GPU MEM")
	if utilGPUMEMPos != aliceGPUMEMPos || aliceGPUMEMPos != bobGPUMEMPos {
		t.Errorf("GPU MEM heading misaligned: util=%d, alice=%d, bob=%d", utilGPUMEMPos, aliceGPUMEMPos, bobGPUMEMPos)
	}

	aliceInfo := lines[aliceIdx+3]
	bobInfo := lines[bobIdx+3]

	// Verify info row fractions align across jobs
	aliceGPUIdx := strings.Index(aliceInfo, "3/4")
	bobGPUIdx := strings.Index(bobInfo, "2/2")
	if aliceGPUIdx != bobGPUIdx {
		t.Errorf("GPU info fraction misaligned: alice=%d (3/4), bob=%d (2/2)", aliceGPUIdx, bobGPUIdx)
	}

	aliceCPUIdx := strings.Index(aliceInfo, " 6/32")
	bobCPUIdx := strings.Index(bobInfo, "15/16")
	if aliceCPUIdx != bobCPUIdx {
		t.Errorf("CPU info column misaligned: alice=%d ( 6/32), bob=%d (15/16)", aliceCPUIdx, bobCPUIdx)
	}

	aliceCPUSlash := strings.Index(aliceInfo, "/32")
	bobCPUSlash := strings.Index(bobInfo, "/16")
	if aliceCPUSlash != bobCPUSlash {
		t.Errorf("CPU slash misaligned: alice=%d (/32), bob=%d (/16)", aliceCPUSlash, bobCPUSlash)
	}
}

func TestJobWithZeroGPUs(t *testing.T) {
	node := Node{
		Name:              "g001",
		State:             "ALLOCATED",
		GPUType:           "h200",
		GPUTotal:          8,
		GPUAllocated:      4,
		h200Total:         8,
		h200Allocated:     4,
		CPUTotal:          64,
		CPUAllocated:      36,
		MemoryTotalMB:     2048000,
		MemoryAllocatedMB: 1536000,
	}

	gpuJob := Job{
		ID:         101,
		User:       "alice",
		Account:    "lab",
		State:      "RUNNING",
		CPUs:       32,
		GPUs:       4,
		MemoryMB:   800000,
		nodes:      "g001",
		gpuIndices: []int{0, 1, 2, 3},
	}

	cpuOnlyJob := Job{
		ID:       102,
		User:     "charlie",
		Account:  "lab",
		State:    "RUNNING",
		CPUs:     4,
		GPUs:     0,
		MemoryMB: 16384,
		nodes:    "g001",
	}

	activeCores := 10.0
	nodeMemUsed := 200000
	charlieCores := 2.0
	charlieMem := 8000
	telem := &NodeTelemetry{
		NodeName: "g001",
		GPUUtils: map[int]float64{
			0: 50.0, 1: 50.0, 2: 50.0, 3: 50.0,
		},
		GPUMemUsedMB: map[int]int{
			0: 20000, 1: 20000, 2: 20000, 3: 20000,
		},
		GPUMemTotalMB: map[int]int{
			0: 141000, 1: 141000, 2: 141000, 3: 141000,
		},
		NodeCPUActiveCores: &activeCores,
		NodeMemUsedMB:      &nodeMemUsed,
		JobCPUCores: map[int]float64{
			101: 8.0,
			102: charlieCores,
		},
		JobMemUsedMB: map[int]int{
			101: 100000,
			102: charlieMem,
		},
	}

	// 1. Without telemetry: 0-GPU job should still show 0% 0/0 and 0% 0/0G (no question marks for GPU or GPU MEM)
	barsNoTelem := nodeJobUtilizationBars(cpuOnlyJob, false)
	outNoTelem := strings.Join(barsNoTelem, "\n")
	if !strings.Contains(outNoTelem, "0/0") {
		t.Errorf("expected '0/0' for 0-GPU job GPU, got:\n%s", outNoTelem)
	}
	if !strings.Contains(outNoTelem, "0/0G") {
		t.Errorf("expected '0/0G' for 0-GPU job GPU MEM, got:\n%s", outNoTelem)
	}
	// Status line for 0-GPU job without telemetry: GPU and GPU MEM should have empty bars (░), not '?'
	headingLineNoTelem := barsNoTelem[1]
	statusLineNoTelem := barsNoTelem[2]
	cpuColIdx := strings.Index(headingLineNoTelem, "CPU")
	if strings.Contains(statusLineNoTelem[:cpuColIdx], "?") {
		t.Errorf("expected no '?' in GPU/GPU MEM status bars for 0-GPU job without telem, got line:\n%s", statusLineNoTelem)
	}

	// 2. With telemetry:
	barsTelem := nodeJobUtilizationBars(cpuOnlyJob, false, telem)
	outTelem := strings.Join(barsTelem, "\n")
	if strings.Contains(outTelem, "?") {
		t.Errorf("expected no '?' anywhere for 0-GPU job with full telemetry, got:\n%s", outTelem)
	}
	if !strings.Contains(outTelem, "0/0G") {
		t.Errorf("expected '0/0G' in:\n%s", outTelem)
	}

	// 3. In nodeDetails with both jobs:
	outputLines := nodeDetails(node, []Job{gpuJob, cpuOnlyJob}, 160, 40, 0, false, telem)
	output := strings.Join(outputLines, "\n")

	charlieIdx := -1
	for i, l := range outputLines {
		if strings.Contains(l, "102 • charlie") {
			charlieIdx = i
			break
		}
	}
	if charlieIdx == -1 {
		t.Fatalf("charlie job box not found in output:\n%s", output)
	}

	charlieHeadings := outputLines[charlieIdx+1]
	charlieStatus := outputLines[charlieIdx+2]
	charlieInfo := outputLines[charlieIdx+3]

	if !strings.Contains(charlieHeadings, "GPU MEM") {
		t.Errorf("expected GPU MEM heading in charlie's box:\n%s", charlieHeadings)
	}
	if strings.Contains(charlieStatus, "?") {
		t.Errorf("expected no '?' in status bar for charlie, got:\n%s", charlieStatus)
	}
	if !strings.Contains(charlieInfo, "0/0G") {
		t.Errorf("expected '0/0G' in charlie's info line, got:\n%s", charlieInfo)
	}
	if !strings.Contains(charlieInfo, "0/0") {
		t.Errorf("expected '0/0' in charlie's info line, got:\n%s", charlieInfo)
	}
	t.Logf("\n%s\n", output)
}

func TestUniformStatusBarLengths(t *testing.T) {
	node := Node{
		Name:               "g001",
		State:              "ALLOCATED",
		GPUType:            "h200",
		GPUTotal:           8,
		GPUAllocated:       6,
		h200Total:          8,
		h200Allocated:      6,
		gpuAllocationTyped: true,
		CPUTotal:           64,
		CPUAllocated:       48,
		MemoryTotalMB:      2048000,
		MemoryAllocatedMB:  1536000,
	}

	job1 := Job{
		ID:         101,
		User:       "alice",
		Account:    "lab",
		State:      "RUNNING",
		CPUs:       32,
		GPUs:       4,
		MemoryMB:   800000,
		nodes:      "g001",
		gpuIndices: []int{0, 1, 2, 3},
	}

	job2 := Job{
		ID:         102,
		User:       "bob",
		Account:    "lab",
		State:      "RUNNING",
		CPUs:       16,
		GPUs:       2,
		MemoryMB:   400000,
		nodes:      "g001",
		gpuIndices: []int{4, 5},
	}

	job3 := Job{
		ID:       103,
		User:     "charlie",
		Account:  "lab",
		State:    "RUNNING",
		CPUs:     4,
		GPUs:     0,
		MemoryMB: 16384,
		nodes:    "g001",
	}

	activeCores := 15.0
	nodeMemUsed := 500000
	telem := &NodeTelemetry{
		NodeName: "g001",
		GPUUtils: map[int]float64{
			0: 90.0, 1: 80.0, 2: 70.0, 3: 60.0, 4: 50.0, 5: 40.0,
		},
		GPUMemUsedMB: map[int]int{
			0: 40000, 1: 40000, 2: 40000, 3: 40000, 4: 20000, 5: 20000,
		},
		GPUMemTotalMB: map[int]int{
			0: 141000, 1: 141000, 2: 141000, 3: 141000, 4: 141000, 5: 141000,
		},
		NodeCPUActiveCores: &activeCores,
		NodeMemUsedMB:      &nodeMemUsed,
		JobCPUCores: map[int]float64{
			101: 8.0, 102: 6.0, 103: 1.5,
		},
		JobMemUsedMB: map[int]int{
			101: 200000, 102: 100000, 103: 8000,
		},
	}

	outputLines := nodeDetails(node, []Job{job1, job2, job3}, 180, 50, 0, false, telem)
	output := strings.Join(outputLines, "\n")

	allocIdx, utilIdx, aliceIdx, bobIdx, charlieIdx := -1, -1, -1, -1, -1
	for i, l := range outputLines {
		switch {
		case strings.Contains(l, "ALLOCATED"):
			allocIdx = i
		case strings.Contains(l, "UTILIZED"):
			utilIdx = i
		case strings.Contains(l, "101 • alice"):
			aliceIdx = i
		case strings.Contains(l, "102 • bob"):
			bobIdx = i
		case strings.Contains(l, "103 • charlie"):
			charlieIdx = i
		}
	}
	if allocIdx == -1 || utilIdx == -1 || aliceIdx == -1 || bobIdx == -1 || charlieIdx == -1 {
		t.Fatalf("could not find all boxes in output:\n%s", output)
	}

	extractBar := func(statusLine string, start, end int) string {
		runes := []rune(statusLine)
		if start < 0 || end > len(runes) || start >= end {
			return ""
		}
		var bar strings.Builder
		for _, r := range runes[start:end] {
			if r == '█' || r == '░' {
				bar.WriteRune(r)
			}
		}
		return bar.String()
	}

	colWidths := nodeGroupColumnWidths(node, []Job{job1, job2, job3}, telem, false)
	c0Start := 1
	c0End := c0Start + colWidths[0]
	c1Start := c0End + 2
	c1End := c1Start + colWidths[1]
	c2Start := c1End + 2
	c2End := c2Start + colWidths[2]
	c3Start := c2End + 2
	c3End := c3Start + colWidths[3]

	groups := []struct {
		name string
		idx  int
	}{
		{"UTILIZED", utilIdx},
		{"alice", aliceIdx},
		{"bob", bobIdx},
		{"charlie", charlieIdx},
	}

	// Check GPU bar length uniformity across all groups (including ALLOCATED)
	allocGpuBar := extractBar(outputLines[allocIdx+2], c0Start, c0End)
	if len([]rune(allocGpuBar)) == 0 {
		t.Fatalf("expected non-empty ALLOCATED GPU bar, got %q", outputLines[allocIdx+2])
	}
	for _, g := range groups {
		bar := extractBar(outputLines[g.idx+2], c0Start, c0End)
		if len([]rune(bar)) != len([]rune(allocGpuBar)) {
			t.Errorf("GPU bar length mismatch in %s: len=%d (%q), want %d (%q)",
				g.name, len([]rune(bar)), bar, len([]rune(allocGpuBar)), allocGpuBar)
		}
	}

	// Check GPU MEM bar length uniformity across groups with GPU MEM
	refGpuMemBar := extractBar(outputLines[utilIdx+2], c1Start, c1End)
	if len([]rune(refGpuMemBar)) == 0 {
		t.Fatalf("expected non-empty UTILIZED GPU MEM bar, got %q", outputLines[utilIdx+2])
	}
	for _, g := range groups[1:] {
		bar := extractBar(outputLines[g.idx+2], c1Start, c1End)
		if len([]rune(bar)) != len([]rune(refGpuMemBar)) {
			t.Errorf("GPU MEM bar length mismatch in %s: len=%d (%q), want %d (%q)",
				g.name, len([]rune(bar)), bar, len([]rune(refGpuMemBar)), refGpuMemBar)
		}
	}

	// Check CPU bar length uniformity across ALL groups (including ALLOCATED)
	allocCpuBar := extractBar(outputLines[allocIdx+2], c2Start, c2End)
	if len([]rune(allocCpuBar)) == 0 {
		t.Fatalf("expected non-empty ALLOCATED CPU bar, got %q", outputLines[allocIdx+2])
	}
	for _, g := range groups {
		bar := extractBar(outputLines[g.idx+2], c2Start, c2End)
		if len([]rune(bar)) != len([]rune(allocCpuBar)) {
			t.Errorf("CPU bar length mismatch in %s: len=%d (%q), want %d (%q)",
				g.name, len([]rune(bar)), bar, len([]rune(allocCpuBar)), allocCpuBar)
		}
	}

	// Check MEM bar length uniformity across ALL groups (including ALLOCATED)
	allocMemBar := extractBar(outputLines[allocIdx+2], c3Start, c3End)
	if len([]rune(allocMemBar)) == 0 {
		t.Fatalf("expected non-empty ALLOCATED MEM bar, got %q", outputLines[allocIdx+2])
	}
	for _, g := range groups {
		bar := extractBar(outputLines[g.idx+2], c3Start, c3End)
		if len([]rune(bar)) != len([]rune(allocMemBar)) {
			t.Errorf("MEM bar length mismatch in %s: len=%d (%q), want %d (%q)",
				g.name, len([]rune(bar)), bar, len([]rune(allocMemBar)), allocMemBar)
		}
	}
	t.Logf("\n%s\n", output)
}
