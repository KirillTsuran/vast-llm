package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rangeServer serves data by Range requests; the first `breaks` requests are cut off after a quarter.
func rangeServer(data []byte, breaks int32) (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= breaks {
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[:len(data)/4]) // fewer bytes than asked, then the answer ends
			return
		}
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(data))
	})), &calls
}

func TestDownload(t *testing.T) {
	data := bytes.Repeat([]byte("strata-"), 5000)
	sum := sha256.Sum256(data)
	file := File{Name: "dir/model.gguf", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	path := filepath.Join(t.TempDir(), "model", "model.gguf")

	srv, calls := rangeServer(data, 1)
	defer srv.Close()
	var got atomic.Int64
	if err := download(context.Background(), srv.Client(), srv.URL, path, file, &got); err != nil {
		t.Fatal(err)
	}
	if have, _ := os.ReadFile(path); !bytes.Equal(have, data) {
		t.Error("the file must be complete after a broken first request")
	}
	if _, err := os.Stat(path + ".part"); err == nil {
		t.Error(".part must be renamed into place")
	}
	if got.Load() != file.Size {
		t.Errorf("progress counts every byte once: %d of %d", got.Load(), file.Size)
	}

	before := calls.Load()
	if err := download(context.Background(), srv.Client(), srv.URL, path, file, &got); err != nil || calls.Load() != before {
		t.Errorf("a verified file is kept without a request: err %v, requests %d", err, calls.Load()-before)
	}

	wrong := file
	wrong.SHA256 = strings.Repeat("0", 64)
	other := filepath.Join(t.TempDir(), "x.gguf")
	if err := download(context.Background(), srv.Client(), srv.URL, other, wrong, &got); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Errorf("a wrong hash must fail, got %v", err)
	}
	if _, err := os.Stat(other); err == nil {
		t.Error("a file with a wrong hash must not be left in place")
	}
}

func cgroup(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	for name, text := range files {
		os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644)
	}
	return dir
}

func TestMachineChecks(t *testing.T) {
	// the tested host: Ryzen 7 5700X with a 15.36-core quota and a 60.2 GiB limit
	good := cgroup(t, map[string]string{"cpu.max": "1536000 100000\n", "memory.max": "64647856128\n", "cpuinfo": "flags : fpu sse avx avx2 bmi1\n"})
	if n := cpuLimit(good); n != 15 {
		t.Errorf("cpu quota 15.36 -> 15 CPUs, got %d", n)
	}
	if err := checkMachine(filepath.Join(good, "cpuinfo"), good, ""); err != nil {
		t.Errorf("the tested host must pass: %v", err)
	}
	small := cgroup(t, map[string]string{"memory.max": "34359738368\n", "cpuinfo": "flags : avx avx2\n"})
	if err := checkMachine(filepath.Join(small, "cpuinfo"), small, ""); err == nil || !strings.Contains(err.Error(), "мало памяти") {
		t.Errorf("32 GiB must be refused, got %v", err)
	}
	old := cgroup(t, map[string]string{"memory.max": "max\n", "cpuinfo": "flags : fpu sse avx\n", "meminfo": "MemTotal:       131072000 kB\n"})
	if err := checkMachine(filepath.Join(old, "cpuinfo"), old, filepath.Join(old, "meminfo")); err == nil || !strings.Contains(err.Error(), "AVX2") {
		t.Errorf("a CPU without AVX2 must be refused, got %v", err)
	}
	if got := memoryLimit(old, filepath.Join(old, "meminfo")); got != 131072000*1024 {
		t.Errorf("no container limit: the host's RAM, got %d", got)
	}
}

func TestEngineConfig(t *testing.T) {
	m := Model{Name: "sc117", Context: 262144, Weights: File{Name: "IQ3_XXS/a-00001-of-00002.gguf"}, PLE: File{Name: "IQ3_XXS/a-00002-of-00002.gguf"}}
	cfg := engineConfig(m, 15)
	args := strings.Join(cfg["args"].([]string), " ")
	for _, want := range []string{"--pool-workers 14", "--max-context 262144", "--native /opt/llm/data/model/a-00001-of-00002.gguf",
		"--ple-gguf /opt/llm/data/model/a-00002-of-00002.gguf", "--vram-reserve-mib 2048", "--kv-resident 32768"} {
		if !strings.Contains(filepath.ToSlash(args), want) {
			t.Errorf("args lack %q: %s", want, args)
		}
	}
	if cfg["parallel"] != nil {
		t.Errorf("one request at a time is the engine's default: no \"parallel\" key, got %v", cfg["parallel"])
	}
	m.Parallel = 3
	if got := engineConfig(m, 15)["parallel"]; got != 3 {
		t.Errorf("three slots must reach the engine's config, got %v", got)
	}
	if strings.Contains(args, "--prompt-cache") || cfg["repeat_stop_tokens"] != nil || cfg["host"] != "127.0.0.1" {
		t.Errorf("prompt cache and repeat guard stay at the engine's defaults, the API stays on loopback: %v", cfg)
	}
}

