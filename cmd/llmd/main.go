// llmd is the only process the image starts on a rented machine: its own sshd for the app's tunnel, the monitoring
// stack, the model download, and the Strata engine with its OpenAI-compatible API on 127.0.0.1:8080.
// The app follows /opt/llm/state: downloading … | preparing | loading | ready | failed <why>.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	strataDir = "/opt/strata"
	dataDir   = "/opt/llm/data"
	logDir    = "/var/log/llm"
	stateFile = "/opt/llm/state"
	engineURL = "http://127.0.0.1:8080"
	// the experts (40 GiB) are pinned in RAM; with one request at 262144 the engine takes ~46 GiB, and a long prompt
	// also needs the page cache for the PLE table read from disk
	minRAMGiB = 58
	// the MTP draft vocabulary (Strata's data/draft_vocab_<name>.bin): English, code and Cyrillic
	draftVocab = "cyrillic"
	// RAM kept for the engine and the page cache before any goes to the conversation cache
	conversationRAMFloorGiB = 56
)

// Model is /opt/llm/model.json: pinned Hugging Face files and the name the API answers to.
type Model struct {
	Name     string `json:"name"`
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	Context  int    `json:"context"`
	Parallel int    `json:"parallel"` // requests decoded at once, each with the whole context; more wait in the queue
	Weights  File   `json:"weights"`  // GGUF shard 1: iq_pack source, native projections
	PLE      File   `json:"ple"`      // GGUF shard 2: the PLE lookup table, read from disk
	MTP      File   `json:"mtp"`      // draft layer
}

type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func (m Model) path(f File) string { return filepath.Join(dataDir, "model", filepath.Base(f.Name)) }

var engineRestarts atomic.Int64

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	os.MkdirAll(logDir, 0o755)
	if logFile, err := os.OpenFile(logDir+"/llmd.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, logFile)) // the file is what Loki and Grafana show
	}
	startSSHD()
	var model Model
	raw, err := os.ReadFile("/opt/llm/model.json")
	if err == nil {
		err = json.Unmarshal(raw, &model)
	}
	if err != nil {
		fail(fmt.Errorf("model.json: %w", err))
	}
	go serveMetrics("127.0.0.1:9101")
	startMonitoring()
	if err := bringUp(model); err != nil {
		fail(err)
	}
	select {}
}

// fail leaves the reason for the app and keeps the container (and its sshd, logs and Grafana) alive.
func fail(err error) {
	log.Print("failed: ", err)
	setState("failed " + err.Error())
	select {}
}

func setState(s string) {
	tmp := stateFile + ".tmp"
	err := os.WriteFile(tmp, []byte(s+"\n"), 0o644)
	if err == nil {
		err = os.Rename(tmp, stateFile) // atomic: the app never reads a half-written line
	}
	if err != nil {
		log.Print("state: ", err)
	}
}

// startSSHD: the container's own sshd, key-only, with the app's public key (PUBKEY_B64).
func startSSHD() {
	key, err := base64.StdEncoding.DecodeString(os.Getenv("PUBKEY_B64"))
	if err != nil || len(key) == 0 {
		log.Print("PUBKEY_B64 is missing or broken: nobody will be able to log in")
	}
	os.MkdirAll("/root/.ssh", 0o700)
	os.MkdirAll("/run/sshd", 0o755)
	if err := os.WriteFile("/root/.ssh/authorized_keys", append(key, '\n'), 0o600); err != nil {
		log.Print("authorized_keys: ", err)
	}
	for _, argv := range [][]string{
		{"ssh-keygen", "-A"},
		{"/usr/sbin/sshd", "-o", "PermitRootLogin=prohibit-password", "-o", "PasswordAuthentication=no", "-o", "ClientAliveInterval=30"},
	} {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stderr = os.Stderr // sshd goes on running in the background: no pipe to wait on
		if err := cmd.Run(); err != nil {
			log.Printf("%s: %v", argv[0], err)
		}
	}
}

