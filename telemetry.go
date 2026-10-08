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

// parseGresIndicesByNode pairs each node of a job's hostlist with its GPU
// indices. Slurm reports gres_detail as one entry per allocated node, in
// hostlist order, and the same index can mean different GPUs on different
// nodes. It returns nil when the pairing cannot be determined.
func parseGresIndicesByNode(nodes string, gresDetail []string) map[string][]int {
	names := expandNodeList(nodes)
	if len(names) == 0 {
		return nil
	}
	if len(names) == 1 {
		if idx := parseGresIndices(gresDetail); len(idx) > 0 {
			return map[string][]int{names[0]: idx}
		}
		return nil
	}
	if len(names) != len(gresDetail) {
		return nil
	}
	byNode := make(map[string][]int, len(names))
	for i, name := range names {
		if idx := parseGresIndices(gresDetail[i : i+1]); len(idx) > 0 {
			byNode[name] = idx
		}
	}
	if len(byNode) == 0 {
		return nil
	}
	return byNode
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

const (
	// efficiencyTimeout bounds one full efficiency pass. It is separate from
	// PrometheusConfig.Timeout, which is sized for single node-detail lookups.
	efficiencyTimeout = 30 * time.Second
	// efficiencyMaxPoints caps the subquery samples taken over a job lifetime.
	efficiencyMaxPoints = 1500
	efficiencyMinStep   = 30 * time.Second
	// efficiencyJobBatch caps the job IDs placed in one jobid regex.
	efficiencyJobBatch = 200
	// efficiencyParallel caps concurrent per-job DCGM queries.
	efficiencyParallel = 8
)

// queryExprAt evaluates an instant query at a fixed time. It uses a POST form
// so that long jobid regexes do not exceed URL length limits.
func (cfg PrometheusConfig) queryExprAt(ctx context.Context, client *http.Client, expr string, at time.Time) ([]promSeries, error) {
	form := url.Values{"query": {expr}, "time": {strconv.FormatInt(at.Unix(), 10)}}
	reqURL := strings.TrimRight(cfg.URL, "/") + "/api/v1/query"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

// medianWindow returns a PromQL subquery range covering the last window,
// sampled at most efficiencyMaxPoints times.
func medianWindow(window time.Duration) string {
	step := efficiencyMinStep
	if perPoint := window / efficiencyMaxPoints; perPoint > step {
		step = perPoint.Truncate(time.Second) + time.Second
	}
	if window < step {
		window = step
	}
	return fmt.Sprintf("[%ds:%ds]", int64(window.Seconds()+0.5), int64(step.Seconds()))
}

// cgroupMedianExpr builds the lifetime-median query for a job-level cgroup
// metric. Only the job-level series (no step label) are used, because the
// step and task series are subsets of it. Series from multiple nodes are
// summed at each instant before taking the median. A job's cgroup series
// only exist while it runs, so one window reaching back to the oldest job
// still yields each job's own lifetime median.
func cgroupMedianExpr(inner string, ids []int, window time.Duration) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	sel := fmt.Sprintf(`{jobid=~"%s",step=""}`, strings.Join(parts, "|"))
	return fmt.Sprintf("quantile_over_time(0.5, (sum by (jobid) (%s))%s)", strings.ReplaceAll(inner, "{}", sel), medianWindow(window))
}

// dcgmSelector selects exactly the GPUs allocated to a job on each node.
func dcgmSelector(metric string, gpusByNode map[string][]int) string {
	nodes := make([]string, 0, len(gpusByNode))
	for n := range gpusByNode {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	terms := make([]string, 0, len(nodes))
	for _, n := range nodes {
		idx := make([]string, len(gpusByNode[n]))
		for i, g := range gpusByNode[n] {
			idx[i] = strconv.Itoa(g)
		}
		terms = append(terms, fmt.Sprintf(`%s{Hostname=%q,gpu=~"%s"}`, metric, n, strings.Join(idx, "|")))
	}
	return strings.Join(terms, " or ")
}

// gpuMedianExprs builds the lifetime-median queries for a job's GPUs. GPU
// series outlive jobs, so the window must be the job's own elapsed time.
// The utilization query returns summed percent across the job's GPUs; the
// memory query returns the used fraction of the job's GPU framebuffer.
func gpuMedianExprs(gpusByNode map[string][]int, window time.Duration) (util, mem string) {
	w := medianWindow(window)
	used := dcgmSelector("DCGM_FI_DEV_FB_USED", gpusByNode)
	free := dcgmSelector("DCGM_FI_DEV_FB_FREE", gpusByNode)
	util = fmt.Sprintf("quantile_over_time(0.5, (sum(%s))%s)", dcgmSelector("DCGM_FI_DEV_GPU_UTIL", gpusByNode), w)
	mem = fmt.Sprintf("quantile_over_time(0.5, (sum(%s) / (sum(%s) + sum(%s)))%s)", used, used, free, w)
	return util, mem
}

func clampUnit(v float64) float64 {
	return min(max(v, 0), 1)
}

func fetchClusterJobEfficiencies(ctx context.Context, cfg PrometheusConfig, jobs []Job) (map[int]JobEfficiency, error) {
	result := make(map[int]JobEfficiency)
	running := make([]Job, 0, len(jobs))
	for _, j := range jobs {
		if j.State == "RUNNING" {
			running = append(running, j)
			result[j.ID] = JobEfficiency{JobID: j.ID}
		}
	}
	if cfg.Disabled || cfg.URL == "" || len(running) == 0 {
		return result, nil
	}

	ctx, cancel := context.WithTimeout(ctx, efficiencyTimeout)
	defer cancel()
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	now := time.Now()
	age := func(j Job) time.Duration {
		return max(now.Sub(j.StartTime()), 0)
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		sem      = make(chan struct{}, efficiencyParallel)
	)
	cpuCores := make(map[int]float64)
	memUsed := make(map[int]float64)
	memRSS := make(map[int]float64)
	gpuUtilSum := make(map[int]float64)
	gpuMemFrac := make(map[int]float64)

	run := func(expr string, handle func([]promSeries)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			series, err := cfg.queryExprAt(ctx, client, expr, now)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("prometheus query %q: %w", expr, err)
				}
				return
			}
			handle(series)
		}()
	}
	byJob := func(dst map[int]float64) func([]promSeries) {
		return func(series []promSeries) {
			for _, s := range series {
				if id, ok := promJobID(s.Metric); ok {
					dst[id] = s.Value
				}
			}
		}
	}
	single := func(dst map[int]float64, id int) func([]promSeries) {
		return func(series []promSeries) {
			if len(series) > 0 {
				dst[id] = series[0].Value
			}
		}
	}

	for start := 0; start < len(running); start += efficiencyJobBatch {
		batch := running[start:min(start+efficiencyJobBatch, len(running))]
		ids := make([]int, len(batch))
		var window time.Duration
		for i, j := range batch {
			ids[i] = j.ID
			window = max(window, age(j))
		}
		run(cgroupMedianExpr("rate(cgroup_cpu_total_seconds{}[2m])", ids, window), byJob(cpuCores))
		run(cgroupMedianExpr("cgroup_memory_used_bytes{}", ids, window), byJob(memUsed))
		run(cgroupMedianExpr("cgroup_memory_rss_bytes{}", ids, window), byJob(memRSS))
	}
	for _, j := range running {
		if j.GPUs > 0 && len(j.gpuIndicesByNode) > 0 {
			utilExpr, memExpr := gpuMedianExprs(j.gpuIndicesByNode, age(j))
			run(utilExpr, single(gpuUtilSum, j.ID))
			run(memExpr, single(gpuMemFrac, j.ID))
		}
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	for _, j := range running {
		e := JobEfficiency{JobID: j.ID}
		var total, weights float64
		if j.GPUs > 0 {
			e.GPUUtil = clampUnit(gpuUtilSum[j.ID] / (100 * float64(j.GPUs)))
			e.GPUMemUtil = clampUnit(gpuMemFrac[j.ID])
			total += e.GPUUtil + e.GPUMemUtil
			weights += 2
		}
		if j.CPUs > 0 {
			e.CPUUtil = clampUnit(cpuCores[j.ID] / float64(j.CPUs))
			total += e.CPUUtil
			weights++
		}
		if j.MemoryMB > 0 {
			bytes, ok := memUsed[j.ID]
			if !ok {
				bytes = memRSS[j.ID]
			}
			e.MemUtil = clampUnit(bytes / (float64(j.MemoryMB) * 1024 * 1024))
			total += e.MemUtil
			weights++
		}
		if weights > 0 {
			e.Score = clampUnit(total / weights)
		}
		result[j.ID] = e
	}
	return result, nil
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
