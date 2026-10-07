package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var cgroupDir = "/sys/fs/cgroup"

// serveMetrics is the Prometheus exporter on 127.0.0.1:9101: the GPU (nvidia-smi), this container's own limits and
// usage (cgroup v2; node_exporter sees the whole Vast host), and the engine's JSON /metrics turned into counters.
func serveMetrics(addr string) {
	var finished finishCounter
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		var b strings.Builder
		if out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu="+gpuQuery, "--format=csv,noheader,nounits").Output(); err != nil {
			fmt.Fprintf(&b, "# gpu: %v\n", err)
		} else {
			gpuMetrics(&b, string(out))
		}
		containerMetrics(&b, cgroupDir)
		engine, err := fetchEngine(ctx, engineURL+"/metrics")
		gauge(&b, "llm_engine_up", b2f(err == nil))
		if err == nil {
			engineMetrics(&b, engine, &finished)
		}
		counterLine(&b, "llm_engine_restarts_total", float64(engineRestarts.Load()))
		w.Write([]byte(b.String()))
	})
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "metrics:", err)
	}
}

func gauge(b *strings.Builder, name string, v float64) {
	fmt.Fprintf(b, "# TYPE %s gauge\n%s %g\n", name, name, v)
}

func counterLine(b *strings.Builder, name string, v float64) {
	fmt.Fprintf(b, "# TYPE %s counter\n%s %g\n", name, name, v)
}

func b2f(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

const gpuQuery = "utilization.gpu,memory.used,memory.total,power.draw,power.limit,temperature.gpu,clocks.sm,clocks.max.sm"

var gpuNames = []string{"llm_gpu_util_percent", "llm_gpu_mem_used_mib", "llm_gpu_mem_total_mib", "llm_gpu_power_watts", "llm_gpu_power_limit_watts",
	"llm_gpu_temp_celsius", "llm_gpu_sm_clock_mhz", "llm_gpu_sm_clock_max_mhz"}

// gpuMetrics reads the first card's CSV line; a value the card does not report ("[N/A]") is left out.
func gpuMetrics(b *strings.Builder, csv string) {
	line, _, _ := strings.Cut(csv, "\n")
	for i, field := range strings.Split(line, ",") {
		if v, err := strconv.ParseFloat(strings.TrimSpace(field), 64); err == nil && i < len(gpuNames) {
			gauge(b, gpuNames[i], v)
		}
	}
}

// keyed reads a cgroup "key value" file.
func keyed(dir, name string) map[string]float64 {
	out := map[string]float64{}
	raw, _ := os.ReadFile(filepath.Join(dir, name))
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			if v, err := strconv.ParseFloat(f[1], 64); err == nil {
				out[f[0]] = v
			}
		}
	}
	return out
}

func containerMetrics(b *strings.Builder, dir string) {
	raw, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		fmt.Fprintf(b, "# container: %v\n", err)
		return
	}
	used, _ := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	gauge(b, "llm_container_memory_used_bytes", used)
	if raw, err := os.ReadFile(filepath.Join(dir, "memory.max")); err == nil {
		if limit, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64); err == nil {
			gauge(b, "llm_container_memory_limit_bytes", limit)
		}
	}
	stat := keyed(dir, "memory.stat")
	// what cannot be dropped under pressure (programs, shared memory) vs the file cache the kernel frees when needed
	gauge(b, "llm_container_memory_programs_bytes", stat["anon"]+stat["shmem"]+stat["kernel"])
	gauge(b, "llm_container_memory_file_cache_bytes", max(stat["file"]-stat["shmem"], 0))
	counterLine(b, "llm_container_oom_kills_total", keyed(dir, "memory.events")["oom_kill"])
	counterLine(b, "llm_container_cpu_seconds_total", keyed(dir, "cpu.stat")["usage_usec"]/1e6)
	if raw, err := os.ReadFile(filepath.Join(dir, "cpu.max")); err == nil {
		if f := strings.Fields(string(raw)); len(f) == 2 && f[0] != "max" {
			quota, _ := strconv.ParseFloat(f[0], 64)
			period, _ := strconv.ParseFloat(f[1], 64)
			gauge(b, "llm_container_cpu_limit_cores", quota/period)
		}
	}
	var read, written float64
	io, _ := os.ReadFile(filepath.Join(dir, "io.stat"))
	for _, field := range strings.Fields(string(io)) {
		if v, ok := strings.CutPrefix(field, "rbytes="); ok {
			n, _ := strconv.ParseFloat(v, 64)
			read += n
		} else if v, ok := strings.CutPrefix(field, "wbytes="); ok {
			n, _ := strconv.ParseFloat(v, 64)
			written += n
		}
	}
	counterLine(b, "llm_container_disk_read_bytes_total", read)
	counterLine(b, "llm_container_disk_written_bytes_total", written)
}