// keep runs a service forever: its output goes to /var/log/llm/<name>.log, an exit is logged and it starts again.
func keep(name, dir string, env []string, onExit func(), argv ...string) {
	go func() {
		for {
			out, err := os.OpenFile(filepath.Join(logDir, name+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				log.Printf("%s: %v", name, err)
				time.Sleep(5 * time.Second)
				continue
			}
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, append(os.Environ(), env...), out, out
			err = cmd.Run()
			fmt.Fprintf(out, "%s %s exited (%v), restart in 5s\n", time.Now().UTC().Format("2006-01-02 15:04:05"), name, err)
			out.Close()
			if onExit != nil {
				onExit()
			}
			time.Sleep(5 * time.Second)
		}
	}()
}

// startMonitoring: everything listens on 127.0.0.1; Grafana is reached through the app's SSH tunnel.
// Data is kept at most 3 days and disappears with the machine.
func startMonitoring() {
	const m = "/opt/monitoring"
	for _, d := range []string{"/var/lib/prometheus", "/var/lib/loki", "/var/lib/alloy", "/var/lib/grafana", m + "/dashboards",
		m + "/provisioning/alerting", m + "/provisioning/plugins"} { // Grafana logs an error for every missing provisioning directory
		os.MkdirAll(d, 0o755)
	}
	for name, board := range dashboards() {
		if err := os.WriteFile(filepath.Join(m, "dashboards", name), board, 0o644); err != nil {
			log.Print("dashboard: ", err)
		}
	}
	// overlay is not excluded (the default excludes it), so the container's root disk is visible
	keep("node_exporter", "", nil, nil, "/opt/node_exporter/node_exporter", "--web.listen-address=127.0.0.1:9100",
		"--collector.filesystem.fs-types-exclude=^(autofs|binfmt_misc|bpf|cgroup2?|configfs|debugfs|devpts|devtmpfs|fusectl|hugetlbfs|iso9660|mqueue|nsfs|proc|procfs|pstore|rpc_pipefs|securityfs|selinuxfs|squashfs|sysfs|tracefs)$")
	keep("prometheus", "", nil, nil, "/opt/prometheus/prometheus", "--config.file="+m+"/prometheus.yml",
		"--storage.tsdb.path=/var/lib/prometheus", "--storage.tsdb.retention.time=3d", "--web.listen-address=127.0.0.1:9090")
	keep("loki", "", nil, nil, "/opt/loki/loki", "--config.file="+m+"/loki.yml")
	keep("alloy", "", nil, nil, "/opt/alloy/alloy", "run", "--server.http.listen-addr=127.0.0.1:12345", "--storage.path=/var/lib/alloy", m+"/config.alloy")
	keep("grafana", "", []string{
		"GF_SERVER_HTTP_ADDR=127.0.0.1", "GF_SERVER_HTTP_PORT=3000", "GF_PATHS_PROVISIONING=" + m + "/provisioning", "GF_PATHS_DATA=/var/lib/grafana",
		"GF_AUTH_ANONYMOUS_ENABLED=true", "GF_AUTH_ANONYMOUS_ORG_ROLE=Admin", "GF_AUTH_DISABLE_LOGIN_FORM=true", "GF_ANALYTICS_REPORTING_ENABLED=false",
		"GF_ANALYTICS_CHECK_FOR_UPDATES=false", "GF_NEWS_NEWS_FEED_ENABLED=false", "GF_USERS_DEFAULT_LANGUAGE=ru-RU", "GF_LOG_LEVEL=warn",
		"GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=" + m + "/dashboards/llm-metrics.json",
	}, nil, "/opt/grafana/bin/grafana", "server", "--homepath", "/opt/grafana")
}

// bringUp: check the machine, download and verify the model, prepare it for the engine, start the engine.
func bringUp(m Model) error {
	if err := checkMachine("/proc/cpuinfo", cgroupDir, "/proc/meminfo"); err != nil {
		return err
	}
	files := []File{m.Weights, m.PLE, m.MTP}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	var got atomic.Int64
	stop := make(chan struct{})
	go func() { // progress for the app's window
		last, tick := int64(0), time.NewTicker(2*time.Second)
		defer tick.Stop()
		for {
			now := got.Load()
			setState(fmt.Sprintf("downloading %.1f из %.1f ГБ, %d МБ/с", float64(now)/1e9, float64(total)/1e9, (now-last)/2e6))
			last = now
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	for _, f := range files {
		url := "https://huggingface.co/" + m.Repo + "/resolve/" + m.Revision + "/" + f.Name
		if err := download(context.Background(), http.DefaultClient, url, m.path(f), f, &got); err != nil {
			close(stop)
			return fmt.Errorf("скачивание %s: %w", filepath.Base(f.Name), err)
		}
	}
	close(stop)

	setState("preparing")
	pack, mtp := dataDir+"/pack", dataDir+"/mtp/rt"
	python := strataDir + "/.venv/bin/python"
	steps := [][]string{ // docs/ORCA.md of Strata: the manual setup of a model outside its installer menu
		{python, "tools/mtp_rt.py", "--gguf", m.path(m.MTP), "--out", mtp},
		{python, "tools/iq_pack.py", "--gguf", m.path(m.Weights), "--out", pack},
	}
	if _, err := os.Stat(pack + "/tokenizer"); err == nil {
		steps = nil // prepared before this container restart
	}
	// the default draft vocabulary has 142 Cyrillic tokens of 18,580: drafts of a Russian answer are almost never accepted
	steps = append(steps, []string{"cp", "data/draft_vocab_" + draftVocab + ".bin", mtp + "/draft_vocab.bin"})
	for _, argv := range steps {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = strataDir
		cmd.Env = append(os.Environ(), "STRATA_GGUF_PY="+strataDir+"/third_party/llama.cpp/gguf-py")
		out, err := cmd.CombinedOutput()
		os.WriteFile(filepath.Join(logDir, "prepare.log"), out, 0o644)
		if err != nil {
			return fmt.Errorf("подготовка весов (%s): %v: %s", filepath.Base(argv[1]), err, lastLine(out))
		}
	}

	cpus := cpuLimit(cgroupDir)
	if cores := physicalCores("/proc/cpuinfo"); cores > 0 {
		cpus = min(cpus, cores)
	}
	cfg, err := json.MarshalIndent(engineConfig(m, cpus, float64(memoryLimit(cgroupDir, "/proc/meminfo"))/(1<<30)), "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile("/opt/llm/strata.json", cfg, 0o644); err != nil {
		return err
	}
	setState("loading")
	// the engine is restarted if it ever exits (CUDA error, out of memory, crash); the reason stays in strata.log
	var crashes crashLoop
	keep("strata", strataDir, []string{"STRATA_NO_LARGEPAGES=1"}, func() {
		engineRestarts.Add(1)
		// an engine that cannot start (a flag it does not know, out of memory, a CUDA error) would be restarted forever
		// while the machine is billed: after 5 exits in 10 minutes the app is told the machine failed
		if crashes.exit(time.Now()) {
			setState("failed движок падает: " + crashReason(tail(filepath.Join(logDir, "strata.log"), 8192)))
			return
		}
		setState("loading")
	},
		strataDir+"/.venv/bin/python", "-m", "serve.server", "--engine", "strata", "--config", "/opt/llm/strata.json", "--port", "8080")
	go func() { // ready once a real request is answered, not merely when the port is open
		probing := &http.Client{Timeout: 2 * time.Minute}
		probe := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Say OK"}],"max_tokens":4,"reasoning_effort":"none"}`, m.Name)
		for ready := false; ; time.Sleep(5 * time.Second) {
			wasReady := ready
			if !ready {
				resp, err := probing.Post(engineURL+"/v1/chat/completions", "application/json", strings.NewReader(probe))
				ready = err == nil && resp.StatusCode == http.StatusOK
				if err == nil {
					resp.Body.Close()
				}
			} else if resp, err := probing.Get(engineURL + "/health"); err != nil {
				ready = false
			} else {
				resp.Body.Close()
			}
			if ready && !wasReady {
				setState("ready")
			}
		}
	}()
	return nil
}

// engineConfig is the configuration measured on RTX 3090 (results-maxreason-20261006, and the ~110 tok/s setup of
// 2026-10-10), with the engine's own defaults for the prompt cache and the repeat guard. `cpus` is the container's
// quota capped at the physical cores (hyper-threads add nothing to the expert pool, results-cores-20261009); the
// engine's default counts the host's cores, which halves the speed on a machine with a quota.
func engineConfig(m Model, cpus int, memGiB float64) map[string]any {
	pack := dataDir + "/pack"
	args := []string{
		"--pack", pack, "--native", m.path(m.Weights), "--ple-gguf", m.path(m.PLE),
		"--expert-profile", strataDir + "/data/expert-profile.bin", "--expert-cache", "auto", "--prefill", "auto",
		"--spec", "4", "--spec-min-p", "0.5", "--mtp", dataDir + "/mtp/rt",
		"--max-context", strconv.Itoa(m.Context), "--kv", "int8", "--kv-resident", "32768",
		// 700 MiB as in the ~110 tok/s setup (2048 before): ~800 more experts fit in VRAM
		"--pool-workers", strconv.Itoa(max(1, cpus-1)), "--vram-reserve-mib", "700",
	}
	// the conversation cache parks whole conversations in RAM: an agent and its subagents take turns in the one slot, and
	// without it every switch read the conversation again (24K tokens: 10.6 s -> 0.9 s; the continuation decodes the same
	// tokens). It gets the RAM the engine (~46 GiB) and the page cache of the PLE table leave, up to 16 GiB.
	if mib := min(16384, int(memGiB-conversationRAMFloorGiB)*1024); mib >= 4096 { // less holds barely one long conversation
		args = append(args, "--conversation-cache-mib", strconv.Itoa(mib), "--conversation-cache-slots", "4")
	}
	cfg := map[string]any{
		"exe":  strataDir + "/engine/strata",
		"args": args,
		"cwd":  strataDir, "tokenizer": pack + "/tokenizer", "model_name": m.Name, "lib_dirs": []string{"/usr/local/cuda-13.0/lib64"},
		"host": "127.0.0.1", "port": 8080, "open_browser": false, "log": logDir + "/engine.log",
		// a client that asks for more answer than the context has left (ZCode: up to 131072 tokens beside a 131K-token
		// prompt) gets the answer shortened to the room left instead of a 400 that stops its turn
		"fit_max_tokens": true,
	}
	// every slot keeps its own context (+3.1 GiB of RAM, -0.95 GiB of the expert cache in VRAM) and decodes without the
	// MTP drafts: on a 24 GB card slots cut a single answer by ~10% and add nothing to the total, so model.json has one
	if m.Parallel > 1 {
		cfg["parallel"] = m.Parallel
	}
	return cfg
}

// checkMachine refuses hosts the engine cannot run on, so the app takes another one instead of waiting for a crash.
func checkMachine(cpuinfo, cgroup, meminfo string) error {
	raw, err := os.ReadFile(cpuinfo)
	if err != nil {
		return err
	}
	if !bytes.Contains(raw, []byte(" avx2")) {
		return fmt.Errorf("процессор без AVX2")
	}
	limit := memoryLimit(cgroup, meminfo)
	if gib := float64(limit) / (1 << 30); gib < minRAMGiB {
		return fmt.Errorf("мало памяти: контейнеру доступно %.1f ГиБ, нужно %d", gib, minRAMGiB)
	}
	return nil
}

// memoryLimit is the container's RAM limit in bytes, or the host's RAM when the container has none.
func memoryLimit(cgroup, meminfo string) int64 {
	if raw, err := os.ReadFile(filepath.Join(cgroup, "memory.max")); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil {
			return n
		}
	}
	// cgroup v1: "no limit" is written there as a huge number
	if raw, err := os.ReadFile(filepath.Join(cgroup, "memory", "memory.limit_in_bytes")); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil && n < 1<<60 {
			return n
		}
	}
	raw, _ := os.ReadFile(meminfo)
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb * 1024
		}
	}
	return 0
}

