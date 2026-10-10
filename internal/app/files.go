// Package app is the VastLLM logic without the window: files next to the exe, the QuickPod API,
// renting (optionally a two-host race), the SSH tunnel and the 30-second upkeep tick.
package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// Label marks the machines rented by this program (the pod's altname at QuickPod).
	Label = "vast-llm"
	// Model is the name the engine in the image answers to (image/model.json).
	Model = "sc117-abliterated-iq3_xxs"
	// defaultImage is built from image/ of the same major version; VASTLLM_IMAGE overrides it for tests.
	defaultImage = "ghcr.io/kirilltsuran/vast-llm:3"
	provider     = "quickpod"
)

// GPUs are the cards offered in the window: the engine in the image is compiled for them (sm_86, sm_89, sm_120).
var GPUs = []string{"RTX 3090", "RTX 3090 Ti", "RTX 4090", "RTX 5090"}

// Config is config.json next to the exe.
type Config struct {
	Key string `json:"quickpod_api_key"`
	// the account's template (console.quickpod.io → Templates): image ghcr.io/kirilltsuran/vast-llm:3, launch mode docker
	Template string `json:"quickpod_template_uuid"`
	GPU      string `json:"gpu"`
	// the race rents the 2 best machines; if the second comes up first, the best gets this many seconds to catch up
	Grace int  `json:"cheapest_grace_seconds"`
	Race  bool `json:"race_two_hosts"`
	// rent only the fast configuration (>= 100 GB RAM, PCIe 4.0 x16, >= 20 threads): without it the model answers
	// slower. While there is none, Up waits for one up to wait_fast_minutes, then stops and says what is free instead.
	FastOnly    bool    `json:"fast_only"`
	WaitFastMin int     `json:"wait_fast_minutes"`
	LocalPort   int     `json:"local_port"`
	GrafanaPort int     `json:"grafana_port"`
	UsdRub      float64 `json:"usd_rub"`
	// start with Windows (minimized to the tray) and reconnect to the rented machine by itself
	Autostart bool `json:"autostart"`
	// logs\vast-llm-<date>.log next to the exe; files older than this are deleted
	LogDays int `json:"log_retention_days"`
	// extra "debug:" lines in the log file (API calls, SSH attempts, tunnel statistics)
	Debug bool `json:"debug_log"`
}

func defaultConfig() Config {
	return Config{GPU: GPUs[0], Grace: 20, FastOnly: true, WaitFastMin: 30, LocalPort: 8080, GrafanaPort: 3000, UsdRub: 83.56,
		Autostart: true, LogDays: 30, Debug: true}
}

// Rented is one machine of an Up that has not picked its winner yet.
type Rented struct {
	ID      string  `json:"pod_uuid"`
	Machine int64   `json:"machine"`
	GPU     string  `json:"gpu"`
	Geo     string  `json:"geo"`
	Dph     float64 `json:"usd_per_hour"`
	Created stamp   `json:"created"`
}

// State is state.json: every rented machine, written right after renting.
type State struct {
	Provider string   `json:"provider"` // "quickpod"; a state.json without it is of the Vast versions
	ID       string   `json:"pod_uuid"`
	Machine  int64    `json:"machine_id"`
	GPU      string   `json:"gpu"`
	Geo      string   `json:"geo"`
	Dph      float64  `json:"usd_per_hour"`
	Created  stamp    `json:"created"`
	HostKey  string   `json:"host_key"`
	Blocked  []Block  `json:"blocked"` // machines not to rent for a while, and why
	Pending  []Rented `json:"pending"`
}

// Block keeps a machine out of the offers until a time: for good when it cannot run the engine (no AVX2, too little
// memory), for hours when it failed once (a download, SSH) - a busy or vanished offer is no fault of the machine.
type Block struct {
	Machine int64  `json:"machine"`
	Until   stamp  `json:"until"`
	Reason  string `json:"reason"`
}

// stamp is a UTC time that also reads the zone-less form written by VastLLM 1.x ("0001-01-01T00:00:00").
type stamp struct{ time.Time }

func now() stamp { return stamp{time.Now().UTC()} }

func (s *stamp) UnmarshalJSON(b []byte) error {
	text := strings.Trim(string(b), `"`)
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05.999999999", text)
	}
	s.Time = t.UTC()
	return err
}

func (c *Core) path(name string) string { return filepath.Join(c.Dir, name) }

