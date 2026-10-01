package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type PrometheusConfig struct {
	URL      string
	Username string
	Password string
	Timeout  time.Duration
	Disabled bool
}

type NodeTelemetry struct {
	NodeName           string
	GPUUtils           map[int]float64 // DCGM_FI_DEV_GPU_UTIL (%)
	GPUMemUsedMB       map[int]int     // DCGM_FI_DEV_FB_USED (MB)
	GPUMemFreeMB       map[int]int     // DCGM_FI_DEV_FB_FREE (MB)
	GPUMemTotalMB      map[int]int     // FB_USED + FB_FREE + FB_RESERVED (MB)
	GPUPowerWatts      map[int]float64 // DCGM_FI_DEV_POWER_USAGE (Watts)
	GPUTempC           map[int]int     // DCGM_FI_DEV_GPU_TEMP (C)
	NodeCPUActiveCores *float64        // active cores from node_exporter or cgroup sum
	NodeMemUsedMB      *int            // used memory in MB
	NodeMemTotalMB     *int            // total memory in MB
	JobCPUCores        map[int]float64 // rate(cgroup_cpu_total_seconds[2m]) per job ID
	JobMemRSSMB        map[int]int     // cgroup_memory_rss_bytes in MB per job ID
	JobMemUsedMB       map[int]int     // cgroup_memory_used_bytes in MB per job ID
}

func (t *NodeTelemetry) TotalGPUMemUsedMB() int {
	if t == nil {
		return 0
	}
	sum := 0
	for _, m := range t.GPUMemUsedMB {
		sum += m
	}
	return sum
}

func (t *NodeTelemetry) TotalGPUMemMB() int {
	if t == nil {
		return 0
	}
	sum := 0
	for _, m := range t.GPUMemTotalMB {
		sum += m
	}
	return sum
}

func (t *NodeTelemetry) ActiveGPUCount() int {
	if t == nil {
		return 0
	}
	count := 0
	seen := make(map[int]bool)
	for gpu, u := range t.GPUUtils {
		if u > 0 || t.GPUMemUsedMB[gpu] > 100 {
			if !seen[gpu] {
				seen[gpu] = true
				count++
			}
		}
	}
	return count
}

func (t *NodeTelemetry) ActiveCPUCores() float64 {
	if t == nil || t.NodeCPUActiveCores == nil {
		return 0
	}
	return *t.NodeCPUActiveCores
}

var gresIdxRE = regexp.MustCompile(`IDX:([0-9,\-]+)`)

func parseGresIndices(gresDetail []string) []int {
	var indices []int
	seen := make(map[int]bool)
	for _, item := range gresDetail {
		m := gresIdxRE.FindStringSubmatch(item)
		if len(m) < 2 {
			continue
		}
		for _, span := range strings.Split(m[1], ",") {
			first, last, ranged := strings.Cut(span, "-")
			start, err1 := strconv.Atoi(first)
			if ranged {
				end, err2 := strconv.Atoi(last)
				if err1 == nil && err2 == nil && end >= start {
					for n := start; n <= end; n++ {
						if !seen[n] {
							seen[n] = true
							indices = append(indices, n)
						}
					}
				}
			} else if err1 == nil {
				if !seen[start] {
					seen[start] = true
					indices = append(indices, start)
				}
			}
		}
	}
	sort.Ints(indices)
	return indices
}

type promSeries struct {
	Metric map[string]string
	Value  float64
}

type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func parsePrometheusResponse(data []byte) ([]promSeries, error) {
	var resp promResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("prometheus query status: %s", resp.Status)
	}
	series := make([]promSeries, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		if len(r.Value) < 2 {
			continue
		}
		var val float64
		switch v := r.Value[1].(type) {
		case string:
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			val = f
		case float64:
			val = v
		default:
			continue
		}
		series = append(series, promSeries{
			Metric: r.Metric,
			Value:  val,
		})
	}
	return series, nil
}