// cpuLimit is the number of CPUs the container may use: its cgroup quota (v2, else v1), else every CPU it sees.
func cpuLimit(cgroup string) int {
	if raw, err := os.ReadFile(filepath.Join(cgroup, "cpu.max")); err == nil {
		if f := strings.Fields(string(raw)); len(f) == 2 && f[0] != "max" {
			quota, _ := strconv.ParseFloat(f[0], 64)
			period, _ := strconv.ParseFloat(f[1], 64)
			if quota > 0 && period > 0 {
				return int(quota / period)
			}
		}
	}
	for _, dir := range []string{"cpu", "cpu,cpuacct"} {
		quota, err1 := os.ReadFile(filepath.Join(cgroup, dir, "cpu.cfs_quota_us"))
		period, err2 := os.ReadFile(filepath.Join(cgroup, dir, "cpu.cfs_period_us"))
		q, _ := strconv.ParseFloat(strings.TrimSpace(string(quota)), 64)
		p, _ := strconv.ParseFloat(strings.TrimSpace(string(period)), 64)
		if err1 == nil && err2 == nil && q > 0 && p > 0 {
			return int(q / p)
		}
	}
	out, _ := exec.Command("nproc").Output()
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// physicalCores counts the distinct (physical id, core id) pairs of /proc/cpuinfo; 0 when it does not say.
func physicalCores(cpuinfo string) int {
	raw, err := os.ReadFile(cpuinfo)
	if err != nil {
		return 0
	}
	cores := map[[2]string]bool{}
	var socket string
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "physical id":
			socket = strings.TrimSpace(value)
		case "core id":
			cores[[2]string{socket, strings.TrimSpace(value)}] = true
		}
	}
	return len(cores)
}

// crashLoop counts the engine's exits; exit says whether there were 5 within 10 minutes.
type crashLoop struct{ at []time.Time }

func (c *crashLoop) exit(t time.Time) bool {
	recent := c.at[:0]
	for _, a := range c.at {
		if t.Sub(a) < 10*time.Minute {
			recent = append(recent, a)
		}
	}
	c.at = append(recent, t)
	return len(c.at) >= 5
}

// crashReason is the engine's last line before keep's own "exited" lines: what it died of.
func crashReason(text []byte) string {
	lines := strings.Split(strings.TrimSpace(string(text)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" && !strings.Contains(line, " exited (") {
			return line
		}
	}
	return "причина не записана"
}

// tail is the last n bytes of a file.
func tail(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > n {
		f.Seek(info.Size()-n, io.SeekStart)
	}
	out, _ := io.ReadAll(f)
	return out
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines[len(lines)-1]
}