// loadConfig reads config.json (creating it on first start) and returns what the user still has to do, if anything.
func (c *Core) loadConfig() string {
	cfg := defaultConfig()
	raw, err := os.ReadFile(c.path("config.json"))
	switch {
	case os.IsNotExist(err):
		c.setConfig(cfg)
		return "создан config.json рядом с программой: впишите quickpod_api_key и quickpod_template_uuid"
	case err == nil:
		err = json.Unmarshal(raw, &cfg)
	}
	if err != nil {
		return "config.json не читается: " + err.Error()
	}
	c.setConfig(cfg) // rewrites the file: new keys appear, keys of older versions go away
	if strings.TrimSpace(cfg.Key) == "" || strings.TrimSpace(cfg.Template) == "" {
		return "впишите quickpod_api_key и quickpod_template_uuid в config.json и нажмите Up"
	}
	return ""
}

func (c *Core) setConfig(cfg Config) {
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
	if err := writeJSON(c.path("config.json"), cfg); err != nil {
		c.Log("config.json не записан: " + err.Error())
	}
}

// saveState is atomic: a crash or power loss never leaves a half-written state.json. Callers hold c.mu.
func (c *Core) saveState() {
	c.st.Provider = provider
	if c.st.Blocked == nil {
		c.st.Blocked = []Block{} // written as [] rather than null
	}
	if c.st.Pending == nil {
		c.st.Pending = []Rented{}
	}
	if err := writeJSON(c.path("state.json"), c.st); err != nil {
		c.logLocked("state.json не записан: " + err.Error())
	}
}

func (c *Core) loadState() {
	raw, err := os.ReadFile(c.path("state.json"))
	if os.IsNotExist(err) {
		return
	}
	var st State
	if err == nil {
		err = json.Unmarshal(raw, &st)
	}
	if err != nil {
		// not fatal: Startup adopts every machine with our label that QuickPod still lists
		c.Log("state.json не читается (" + err.Error() + "), машины будут найдены по списку QuickPod")
		return
	}
	if st.Provider != provider { // machine numbers and the blacklist of Vast mean nothing at QuickPod
		c.Log("state.json остался от версии для Vast — начинаю с чистого")
		var vast struct {
			ID      int64 `json:"instance_id"`
			Pending []struct {
				ID int64 `json:"id"`
			} `json:"pending"`
		}
		json.Unmarshal(raw, &vast)
		var left []string
		if vast.ID != 0 {
			left = append(left, fmt.Sprint(vast.ID))
		}
		for _, p := range vast.Pending {
			left = append(left, fmt.Sprint(p.ID))
		}
		if len(left) > 0 { // this version cannot delete them: they would be billed by Vast unseen
			c.Log("⚠ в нём были машины Vast " + strings.Join(left, ", ") + ": эта версия их не видит — удалите их на console.vast.ai, если они ещё есть")
		}
		return
	}
	c.mu.Lock()
	c.st = st
	c.mu.Unlock()
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- journal: logs\vast-llm-<date>.log, one file per day ----------

var logMu sync.Mutex

// Append writes one line to today's log file; usable before Core exists and from crash handlers.
func Append(dir, line string) {
	logMu.Lock()
	defer logMu.Unlock()
	logs := filepath.Join(dir, "logs")
	t := time.Now()
	// a journal that cannot be written has nowhere to report it: the line is dropped
	if os.MkdirAll(logs, 0o755) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(logs, "vast-llm-"+t.Format("2006-01-02")+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\r\n", t.Format("2006-01-02 15:04:05.000"), line)
}

// Log goes to the file and to the window.
func (c *Core) Log(line string) {
	Append(c.Dir, line)
	if c.OnLog != nil {
		c.OnLog(time.Now().Format("15:04:05 ") + line)
	}
}

func (c *Core) logf(format string, a ...any) { c.Log(fmt.Sprintf(format, a...)) }

// logLocked is Log for callers that hold c.mu: the window callback must not run under the lock.
func (c *Core) logLocked(line string) { go c.Log(line) }

func (c *Core) debugf(format string, a ...any) {
	if c.debug.Load() {
		Append(c.Dir, "debug: "+fmt.Sprintf(format, a...))
	}
}

// cleanLogs deletes day logs older than log_retention_days.
func (c *Core) cleanLogs() {
	days := max(1, c.Config().LogDays)
	files, _ := filepath.Glob(filepath.Join(c.Dir, "logs", "vast-llm-*.log"))
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil || time.Since(info.ModTime()) < time.Duration(days)*24*time.Hour {
			continue
		}
		if err := os.Remove(f); err != nil {
			c.debugf("старый журнал %s не удалён: %v", filepath.Base(f), err)
			continue
		}
		c.debugf("удалён старый журнал %s", filepath.Base(f))
	}
}