func (cfg PrometheusConfig) queryExpr(ctx context.Context, client *http.Client, expr string) ([]promSeries, error) {
	if cfg.Disabled || cfg.URL == "" {
		return nil, nil
	}
	reqURL := fmt.Sprintf("%s/api/v1/query?query=%s", strings.TrimRight(cfg.URL, "/"), url.QueryEscape(expr))
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	if cfg.Username != "" || cfg.Password != "" {
		req.SetBasicAuth(cfg.Username, cfg.Password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus HTTP status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parsePrometheusResponse(data)
}

func fetchNodeTelemetry(ctx context.Context, cfg PrometheusConfig, nodeName string) (*NodeTelemetry, error) {
	if cfg.Disabled || cfg.URL == "" || nodeName == "" {
		return nil, nil
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: timeout,
	}

	telem := &NodeTelemetry{
		NodeName:      nodeName,
		GPUUtils:      make(map[int]float64),
		GPUMemUsedMB:  make(map[int]int),
		GPUMemFreeMB:  make(map[int]int),
		GPUMemTotalMB: make(map[int]int),
		GPUPowerWatts: make(map[int]float64),
		GPUTempC:      make(map[int]int),
		JobCPUCores:   make(map[int]float64),
		JobMemRSSMB:   make(map[int]int),
		JobMemUsedMB:  make(map[int]int),
	}

	var mu sync.Mutex
	var wg sync.WaitGroup

	queries := []struct {
		expr    string
		handler func([]promSeries)
	}{
		{
			expr: fmt.Sprintf("DCGM_FI_DEV_GPU_UTIL{Hostname=%q}", nodeName),
			handler: func(series []promSeries) {
				for _, s := range series {
					if gpuStr, ok := s.Metric["gpu"]; ok {
						if g, err := strconv.Atoi(gpuStr); err == nil {
							mu.Lock()
							telem.GPUUtils[g] = s.Value
							mu.Unlock()
						}
					}
				}
			},
		},
		{
			expr: fmt.Sprintf("DCGM_FI_DEV_FB_USED{Hostname=%q}", nodeName),
			handler: func(series []promSeries) {
				for _, s := range series {
					if gpuStr, ok := s.Metric["gpu"]; ok {
						if g, err := strconv.Atoi(gpuStr); err == nil {
							mu.Lock()
							telem.GPUMemUsedMB[g] = int(s.Value)
							telem.GPUMemTotalMB[g] = telem.GPUMemUsedMB[g] + telem.GPUMemFreeMB[g]
							mu.Unlock()
						}
					}
				}
			},
		},
		{
			expr: fmt.Sprintf("DCGM_FI_DEV_FB_FREE{Hostname=%q}", nodeName),
			handler: func(series []promSeries) {
				for _, s := range series {
					if gpuStr, ok := s.Metric["gpu"]; ok {
						if g, err := strconv.Atoi(gpuStr); err == nil {
							mu.Lock()
							telem.GPUMemFreeMB[g] = int(s.Value)
							telem.GPUMemTotalMB[g] = telem.GPUMemUsedMB[g] + telem.GPUMemFreeMB[g]
							mu.Unlock()
						}
					}
				}
			},
		},
		{
			expr: fmt.Sprintf("DCGM_FI_DEV_POWER_USAGE{Hostname=%q}", nodeName),
			handler: func(series []promSeries) {
				for _, s := range series {
					if gpuStr, ok := s.Metric["gpu"]; ok {
						if g, err := strconv.Atoi(gpuStr); err == nil {
							mu.Lock()
							telem.GPUPowerWatts[g] = s.Value
							mu.Unlock()
						}
					}
				}
			},
		},
		{
			expr: fmt.Sprintf("DCGM_FI_DEV_GPU_TEMP{Hostname=%q}", nodeName),
			handler: func(series []promSeries) {
				for _, s := range series {
					if gpuStr, ok := s.Metric["gpu"]; ok {
						if g, err := strconv.Atoi(gpuStr); err == nil {
							mu.Lock()
							telem.GPUTempC[g] = int(s.Value)
							mu.Unlock()
						}
					}
				}
			},
		},
		{
			expr: fmt.Sprintf("rate(cgroup_cpu_total_seconds{instance=~%q, step=\"\"}[2m])", nodeName+".*"),
			handler: func(series []promSeries) {
				cgroupSum := 0.0
				for _, s := range series {
					if jStr, ok := s.Metric["jobid"]; ok {
						if jID, err := strconv.Atoi(jStr); err == nil {
							mu.Lock()
							telem.JobCPUCores[jID] = s.Value
							cgroupSum += s.Value
							mu.Unlock()
						}
					}
				}
				mu.Lock()
				if telem.NodeCPUActiveCores == nil && cgroupSum > 0 {
					telem.NodeCPUActiveCores = &cgroupSum
				}
				mu.Unlock()
			},
		},
		{
			expr: fmt.Sprintf("cgroup_memory_rss_bytes{instance=~%q, step=\"\"}", nodeName+".*"),
			handler: func(series []promSeries) {
				for _, s := range series {
					if jStr, ok := s.Metric["jobid"]; ok {
						if jID, err := strconv.Atoi(jStr); err == nil {
							mb := int(s.Value / 1024 / 1024)
							mu.Lock()
							telem.JobMemRSSMB[jID] = mb
							mu.Unlock()
						}
					}
				}
			},
		},
		{
			expr: fmt.Sprintf("cgroup_memory_used_bytes{instance=~%q, step=\"\"}", nodeName+".*"),
			handler: func(series []promSeries) {
				cgroupMemSum := 0
				for _, s := range series {
					if jStr, ok := s.Metric["jobid"]; ok {
						if jID, err := strconv.Atoi(jStr); err == nil {
							mb := int(s.Value / 1024 / 1024)
							mu.Lock()
							telem.JobMemUsedMB[jID] = mb
							cgroupMemSum += mb
							mu.Unlock()
						}
					}
				}
				mu.Lock()
				if telem.NodeMemUsedMB == nil && cgroupMemSum > 0 {
					telem.NodeMemUsedMB = &cgroupMemSum
				}
				mu.Unlock()
			},
		},
		{
			expr: fmt.Sprintf("sum(rate(node_cpu_seconds_total{instance=~%q, mode!=\"idle\"}[2m]))", nodeName+".*"),
			handler: func(series []promSeries) {
				if len(series) > 0 {
					val := series[0].Value
					mu.Lock()
					telem.NodeCPUActiveCores = &val
					mu.Unlock()
				}
			},
		},
		{
			expr: fmt.Sprintf("(node_memory_MemTotal_bytes{instance=~%q} - node_memory_MemAvailable_bytes{instance=~%q}) / 1024 / 1024", nodeName+".*", nodeName+".*"),
			handler: func(series []promSeries) {
				if len(series) > 0 {
					mb := int(series[0].Value)
					mu.Lock()
					telem.NodeMemUsedMB = &mb
					mu.Unlock()
				}
			},
		},
	}

	for _, q := range queries {
		wg.Add(1)
		go func(expr string, h func([]promSeries)) {
			defer wg.Done()
			series, err := cfg.queryExpr(ctx, client, expr)
			if err == nil && len(series) > 0 {
				h(series)
			}
		}(q.expr, q.handler)
	}

	wg.Wait()
	return telem, nil
}