func TestMetrics(t *testing.T) {
	raw, err := os.ReadFile("testdata/strata-metrics.json") // GET /metrics of engine 0.1.39, saved by the RTX 3090 test
	if err != nil {
		t.Fatal(err)
	}
	var stats engineStats
	if err := json.Unmarshal(raw, &stats); err != nil {
		t.Fatal(err)
	}
	var finished finishCounter
	var b strings.Builder
	engineMetrics(&b, stats, &finished)
	b.Reset()
	engineMetrics(&b, stats, &finished) // the same requests seen again must not be counted twice
	gpuMetrics(&b, "37, 22467, 24576, 239.71, 350.00, 61, [N/A], 2100\n")
	containerMetrics(&b, cgroup(t, map[string]string{
		"memory.current": "59700000000\n", "memory.max": "64647856128\n", "memory.stat": "anon 100\nfile 500\nshmem 40\nkernel 7\n",
		"memory.events": "low 0\noom_kill 2\n", "cpu.stat": "usage_usec 2500000\n", "cpu.max": "1536000 100000\n",
		"io.stat": "8:0 rbytes=100 wbytes=10 rios=1\n8:16 rbytes=5 wbytes=1\n"}))
	out := b.String()
	for _, want := range []string{
		"llm_gpu_util_percent 37\n", "llm_gpu_power_limit_watts 350\n", "llm_gpu_sm_clock_max_mhz 2100\n",
		"llm_container_memory_limit_bytes 6.4647856128e+10\n", "llm_container_memory_programs_bytes 147\n", "llm_container_memory_file_cache_bytes 460\n",
		"llm_container_oom_kills_total 2\n", "llm_container_cpu_seconds_total 2.5\n", "llm_container_cpu_limit_cores 15.36\n",
		"llm_container_disk_read_bytes_total 105\n", "llm_container_disk_written_bytes_total 11\n",
		"llm_context_tokens 262144\n", "llm_expert_cache_mib 15582\n", "llm_requests_processing 0\n", "llm_requests_total 1\n",
		"llm_output_tokens_total 2\n", "llm_decode_seconds_total 0.092", "llm_drafts_accepted_total 3\n",
		"llm_finished_total{finish=\"stop\"} 1\n", "llm_finished_total{finish=\"length\"} 0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "llm_gpu_sm_clock_mhz ") {
		t.Error("a value the card does not report must be left out")
	}
}

func TestDashboards(t *testing.T) {
	boards := dashboards()
	for name, wantPanels := range map[string]int{"llm-metrics.json": 38, "llm-logs.json": 9} {
		var d struct {
			UID    string
			Panels []struct {
				Type    string
				Targets []struct{ Expr string }
				GridPos struct{ X, Y, W, H int }
			}
		}
		if err := json.Unmarshal(boards[name], &d); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d.UID+".json" != name || len(d.Panels) != wantPanels {
			t.Errorf("%s: uid %q, %d panels (want %d)", name, d.UID, len(d.Panels), wantPanels)
		}
		taken := map[[2]int]bool{}
		for _, p := range d.Panels {
			if p.Type != "row" && (len(p.Targets) == 0 || p.Targets[0].Expr == "") {
				t.Errorf("%s: a %s panel without a query", name, p.Type)
			}
			if p.GridPos.X+p.GridPos.W > 24 {
				t.Errorf("%s: a panel wider than the grid: %+v", name, p.GridPos)
			}
			for x := p.GridPos.X; x < p.GridPos.X+p.GridPos.W; x++ {
				for y := p.GridPos.Y; y < p.GridPos.Y+p.GridPos.H; y++ {
					if taken[[2]int{x, y}] {
						t.Fatalf("%s: panels overlap at %d,%d", name, x, y)
					}
					taken[[2]int{x, y}] = true
				}
			}
		}
	}
}

// Opt-in (LLMD_HF_TEST=1, ~0.9 GB of traffic): the smallest file of image/model.json from the real Hugging Face.
func TestDownloadFromHuggingFace(t *testing.T) {
	if os.Getenv("LLMD_HF_TEST") == "" {
		t.Skip("set LLMD_HF_TEST=1 to download from Hugging Face")
	}
	raw, err := os.ReadFile("../../image/model.json")
	if err != nil {
		t.Fatal(err)
	}
	var m Model
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var got atomic.Int64
	start := time.Now()
	url := "https://huggingface.co/" + m.Repo + "/resolve/" + m.Revision + "/" + m.MTP.Name
	if err := download(context.Background(), http.DefaultClient, url, filepath.Join(t.TempDir(), "mtp.gguf"), m.MTP, &got); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d bytes, SHA256 ok, %.0f MB/s", got.Load(), float64(got.Load())/1e6/time.Since(start).Seconds())
}

// GET /metrics of the engine with "parallel": 3 while two requests run in slots (taken from the production machine):
// live describes only the newest request, the dashboards must show both.
func TestMetricsOfParallelSlots(t *testing.T) {
	var stats engineStats
	raw := `{"live": {"state": "generating", "queued": 0, "prompt_tokens": 45727, "generated": 375, "tok_s": 54.6, "parallel": 3, "running": 2, "waiting": 1,
		"slots": [{"slot": 0, "state": "decoding", "prompt_tokens": 22454, "generated": 24588, "tok_s": 56.4}, {"slot": 1, "state": "idle", "held_tokens": 449},
		{"slot": 2, "state": "decoding", "prompt_tokens": 45727, "generated": 375, "tok_s": 27.8}]}}`
	if err := json.Unmarshal([]byte(raw), &stats); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	engineMetrics(&b, stats, &finishCounter{})
	for _, want := range []string{"llm_requests_processing 2\n", "llm_requests_queued 1\n", "llm_live_tok_s 84.2", "llm_live_tok_s_per_request 42.1", "llm_live_request_tokens 47042\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
}