// engineStats is the part of Strata's GET /metrics (JSON) the dashboards use.
type engineStats struct {
	Engine struct {
		MaxContext float64 `json:"max_context"`
		CacheMiB   float64 `json:"expert_cache_mib"`
		FreeMiB    float64 `json:"vram_free_mib"`
	} `json:"engine"`
	Live struct {
		State   string   `json:"state"` // idle | reading | generating | unloaded
		Queued  float64  `json:"queued"`
		TokS    *float64 `json:"tok_s"`
		Prefill *float64 `json:"prefill_tok_s_mean"`
		Prompt  *float64 `json:"prompt_tokens"`
		Written *float64 `json:"generated"`
		// with "parallel" slots: live above describes only the newest request, these describe all of them
		Running float64 `json:"running"`
		Waiting float64 `json:"waiting"`
		Slots   []struct {
			State   string  `json:"state"` // idle | decoding | ...
			Prompt  float64 `json:"prompt_tokens"`
			Written float64 `json:"generated"`
			TokS    float64 `json:"tok_s"`
		} `json:"slots"`
	} `json:"live"`
	Totals struct { // since the engine started
		Requests float64 `json:"requests"`
		Prompt   float64 `json:"prompt_tokens"`
		Reused   float64 `json:"reused"`
		Output   float64 `json:"output_tokens"`
		PromptMs float64 `json:"prompt_ms"`
		DecodeMs float64 `json:"decode_ms"`
		Offered  float64 `json:"drafts_offered"`
		Accepted float64 `json:"drafts_accepted"`
	} `json:"totals"`
	Requests []struct { // the last 12
		Time   float64 `json:"time"`
		Finish string  `json:"finish"`
	} `json:"requests"`
}

func fetchEngine(ctx context.Context, url string) (engineStats, error) {
	var s engineStats
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return s, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return s, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

// finishCounter counts answers by how they ended; the engine only lists its last requests, so each is counted once.
type finishCounter struct {
	mu     sync.Mutex
	seen   float64 // time of the newest request already counted
	counts map[string]float64
}

func (f *finishCounter) add(s engineStats) map[string]float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts == nil {
		f.counts = map[string]float64{"stop": 0, "length": 0}
	}
	newest := f.seen
	for _, r := range s.Requests {
		if r.Time > f.seen {
			f.counts[r.Finish]++
			newest = max(newest, r.Time)
		}
	}
	f.seen = newest
	out := map[string]float64{}
	for k, v := range f.counts {
		out[k] = v
	}
	return out
}

func engineMetrics(b *strings.Builder, s engineStats, finished *finishCounter) {
	val := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	gauge(b, "llm_context_tokens", s.Engine.MaxContext)
	gauge(b, "llm_expert_cache_mib", s.Engine.CacheMiB)
	gauge(b, "llm_vram_free_mib", s.Engine.FreeMiB)
	// one request alone runs outside the slots and is described by live itself; requests that run together sit in
	// slots, and live then shows only the newest of them: the speed is the sum over the slots, the size the largest
	processing := b2f(s.Live.State == "reading" || s.Live.State == "generating")
	speed, size, inSlots := val(s.Live.TokS), val(s.Live.Prompt)+val(s.Live.Written), 0.0
	for _, slot := range s.Live.Slots {
		if slot.State == "idle" {
			continue
		}
		if inSlots == 0 {
			speed, size = 0, 0
		}
		inSlots++
		speed += slot.TokS
		size = max(size, slot.Prompt+slot.Written)
	}
	gauge(b, "llm_requests_processing", max(processing, s.Live.Running, inSlots))
	gauge(b, "llm_requests_queued", s.Live.Queued+s.Live.Waiting)
	gauge(b, "llm_live_tok_s", speed)
	gauge(b, "llm_live_tok_s_per_request", speed/max(inSlots, 1))
	gauge(b, "llm_live_prefill_tok_s", b2f(s.Live.State == "reading")*val(s.Live.Prefill))
	gauge(b, "llm_live_request_tokens", size)
	counterLine(b, "llm_requests_total", s.Totals.Requests)
	counterLine(b, "llm_prompt_tokens_total", s.Totals.Prompt)
	counterLine(b, "llm_reused_tokens_total", s.Totals.Reused)
	counterLine(b, "llm_output_tokens_total", s.Totals.Output)
	counterLine(b, "llm_prompt_seconds_total", s.Totals.PromptMs/1000)
	counterLine(b, "llm_decode_seconds_total", s.Totals.DecodeMs/1000)
	counterLine(b, "llm_drafts_offered_total", s.Totals.Offered)
	counterLine(b, "llm_drafts_accepted_total", s.Totals.Accepted)
	counts := finished.add(s)
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	b.WriteString("# TYPE llm_finished_total counter\n")
	for _, reason := range reasons {
		fmt.Fprintf(b, "llm_finished_total{finish=%q} %g\n", reason, counts[reason])
	}
}
