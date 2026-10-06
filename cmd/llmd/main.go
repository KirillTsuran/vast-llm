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
	// the engine's measured peak is 55.6 GiB inside the container (RTX 3090, context 262144)
	minRAMGiB = 58
)

// Model is /opt/llm/model.json: pinned Hugging Face files and the name the API answers to.
type Model struct {
	Name     string `json:"name"`
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	Context  int    `json:"context"`
	Weights  File   `json:"weights"` // GGUF shard 1: iq_pack source, native projections
	PLE      File   `json:"ple"`     // GGUF shard 2: the PLE lookup table, read from disk
	MTP      File   `json:"mtp"`     // draft layer
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
	if own, err := os.OpenFile(logDir+"/llmd.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, own)) // the file is what Loki and Grafana show
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
		{"cp", "data/draft_vocab.bin", mtp + "/draft_vocab.bin"},
		{python, "tools/iq_pack.py", "--gguf", m.path(m.Weights), "--out", pack},
	}
	if _, err := os.Stat(pack + "/tokenizer"); err == nil {
		steps = nil // prepared before this container restart
	}
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

	cfg, err := json.MarshalIndent(engineConfig(m, cpuLimit(cgroupDir)), "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile("/opt/llm/strata.json", cfg, 0o644); err != nil {
		return err
	}
	setState("loading")
	// the engine is restarted if it ever exits (CUDA error, out of memory, crash); the reason stays in strata.log
	keep("strata", strataDir, []string{"STRATA_NO_LARGEPAGES=1"}, func() { engineRestarts.Add(1); setState("loading") },
		strataDir+"/.venv/bin/python", "-m", "serve.server", "--engine", "strata", "--config", "/opt/llm/strata.json", "--port", "8080")
	go func() { // ready once a real request is answered, not merely when the port is open
		probing := &http.Client{Timeout: 2 * time.Minute}
		probe := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Say OK"}],"max_tokens":4,"reasoning_effort":"none"}`, m.Name)
		for ready := false; ; time.Sleep(5 * time.Second) {
			was := ready
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
			if ready && !was {
				setState("ready")
			}
		}
	}()
	return nil
}

// engineConfig is the configuration measured on RTX 3090 (results-maxreason-20261006), with the engine's own
// defaults for the prompt cache and the repeat guard. The CPU threads follow the container's quota: the engine's
// default counts the host's cores, which halves the speed on a machine with a quota.
func engineConfig(m Model, cpus int) map[string]any {
	pack := dataDir + "/pack"
	return map[string]any{
		"exe": strataDir + "/engine/strata",
		"args": []string{
			"--pack", pack, "--native", m.path(m.Weights), "--ple-gguf", m.path(m.PLE),
			"--expert-profile", strataDir + "/data/expert-profile.bin", "--expert-cache", "auto", "--prefill", "auto",
			"--spec", "4", "--spec-min-p", "0.5", "--mtp", dataDir + "/mtp/rt",
			"--max-context", strconv.Itoa(m.Context), "--kv", "int8", "--kv-resident", "32768",
			"--pool-workers", strconv.Itoa(max(1, cpus-1)), "--vram-reserve-mib", "2048",
		},
		"cwd": strataDir, "tokenizer": pack + "/tokenizer", "model_name": m.Name, "lib_dirs": []string{"/usr/local/cuda-13.0/lib64"},
		"host": "127.0.0.1", "port": 8080, "open_browser": false, "log": logDir + "/engine.log",
	}
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
	raw, _ := os.ReadFile(meminfo)
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb * 1024
		}
	}
	return 0
}

// cpuLimit is the number of CPUs the container may use: its cgroup quota, else every CPU it sees.
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
	out, _ := exec.Command("nproc").Output()
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines[len(lines)-1]
}
