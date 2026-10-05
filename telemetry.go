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

type JobEfficiency struct {
	JobID      int
	Score      float64
	GPUUtil    float64
	GPUMemUtil float64
	CPUUtil    float64
	MemUtil    float64
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2.0
}

type promSeriesRange struct {
	Metric map[string]string
	Times  []float64
	Values []float64
}

type promRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
			Values [][]any           `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func parsePromFloat(v any) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case string:
		f, err := strconv.ParseFloat(val, 64)
		return f, err == nil
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	}
	return 0, false
}

func parsePrometheusRangeResponse(data []byte) ([]promSeriesRange, error) {
	var resp promRangeResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("prometheus query status: %s", resp.Status)
	}
	series := make([]promSeriesRange, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		sr := promSeriesRange{
			Metric: r.Metric,
		}
		if len(r.Values) > 0 {
			for _, pt := range r.Values {
				if len(pt) < 2 {
					continue
				}
				ts, ok1 := parsePromFloat(pt[0])
				v, ok2 := parsePromFloat(pt[1])
				if ok1 && ok2 {
					sr.Times = append(sr.Times, ts)
					sr.Values = append(sr.Values, v)
				}
			}
		} else if len(r.Value) >= 2 {
			ts, ok1 := parsePromFloat(r.Value[0])
			v, ok2 := parsePromFloat(r.Value[1])
			if ok1 && ok2 {
				sr.Times = append(sr.Times, ts)
				sr.Values = append(sr.Values, v)
			}
		}
		series = append(series, sr)
	}
	return series, nil
}

func (cfg PrometheusConfig) fallbackInstantQuery(ctx context.Context, client *http.Client, expr string) ([]promSeriesRange, error) {
	series, err := cfg.queryExpr(ctx, client, expr)
	if err != nil {
		return nil, err
	}
	rangeSeries := make([]promSeriesRange, len(series))
	nowSec := float64(time.Now().Unix())
	for i, s := range series {
		rangeSeries[i] = promSeriesRange{
			Metric: s.Metric,
			Times:  []float64{nowSec},
			Values: []float64{s.Value},
		}
	}
	return rangeSeries, nil
}

func (cfg PrometheusConfig) queryRangeExpr(ctx context.Context, client *http.Client, expr string, start, end time.Time, step time.Duration) ([]promSeriesRange, error) {
	if cfg.Disabled || cfg.URL == "" {
		return nil, nil
	}
	stepSec := int(step.Seconds())
	if stepSec <= 0 {
		stepSec = 30
	}
	reqURL := fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%ds",
		strings.TrimRight(cfg.URL, "/"),
		url.QueryEscape(expr),
		start.Unix(),
		end.Unix(),
		stepSec,
	)
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	if cfg.Username != "" || cfg.Password != "" {
		req.SetBasicAuth(cfg.Username, cfg.Password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return cfg.fallbackInstantQuery(ctx, client, expr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cfg.fallbackInstantQuery(ctx, client, expr)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parsePrometheusRangeResponse(data)
}

func promHost(m map[string]string) string {
	if h, ok := m["Hostname"]; ok && h != "" {
		host, _, _ := strings.Cut(h, ".")
		return host
	}
	if h, ok := m["hostname"]; ok && h != "" {
		host, _, _ := strings.Cut(h, ".")
		return host
	}
	if h, ok := m["node"]; ok && h != "" {
		host, _, _ := strings.Cut(h, ".")
		return host
	}
	if inst, ok := m["instance"]; ok && inst != "" {
		host, _, _ := strings.Cut(inst, ":")
		host, _, _ = strings.Cut(host, ".")
		return host
	}
	return ""
}

func promJobID(m map[string]string) (int, bool) {
	for _, key := range []string{"jobid", "slurmjobid", "job"} {
		if val, ok := m[key]; ok && val != "" {
			if id, err := strconv.Atoi(val); err == nil {
				return id, true
			}
		}
	}
	return 0, false
}

func fetchClusterJobEfficiencies(ctx context.Context, cfg PrometheusConfig, jobs []Job, nodes []Node) (map[int]JobEfficiency, error) {
	result := make(map[int]JobEfficiency)
	if cfg.Disabled || cfg.URL == "" || len(jobs) == 0 {
		for _, j := range jobs {
			if j.State == "RUNNING" {
				result[j.ID] = JobEfficiency{JobID: j.ID, Score: 0}
			}
		}
		return result, nil
	}

	running := make([]Job, 0, len(jobs))
	for _, j := range jobs {
		if j.State == "RUNNING" {
			running = append(running, j)
			result[j.ID] = JobEfficiency{JobID: j.ID, Score: 0}
		}
	}
	if len(running) == 0 {
		return result, nil
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: timeout,
	}

	now := time.Now()
	earliest := now
	hasGPUJob := false
	for _, j := range running {
		st := j.StartTime()
		if !st.IsZero() && st.Before(earliest) {
			earliest = st
		}
		if j.GPUs > 0 {
			hasGPUJob = true
		}
	}
	if earliest.After(now.Add(-time.Minute)) {
		earliest = now.Add(-5 * time.Minute)
	}

	step := 30 * time.Second
	dur := now.Sub(earliest)
	if dur > 12*time.Hour {
		step = 2 * time.Minute
	} else if dur > 2*time.Hour {
		step = time.Minute
	}

	type samplePoint struct {
		time float64
		val  float64
	}

	var mu sync.Mutex
	cpuByJob := make(map[int][]samplePoint)
	memUsedByJob := make(map[int][]samplePoint)
	memRSSByJob := make(map[int][]samplePoint)
	gpuUtilByNodeGPU := make(map[string][]samplePoint)
	gpuFBUsedByNodeGPU := make(map[string][]samplePoint)
	gpuFBFreeByNodeGPU := make(map[string][]samplePoint)

	var wg sync.WaitGroup

	queries := []struct {
		expr    string
		handler func([]promSeriesRange)
	}{
		{
			expr: "rate(cgroup_cpu_total_seconds[2m])",
			handler: func(series []promSeriesRange) {
				mu.Lock()
				defer mu.Unlock()
				for _, s := range series {
					if jID, ok := promJobID(s.Metric); ok {
						for k := 0; k < len(s.Values); k++ {
							cpuByJob[jID] = append(cpuByJob[jID], samplePoint{time: s.Times[k], val: s.Values[k]})
						}
					}
				}
			},
		},
		{
			expr: "cgroup_memory_used_bytes",
			handler: func(series []promSeriesRange) {
				mu.Lock()
				defer mu.Unlock()
				for _, s := range series {
					if jID, ok := promJobID(s.Metric); ok {
						for k := 0; k < len(s.Values); k++ {
							memUsedByJob[jID] = append(memUsedByJob[jID], samplePoint{time: s.Times[k], val: s.Values[k]})
						}
					}
				}
			},
		},
		{
			expr: "cgroup_memory_rss_bytes",
			handler: func(series []promSeriesRange) {
				mu.Lock()
				defer mu.Unlock()
				for _, s := range series {
					if jID, ok := promJobID(s.Metric); ok {
						for k := 0; k < len(s.Values); k++ {
							memRSSByJob[jID] = append(memRSSByJob[jID], samplePoint{time: s.Times[k], val: s.Values[k]})
						}
					}
				}
			},
		},
	}

	if hasGPUJob {
		queries = append(queries,
			struct {
				expr    string
				handler func([]promSeriesRange)
			}{
				expr: "DCGM_FI_DEV_GPU_UTIL",
				handler: func(series []promSeriesRange) {
					mu.Lock()
					defer mu.Unlock()
					for _, s := range series {
						host := promHost(s.Metric)
						gpu := s.Metric["gpu"]
						if host != "" && gpu != "" {
							key := host + "/" + gpu
							for k := 0; k < len(s.Values); k++ {
								gpuUtilByNodeGPU[key] = append(gpuUtilByNodeGPU[key], samplePoint{time: s.Times[k], val: s.Values[k]})
							}
						}
					}
				},
			},
			struct {
				expr    string
				handler func([]promSeriesRange)
			}{
				expr: "DCGM_FI_DEV_FB_USED",
				handler: func(series []promSeriesRange) {
					mu.Lock()
					defer mu.Unlock()
					for _, s := range series {
						host := promHost(s.Metric)
						gpu := s.Metric["gpu"]
						if host != "" && gpu != "" {
							key := host + "/" + gpu
							for k := 0; k < len(s.Values); k++ {
								gpuFBUsedByNodeGPU[key] = append(gpuFBUsedByNodeGPU[key], samplePoint{time: s.Times[k], val: s.Values[k]})
							}
						}
					}
				},
			},
			struct {
				expr    string
				handler func([]promSeriesRange)
			}{
				expr: "DCGM_FI_DEV_FB_FREE",
				handler: func(series []promSeriesRange) {
					mu.Lock()
					defer mu.Unlock()
					for _, s := range series {
						host := promHost(s.Metric)
						gpu := s.Metric["gpu"]
						if host != "" && gpu != "" {
							key := host + "/" + gpu
							for k := 0; k < len(s.Values); k++ {
								gpuFBFreeByNodeGPU[key] = append(gpuFBFreeByNodeGPU[key], samplePoint{time: s.Times[k], val: s.Values[k]})
							}
						}
					}
				},
			},
		)
	}

	for _, q := range queries {
		wg.Add(1)
		go func(expr string, h func([]promSeriesRange)) {
			defer wg.Done()
			series, err := cfg.queryRangeExpr(ctx, client, expr, earliest, now, step)
			if err == nil && len(series) > 0 {
				h(series)
			}
		}(q.expr, q.handler)
	}

	wg.Wait()

	for _, j := range running {
		jobStartSec := float64(j.StartTime().Unix())

		// CPU
		cpuUtil := 0.0
		if j.CPUs > 0 {
			var vals []float64
			for _, pt := range cpuByJob[j.ID] {
				if pt.time >= jobStartSec {
					vals = append(vals, pt.val)
				}
			}
			if len(vals) == 0 && len(cpuByJob[j.ID]) > 0 {
				for _, pt := range cpuByJob[j.ID] {
					vals = append(vals, pt.val)
				}
			}
			if len(vals) > 0 {
				med := median(vals)
				cpuUtil = med / float64(j.CPUs)
				if cpuUtil > 1.0 {
					cpuUtil = 1.0
				}
				if cpuUtil < 0.0 {
					cpuUtil = 0.0
				}
			}
		}

		// MEM
		memUtil := 0.0
		if j.MemoryMB > 0 {
			var vals []float64
			pts := memUsedByJob[j.ID]
			if len(pts) == 0 {
				pts = memRSSByJob[j.ID]
			}
			for _, pt := range pts {
				if pt.time >= jobStartSec {
					vals = append(vals, pt.val/(1024*1024))
				}
			}
			if len(vals) == 0 && len(pts) > 0 {
				for _, pt := range pts {
					vals = append(vals, pt.val/(1024*1024))
				}
			}
			if len(vals) > 0 {
				medMB := median(vals)
				memUtil = medMB / float64(j.MemoryMB)
				if memUtil > 1.0 {
					memUtil = 1.0
				}
				if memUtil < 0.0 {
					memUtil = 0.0
				}
			}
		}

		// GPU & GPU MEM
		gpuUtil := 0.0
		gpuMemUtil := 0.0
		if j.GPUs > 0 {
			indices := j.gpuIndices
			if len(indices) == 0 {
				for i := 0; i < j.GPUs; i++ {
					indices = append(indices, i)
				}
			}
			jobNodes := make([]string, 0, 1)
			for _, n := range nodes {
				if jobRunsOnNode(j.nodes, n.Name) {
					jobNodes = append(jobNodes, n.Name)
				}
			}
			if len(jobNodes) == 0 && j.nodes != "" && j.nodes != "(none)" {
				jobNodes = append(jobNodes, j.nodes)
			}

			sumMedGPU := 0.0
			sumMedGPUMemUsed := 0.0
			totalGPUMemMB := 0.0

			for _, nodeName := range jobNodes {
				for _, g := range indices {
					key := fmt.Sprintf("%s/%d", nodeName, g)
					// GPU Util
					var gVals []float64
					for _, pt := range gpuUtilByNodeGPU[key] {
						if pt.time >= jobStartSec {
							gVals = append(gVals, pt.val)
						}
					}
					if len(gVals) == 0 && len(gpuUtilByNodeGPU[key]) > 0 {
						for _, pt := range gpuUtilByNodeGPU[key] {
							gVals = append(gVals, pt.val)
						}
					}
					if len(gVals) > 0 {
						sumMedGPU += median(gVals)
					}

					// GPU FB Used
					var mVals []float64
					for _, pt := range gpuFBUsedByNodeGPU[key] {
						if pt.time >= jobStartSec {
							mVals = append(mVals, pt.val)
						}
					}
					if len(mVals) == 0 && len(gpuFBUsedByNodeGPU[key]) > 0 {
						for _, pt := range gpuFBUsedByNodeGPU[key] {
							mVals = append(mVals, pt.val)
						}
					}
					if len(mVals) > 0 {
						sumMedGPUMemUsed += median(mVals)
					}

					// Total GPU FB memory
					totMem := 141.0 * 1024.0 // default H200
					freePts := gpuFBFreeByNodeGPU[key]
					usedPts := gpuFBUsedByNodeGPU[key]
					if len(freePts) > 0 && len(usedPts) > 0 {
						tot := freePts[len(freePts)-1].val + usedPts[len(usedPts)-1].val
						if tot > 0 {
							totMem = tot
						}
					}
					totalGPUMemMB += totMem
				}
			}

			if j.GPUs > 0 {
				gpuUtil = sumMedGPU / (float64(j.GPUs) * 100.0)
				if gpuUtil > 1.0 {
					gpuUtil = 1.0
				}
				if gpuUtil < 0.0 {
					gpuUtil = 0.0
				}
			}
			if totalGPUMemMB > 0 {
				gpuMemUtil = sumMedGPUMemUsed / totalGPUMemMB
				if gpuMemUtil > 1.0 {
					gpuMemUtil = 1.0
				}
				if gpuMemUtil < 0.0 {
					gpuMemUtil = 0.0
				}
			}
		}

		var totalScore float64
		var totalWeights float64

		if j.GPUs > 0 {
			totalScore += gpuUtil + gpuMemUtil
			totalWeights += 2.0
		}
		if j.CPUs > 0 {
			totalScore += cpuUtil
			totalWeights += 1.0
		}
		if j.MemoryMB > 0 {
			totalScore += memUtil
			totalWeights += 1.0
		}

		score := 0.0
		if totalWeights > 0 {
			score = totalScore / totalWeights
		}
		if score > 1.0 {
			score = 1.0
		}
		if score < 0.0 {
			score = 0.0
		}

		result[j.ID] = JobEfficiency{
			JobID:      j.ID,
			Score:      score,
			GPUUtil:    gpuUtil,
			GPUMemUtil: gpuMemUtil,
			CPUUtil:    cpuUtil,
			MemUtil:    memUtil,
		}
	}

	return result, nil
}
