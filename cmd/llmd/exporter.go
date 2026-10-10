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
// usage (cgroup v2; node_exporter sees the whole host), and the engine's JSON /metrics turned into counters.
func serveMetrics(addr string) {
	var answers requestLog
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		// the engine answers between generation steps, which takes seconds under load: it gets nearly the whole
		// scrape timeout (9 s) and does not wait for nvidia-smi
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		type gpuAnswer struct {
			csv []byte
			err error
		}
		gpu := make(chan gpuAnswer, 1)
		go func() {
			gpuCtx, stop := context.WithTimeout(ctx, 4*time.Second)
			defer stop()
			out, err := exec.CommandContext(gpuCtx, "nvidia-smi", "--query-gpu="+gpuQuery, "--format=csv,noheader,nounits").Output()
			gpu <- gpuAnswer{out, err}
		}()
		engine, err := fetchEngine(ctx, engineURL+"/metrics")
		var b strings.Builder
		if g := <-gpu; g.err != nil {
			fmt.Fprintf(&b, "# gpu: %v\n", g.err)
		} else {
			gpuMetrics(&b, string(g.csv))
		}
		containerMetrics(&b, cgroupDir)
		gauge(&b, "llm_engine_up", b2f(err == nil))
		if err == nil {
			engineMetrics(&b, engine, &answers)
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
	ioStat, _ := os.ReadFile(filepath.Join(dir, "io.stat"))
	for _, field := range strings.Fields(string(ioStat)) {
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
		Prompt  *float64 `json:"prompt_tokens"`
		Written *float64 `json:"generated"`
		Elapsed *float64 `json:"elapsed_s"`
		// with "parallel" slots: live above describes only the newest request, these describe all of them
		Parallel float64 `json:"parallel"`
		Running  float64 `json:"running"`
		Waiting  float64 `json:"waiting"`
		Slots    []struct {
			// no tok_s: a request moved into a slot mid-answer brings its earlier tokens along, while the slot's clock
			// starts at the move, and showed thousands of tok/s
			State   string  `json:"state"` // idle | reading | decoding
			Prompt  float64 `json:"prompt_tokens"`
			Written float64 `json:"generated"`
			Elapsed float64 `json:"elapsed_s"`
		} `json:"slots"`
	} `json:"live"`
	Hardware struct {
		// sampled every second: the sum over all running requests of the tokens each wrote in the last 2 s; unlike
		// live.tok_s it is there while the newest request still reads its prompt
		TokS *float64 `json:"tok_s"`
	} `json:"hardware"`
	Totals struct { // since the engine started
		Requests float64 `json:"requests"`
		Prompt   float64 `json:"prompt_tokens"`
		Reused   float64 `json:"reused"`
		Output   float64 `json:"output_tokens"`
		PromptMs float64 `json:"prompt_ms"`
		Offered  float64 `json:"drafts_offered"`
		Accepted float64 `json:"drafts_accepted"`
	} `json:"totals"`
	Requests []struct { // the last 12 that ended, ordered by when they ended
		Time     float64 `json:"time"` // when it started
		Finish   string  `json:"finish"`
		Duration float64 `json:"duration_s"`
		PromptMs float64 `json:"prompt_ms"`
		Output   float64 `json:"output_tokens"`
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

// requestLog turns the engine's list of its last 12 finished requests into counters: answers by how they ended, and
// the time and tokens of each. The engine's own totals cannot give the speed: for a request decoded in a slot they
// count the tokens but only the first token's decode time.
type requestLog struct {
	mu         sync.Mutex
	seen       map[float64]bool // start times listed at the previous scrape: the list is ordered by end, not by start
	lastTotal  float64          // totals.requests at the previous scrape
	started    bool
	finished   map[string]float64
	seconds    float64 // whole duration of the counted requests
	promptSecs float64 // of it, reading the prompt
	output     float64 // tokens they wrote
}

type requestTotals struct {
	finished                    map[string]float64
	seconds, promptSecs, output float64
}

func (l *requestLog) add(s engineStats) requestTotals {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.finished == nil { // every way an answer ends, so that the first one of a kind is an increase from 0
		l.finished = map[string]float64{"stop": 0, "length": 0, "cancel": 0, "disconnect": 0, "error": 0, "unknown": 0}
	}
	now, fresh := map[float64]bool{}, 0.0
	for _, r := range s.Requests {
		now[r.Time] = true
		if l.seen[r.Time] {
			continue
		}
		fresh++
		l.finished[r.Finish]++
		l.seconds += r.Duration
		l.promptSecs += r.PromptMs / 1000
		l.output += r.Output
	}
	// more answers ended since the last scrape than the list still shows (or a scrape failed): counted, reason unknown
	ended := s.Totals.Requests - l.lastTotal
	if ended < 0 { // the engine restarted
		ended = s.Totals.Requests
	}
	if l.started && ended > fresh {
		l.finished["unknown"] += ended - fresh
	}
	l.seen, l.lastTotal, l.started = now, s.Totals.Requests, true
	out := requestTotals{map[string]float64{}, l.seconds, l.promptSecs, l.output}
	for k, v := range l.finished {
		out.finished[k] = v
	}
	return out
}

func engineMetrics(b *strings.Builder, s engineStats, answers *requestLog) {
	val := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	gauge(b, "llm_context_tokens", s.Engine.MaxContext)
	gauge(b, "llm_expert_cache_mib", s.Engine.CacheMiB)
	gauge(b, "llm_vram_free_mib", s.Engine.FreeMiB)

	// One request alone runs outside the slots and is described by live itself. Requests that run together sit in
	// slots, and live then shows only the newest of them. The speed is the engine's sum over all of them.
	busy := s.Live.State == "reading" || s.Live.State == "generating"
	speed, elapsed, written := 0.0, 0.0, 0.0
	size := val(s.Live.Prompt) + val(s.Live.Written)
	if s.Hardware.TokS != nil {
		speed = *s.Hardware.TokS
	} else if s.Live.State == "generating" {
		speed = val(s.Live.TokS)
	}
	if busy {
		elapsed, written = val(s.Live.Elapsed), val(s.Live.Written)
	}
	inSlots, decoding := 0.0, 0.0
	for _, slot := range s.Live.Slots {
		if slot.State == "idle" {
			continue
		}
		inSlots++
		if slot.State == "decoding" {
			decoding++
		}
		written = max(written, slot.Written)
		elapsed = max(elapsed, slot.Elapsed)
		// a request moved into a slot mid-answer reports what it had written by then as prompt too, so the sum is an
		// upper bound; the newest request has the exact numbers in live
		if slot.Written != val(s.Live.Written) {
			size = max(size, slot.Prompt+slot.Written)
		}
	}
	// nothing writes, yet the engine's sum is not 0: a request that waits (for a slot, or reads its prompt again after
	// a move) counts there with its mean since its first token. While slots are busy no request writes outside them.
	if inSlots > 0 && decoding == 0 || inSlots == 0 && s.Live.State != "generating" {
		speed = 0
	}
	gauge(b, "llm_slots_total", max(s.Live.Parallel, 1))
	gauge(b, "llm_slots_busy", inSlots)
	gauge(b, "llm_requests_processing", max(b2f(busy), s.Live.Running, inSlots))
	gauge(b, "llm_requests_queued", max(s.Live.Queued, s.Live.Waiting))
	gauge(b, "llm_live_tok_s", speed)
	gauge(b, "llm_live_tok_s_per_request", speed/max(decoding, 1))
	gauge(b, "llm_live_request_tokens", size)
	gauge(b, "llm_live_longest_request_seconds", elapsed)
	gauge(b, "llm_live_longest_answer_tokens", written)

	counterLine(b, "llm_requests_total", s.Totals.Requests)
	counterLine(b, "llm_prompt_tokens_total", s.Totals.Prompt)
	counterLine(b, "llm_reused_tokens_total", s.Totals.Reused)
	counterLine(b, "llm_output_tokens_total", s.Totals.Output)
	counterLine(b, "llm_prompt_seconds_total", s.Totals.PromptMs/1000)
	counterLine(b, "llm_drafts_offered_total", s.Totals.Offered)
	counterLine(b, "llm_drafts_accepted_total", s.Totals.Accepted)

	t := answers.add(s)
	counterLine(b, "llm_request_seconds_total", t.seconds)
	counterLine(b, "llm_request_prompt_seconds_total", t.promptSecs)
	counterLine(b, "llm_request_output_tokens_total", t.output)
	reasons := make([]string, 0, len(t.finished))
	for reason := range t.finished {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	b.WriteString("# TYPE llm_finished_total counter\n")
	for _, reason := range reasons {
		fmt.Fprintf(b, "llm_finished_total{finish=%q} %g\n", reason, t.finished[reason])
	}
}
