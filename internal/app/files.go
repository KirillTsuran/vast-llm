// Package app is the VastLLM logic without the window: files next to the exe, the Vast API,
// renting (two-host race), the SSH tunnel and the 30-second upkeep tick.
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
	// Label marks the machines rented by this program at Vast.
	Label = "vast-llm"
	// Model is the name the engine in the image answers to (image/model.json).
	Model = "sc117-abliterated-iq3_xxs"
	// defaultImage is built from image/ of the same major version; VASTLLM_IMAGE overrides it for tests.
	defaultImage = "ghcr.io/kirilltsuran/vast-llm:2"
)

// GPUs are the cards offered in the window: the engine in the image is compiled for these three.
var GPUs = []string{"RTX 3090", "RTX 4090", "RTX 5090"}

// Config is config.json next to the exe.
type Config struct {
	VastKey string `json:"vast_api_key"`
	GPU     string `json:"gpu"`
	// the race rents the 2 cheapest; if the pricier one comes up first, the cheapest gets this many seconds to catch up
	Grace       int     `json:"cheapest_grace_seconds"`
	Datacenter  bool    `json:"datacenter_only"`
	Race        bool    `json:"race_two_hosts"`
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
	return Config{GPU: GPUs[0], Grace: 20, Race: true, LocalPort: 8080, GrafanaPort: 3000, UsdRub: 83.56,
		Autostart: true, LogDays: 30, Debug: true}
}

// Rented is one machine of an Up that has not picked its winner yet.
type Rented struct {
	ID      int64   `json:"id"`
	Machine int64   `json:"machine"`
	GPU     string  `json:"gpu"`
	Geo     string  `json:"geo"`
	Dph     float64 `json:"usd_per_hour"`
	Created stamp   `json:"created"`
}

// State is state.json: every rented machine, written right after renting.
type State struct {
	ID          int64    `json:"instance_id"`
	Machine     int64    `json:"machine_id"`
	GPU         string   `json:"gpu"`
	Geo         string   `json:"geo"`
	Dph         float64  `json:"usd_per_hour"`
	Created     stamp    `json:"created"`
	HostKey     string   `json:"host_key"`
	BadMachines []int64  `json:"bad_machines"`
	Pending     []Rented `json:"pending"`
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
		return "создан config.json рядом с программой: впишите vast_api_key"
	case err == nil:
		err = json.Unmarshal(raw, &cfg)
	}
	if err != nil {
		return "config.json не читается: " + err.Error()
	}
	c.setConfig(cfg) // rewrites the file: new keys appear, keys of older versions go away
	if strings.TrimSpace(cfg.VastKey) == "" {
		return "впишите vast_api_key в config.json и нажмите Up"
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
	if c.st.BadMachines == nil {
		c.st.BadMachines = []int64{} // written as [] rather than null
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
		// not fatal: Startup adopts every machine with our label that Vast still lists
		c.Log("state.json не читается (" + err.Error() + "), машины будут найдены по списку Vast")
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
