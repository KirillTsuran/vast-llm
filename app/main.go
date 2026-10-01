// vast-llm: one-button GPU for a local OpenAI-compatible endpoint.
// Rents a Vast.ai GPU running TabbyAPI (image from this repo), opens an SSH tunnel to it and serves
// http://127.0.0.1:<local_port>/v1 on this PC. Down destroys the instance (no storage cost).
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

//go:embed ui.html
var uiHTML []byte

const label = "vast-llm"

type Config struct {
	VastKey    string  `json:"vast_api_key"`
	GPU        string  `json:"gpu"`
	MaxDPH     float64 `json:"max_dph"`
	Datacenter bool    `json:"datacenter_only"`
	LocalPort  int     `json:"local_port"`
	UIPort     int     `json:"ui_port"`
	IdleMin    int     `json:"idle_minutes"`
	MaxHours   float64 `json:"max_hours"`
	Image      string  `json:"image"`
	Watchdog   int     `json:"watchdog_minutes"`
	Model      string  `json:"model"`
}

type State struct {
	ID          int       `json:"id"`
	GPU         string    `json:"gpu"`
	Geo         string    `json:"geo"`
	DPH         float64   `json:"dph"`
	Created     time.Time `json:"created"`
	HostKey     string    `json:"host_key"`
	BadMachines []int     `json:"bad_machines"`
	KeyAdded    bool      `json:"key_registered"`
}

type App struct {
	mu        sync.Mutex
	dir       string
	cfg       Config
	st        State
	phase     string
	msg       string
	logs      []string
	client    *ssh.Client
	signer    ssh.Signer
	pub       string
	busy      atomic.Bool
	active    atomic.Int64
	lastUse   atomic.Int64
	readyAt   time.Time
	logFile   *os.File
}

func main() {
	a := &App{phase: "off"}
	cfgDir, _ := os.UserConfigDir()
	a.dir = filepath.Join(cfgDir, label)
	os.MkdirAll(a.dir, 0o700)
	a.logFile, _ = os.OpenFile(filepath.Join(a.dir, "vast-llm.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	a.cfg = Config{GPU: "RTX 3090", MaxDPH: 0.40, LocalPort: 8080, UIPort: 8090, IdleMin: 30, MaxHours: 12,
		Image: "ghcr.io/kirilltsuran/vast-llm:latest", Watchdog: 60, Model: "qwen3.8-27b-uncensored"}
	readJSON(filepath.Join(a.dir, "config.json"), &a.cfg)
	readJSON(filepath.Join(a.dir, "state.json"), &a.st)
	a.saveConfig()

	ui := fmt.Sprintf("127.0.0.1:%d", a.cfg.UIPort)
	ln, err := net.Listen("tcp", ui)
	if err != nil { // already running: just show it
		openBrowser("http://" + ui)
		return
	}
	a.loadKey()
	go a.tunnel()
	go a.monitor()
	if a.st.ID != 0 {
		a.logf("found instance %d from previous session, reconnecting", a.st.ID)
		go a.up()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", "text/html; charset=utf-8"); w.Write(uiHTML) })
	mux.HandleFunc("/api/status", a.hStatus)
	mux.HandleFunc("/api/up", a.post(func() { go a.up() }))
	mux.HandleFunc("/api/down", a.post(func() { go a.down("manual") }))
	mux.HandleFunc("/api/config", a.hConfig)
	mux.HandleFunc("/api/quit", a.post(func() { go func() { time.Sleep(300 * time.Millisecond); os.Exit(0) }() }))
	openBrowser("http://" + ui)
	http.Serve(ln, mux)
}

// ---------- helpers ----------
func readJSON(p string, v any) { b, err := os.ReadFile(p); if err == nil { json.Unmarshal(b, v) } }
func writeJSON(p string, v any) { b, _ := json.MarshalIndent(v, "", "  "); os.WriteFile(p, b, 0o600) }
func (a *App) saveConfig()      { writeJSON(filepath.Join(a.dir, "config.json"), a.cfg) }
func (a *App) saveState()       { writeJSON(filepath.Join(a.dir, "state.json"), a.st) }
func openBrowser(u string)      { exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start() }

func (a *App) logf(f string, v ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(f, v...)
	a.mu.Lock()
	a.logs = append(a.logs, line)
	if len(a.logs) > 300 { a.logs = a.logs[len(a.logs)-300:] }
	a.mu.Unlock()
	if a.logFile != nil { a.logFile.WriteString(time.Now().Format("2006-01-02 ") + line + "\n") }
}

func (a *App) setPhase(p, m string) {
	a.mu.Lock(); changed := a.phase != p || a.msg != m; a.phase, a.msg = p, m; a.mu.Unlock()
	if changed { a.logf("[%s] %s", p, m) }
}

func (a *App) post(f func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { http.Error(w, "POST only", 405); return }
		f(); w.Write([]byte("{}"))
	}
}

// ---------- SSH key (generated once, kept in %APPDATA%\vast-llm) ----------
func (a *App) loadKey() {
	p := filepath.Join(a.dir, "id_ed25519")
	b, err := os.ReadFile(p)
	if err != nil {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		blk, _ := ssh.MarshalPrivateKey(priv, label)
		b = pem.EncodeToMemory(blk)
		os.WriteFile(p, b, 0o600)
	}
	s, err := ssh.ParsePrivateKey(b)
	if err != nil { a.logf("ssh key error: %v", err); return }
	a.signer = s
	a.pub = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) + " " + label
}

// ---------- Vast API ----------
func (a *App) api(method, path string, body, out any) error {
	base := "https://console.vast.ai/api/v0"
	if strings.HasPrefix(path, "/v1/") { base = "https://console.vast.ai/api" }
	var rd io.Reader
	if body != nil { b, _ := json.Marshal(body); rd = bytes.NewReader(b) }
	var lastErr error
	for i := 0; i < 4; i++ {
		req, _ := http.NewRequest(method, base+path, rd)
		req.Header.Set("Authorization", "Bearer "+a.cfg.VastKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 40 * time.Second}).Do(req)
		if err == nil {
			data, _ := io.ReadAll(resp.Body); resp.Body.Close()
			if resp.StatusCode < 300 {
				if out != nil { return json.Unmarshal(data, out) }
				return nil
			}
			lastErr = fmt.Errorf("HTTP %d: %.300s", resp.StatusCode, data)
			if resp.StatusCode < 500 && resp.StatusCode != 429 { return lastErr }
		} else { lastErr = err }
		time.Sleep(time.Duration(3*(i+1)) * time.Second)
		if body != nil { b, _ := json.Marshal(body); rd = bytes.NewReader(b) }
	}
	return lastErr
}

type Offer struct {
	ID        int     `json:"id"`
	Machine   int     `json:"machine_id"`
	DPH       float64 `json:"dph_total"`
	InetCost  float64 `json:"inet_down_cost"`
	Geo       string  `json:"geolocation"`
	GPU       string  `json:"gpu_name"`
}

type Instance struct {
	ID     int                            `json:"id"`
	Label  string                         `json:"label"`
	Status string                         `json:"actual_status"`
	Msg    string                         `json:"status_msg"`
	IP     string                         `json:"public_ipaddr"`
	Ports  map[string][]map[string]string `json:"ports"`
	SSHH   string                         `json:"ssh_host"`
	SSHP   int                            `json:"ssh_port"`
}

func (a *App) instances() ([]Instance, error) {
	var r struct{ Instances []Instance `json:"instances"` }
	err := a.api("GET", "/v1/instances", nil, &r)
	return r.Instances, err
}

func (a *App) instance(id int) (*Instance, error) {
	list, err := a.instances()
	if err != nil { return nil, err }
	for i := range list { if list[i].ID == id { return &list[i], nil } }
	return nil, nil
}

func (a *App) offers() ([]Offer, error) {
	q := map[string]any{"type": "on-demand", "limit": 200, "num_gpus": map[string]any{"eq": 1}, "gpu_name": map[string]any{"eq": a.cfg.GPU},
		"rentable": map[string]any{"eq": true}, "rented": map[string]any{"eq": false}, "verified": map[string]any{"eq": true},
		"cuda_max_good": map[string]any{"gte": 12.8}, "disk_space": map[string]any{"gte": 60}, "cpu_ram": map[string]any{"gte": 32000},
		"inet_down": map[string]any{"gte": 400}, "direct_port_count": map[string]any{"gte": 1}, "reliability": map[string]any{"gte": 0.98},
		"order": [][]string{{"dph_total", "asc"}}, "allocated_storage": 60}
	if a.cfg.Datacenter { q["datacenter"] = map[string]any{"eq": true} }
	var r struct{ Offers []Offer `json:"offers"` }
	if err := a.api("POST", "/bundles/", q, &r); err != nil { return nil, err }
	bad := map[int]bool{}
	for _, m := range a.st.BadMachines { bad[m] = true }
	var out []Offer
	for _, o := range r.Offers {
		if o.DPH <= a.cfg.MaxDPH && o.InetCost <= 0.01 && !bad[o.Machine] && !strings.Contains(o.Geo, "CN") { out = append(out, o) }
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DPH+20*out[i].InetCost < out[j].DPH+20*out[j].InetCost })
	return out, nil
}

func (a *App) destroy(id int) error {
	if err := a.api("DELETE", fmt.Sprintf("/instances/%d/", id), nil, nil); err != nil { a.logf("delete %d: %v", id, err) }
	for i := 0; i < 40; i++ {
		inst, err := a.instance(id)
		if err == nil && inst == nil { a.logf("instance %d destroyed (confirmed by API)", id); return nil }
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("deletion of %d not confirmed - check Vast console", id)
}

// ---------- up / down ----------
func (a *App) up() {
	if a.cfg.VastKey == "" { a.setPhase("error", "сначала введите Vast API key"); return }
	if !a.busy.CompareAndSwap(false, true) { return }
	defer a.busy.Store(false)
	if a.st.ID != 0 {
		inst, err := a.instance(a.st.ID)
		if err == nil && inst == nil { a.logf("instance %d no longer exists", a.st.ID); a.st = State{BadMachines: a.st.BadMachines, KeyAdded: a.st.KeyAdded}; a.saveState() }
	}
	if !a.st.KeyAdded {
		a.api("POST", "/ssh/", map[string]string{"ssh_key": a.pub}, nil) // account-level; "already exists" is fine
		a.st.KeyAdded = true; a.saveState()
	}
	for attempt := 1; a.st.ID == 0 && attempt <= 4; attempt++ {
		a.setPhase("renting", fmt.Sprintf("ищу %s до $%.2f/ч (попытка %d)", a.cfg.GPU, a.cfg.MaxDPH, attempt))
		offs, err := a.offers()
		if err != nil || len(offs) == 0 { a.setPhase("error", fmt.Sprintf("нет подходящих предложений: %v", err)); return }
		o := offs[0]
		body := map[string]any{"image": a.cfg.Image, "label": label, "disk": 60, "runtype": "ssh_direct", "target_state": "running",
			"cancel_unavail": true, "onstart": "bash /opt/llm/start.sh", "env": map[string]string{"WATCHDOG_MIN": fmt.Sprint(a.cfg.Watchdog)}}
		var r struct{ Success bool `json:"success"`; ID int `json:"new_contract"` }
		if err := a.api("PUT", fmt.Sprintf("/asks/%d/", o.ID), body, &r); err != nil || !r.Success {
			a.logf("create failed on offer %d: %v", o.ID, err); a.st.BadMachines = append(a.st.BadMachines, o.Machine); continue
		}
		a.st = State{ID: r.ID, GPU: o.GPU, Geo: o.Geo, DPH: o.DPH, Created: time.Now(), BadMachines: a.st.BadMachines, KeyAdded: true}
		a.saveState()
		a.logf("rented instance %d: %s %s $%.3f/h", r.ID, o.GPU, o.Geo, o.DPH)
		a.api("POST", fmt.Sprintf("/instances/%d/ssh/", r.ID), map[string]string{"ssh_key": a.pub}, nil)
		if err := a.connect(12 * time.Minute); err != nil {
			a.logf("host %d not usable (%v), replacing", o.Machine, err)
			a.st.BadMachines = append(a.st.BadMachines, o.Machine)
			id := a.st.ID; a.st.ID = 0; a.saveState(); a.destroy(id)
		}
	}
	if a.st.ID == 0 { a.setPhase("error", "не удалось поднять машину за 4 попытки"); return }
	if a.client == nil {
		if err := a.connect(5 * time.Minute); err != nil { a.setPhase("error", "нет SSH: "+err.Error()); return }
	}
	// wait for the model
	deadline := time.Now().Add(40 * time.Minute)
	for time.Now().Before(deadline) && a.st.ID != 0 {
		out, err := a.exec("cat /opt/llm/state 2>/dev/null; du -sh /opt/llm/models 2>/dev/null | cut -f1")
		f := strings.Fields(out)
		if err == nil && len(f) > 0 {
			switch f[0] {
			case "ready":
				a.readyAt = time.Now(); a.lastUse.Store(time.Now().Unix())
				a.setPhase("ready", fmt.Sprintf("готово: http://127.0.0.1:%d/v1, модель %s", a.cfg.LocalPort, a.cfg.Model)); return
			case "download-failed":
				a.setPhase("error", "скачивание модели не удалось, нажмите Down и Up"); return
			case "downloading":
				size := ""; if len(f) > 1 { size = f[1] }
				a.setPhase("downloading", "скачиваю модель (~18 ГБ): "+size)
			default:
				a.setPhase("loading", "загружаю модель в GPU")
			}
		} else { a.setPhase("booting", "машина запускается") }
		time.Sleep(10 * time.Second)
	}
	if a.st.ID != 0 { a.setPhase("error", "модель не загрузилась за 40 минут") }
}

func (a *App) down(reason string) {
	for !a.busy.CompareAndSwap(false, true) { time.Sleep(time.Second) }
	defer a.busy.Store(false)
	if a.st.ID == 0 { a.setPhase("off", "машин нет"); return }
	a.setPhase("stopping", "удаляю машину ("+reason+")")
	a.closeClient()
	if err := a.destroy(a.st.ID); err != nil { a.setPhase("error", err.Error()); return }
	a.st = State{BadMachines: a.st.BadMachines, KeyAdded: a.st.KeyAdded}; a.saveState()
	a.setPhase("off", "машина удалена, оплата остановлена")
}

// ---------- SSH + tunnel ----------
func (a *App) connect(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		inst, err := a.instance(a.st.ID)
		if err == nil && inst == nil { return errors.New("instance disappeared") }
		if err == nil && inst.Status == "running" && inst.IP != "" {
			host, port := inst.SSHH, inst.SSHP
			if p := inst.Ports["22/tcp"]; len(p) > 0 && p[0]["HostPort"] != "" { host, port = inst.IP, atoi(p[0]["HostPort"]) }
			cfg := &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(a.signer)}, Timeout: 15 * time.Second,
				HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
					fp := ssh.FingerprintSHA256(k)
					if a.st.HostKey == "" { a.st.HostKey = fp; a.saveState(); return nil }
					if a.st.HostKey != fp { return errors.New("host key changed") }
					return nil
				}}
			c, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", host, port), cfg)
			if err == nil {
				a.mu.Lock(); a.client = c; a.mu.Unlock()
				a.logf("SSH connected %s:%d", host, port)
				a.exec("touch /opt/llm/heartbeat")
				return nil
			}
			a.setPhase("booting", "жду SSH: "+shorten(err.Error()))
		} else if err == nil {
			a.setPhase("booting", "машина запускается: "+inst.Status+" "+shorten(inst.Msg))
		}
		time.Sleep(10 * time.Second)
	}
	return errors.New("SSH timeout")
}

func (a *App) closeClient() {
	a.mu.Lock(); c := a.client; a.client = nil; a.mu.Unlock()
	if c != nil { c.Close() }
}

func (a *App) exec(cmd string) (string, error) {
	a.mu.Lock(); c := a.client; a.mu.Unlock()
	if c == nil { return "", errors.New("no ssh") }
	s, err := c.NewSession()
	if err != nil { return "", err }
	defer s.Close()
	out, err := s.CombinedOutput(cmd)
	return string(out), err
}

// local 127.0.0.1:<port> -> remote 127.0.0.1:8080 through SSH
func (a *App) tunnel() {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", a.cfg.LocalPort))
	if err != nil { a.logf("порт %d занят: %v", a.cfg.LocalPort, err); return }
	for {
		conn, err := ln.Accept()
		if err != nil { continue }
		go func(conn net.Conn) {
			defer conn.Close()
			a.mu.Lock(); c := a.client; a.mu.Unlock()
			if c == nil {
				conn.Write([]byte("HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n{\"error\":{\"message\":\"GPU is off: press Up in vast-llm\"}}"))
				return
			}
			rc, err := c.Dial("tcp", "127.0.0.1:8080")
			if err != nil { return }
			defer rc.Close()
			a.active.Add(1); a.lastUse.Store(time.Now().Unix())
			defer func() { a.active.Add(-1); a.lastUse.Store(time.Now().Unix()) }()
			done := make(chan struct{}, 2)
			go func() { io.Copy(rc, conn); done <- struct{}{} }()
			go func() { io.Copy(conn, rc); done <- struct{}{} }()
			<-done
		}(conn)
	}
}

// keepalive, reconnect, heartbeat, idle/max-hours auto-down
func (a *App) monitor() {
	for range time.Tick(30 * time.Second) {
		if a.st.ID == 0 || a.busy.Load() { continue }
		a.mu.Lock(); c := a.client; a.mu.Unlock()
		alive := false
		if c != nil {
			if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err == nil { alive = true }
		}
		if !alive {
			a.logf("SSH lost, reconnecting")
			a.closeClient()
			inst, err := a.instance(a.st.ID)
			if err == nil && inst == nil { a.setPhase("error", "машина пропала у Vast - нажмите Up для новой"); a.st.ID = 0; a.saveState(); continue }
			if err := a.connect(3 * time.Minute); err != nil { a.logf("reconnect failed: %v", err); continue }
		}
		a.exec("touch /opt/llm/heartbeat")
		if a.phase == "ready" && a.active.Load() == 0 && a.cfg.IdleMin > 0 &&
			time.Since(time.Unix(a.lastUse.Load(), 0)) > time.Duration(a.cfg.IdleMin)*time.Minute {
			go a.down(fmt.Sprintf("нет запросов %d мин", a.cfg.IdleMin)); continue
		}
		if a.cfg.MaxHours > 0 && time.Since(a.st.Created) > time.Duration(a.cfg.MaxHours*float64(time.Hour)) {
			go a.down(fmt.Sprintf("лимит %.0f ч", a.cfg.MaxHours))
		}
	}
}

// ---------- HTTP handlers ----------
func (a *App) hStatus(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	logs := append([]string(nil), a.logs[max(0, len(a.logs)-40):]...)
	s := map[string]any{"phase": a.phase, "msg": a.msg, "configured": a.cfg.VastKey != "", "cfg": map[string]any{
		"gpu": a.cfg.GPU, "max_dph": a.cfg.MaxDPH, "datacenter_only": a.cfg.Datacenter, "idle_minutes": a.cfg.IdleMin, "max_hours": a.cfg.MaxHours},
		"endpoint": fmt.Sprintf("http://127.0.0.1:%d/v1", a.cfg.LocalPort), "model": a.cfg.Model, "tunnel": a.client != nil,
		"active_requests": a.active.Load(), "logs": logs}
	a.mu.Unlock()
	if a.st.ID != 0 {
		h := time.Since(a.st.Created).Hours()
		s["instance"] = map[string]any{"id": a.st.ID, "gpu": a.st.GPU, "geo": a.st.Geo, "dph": a.st.DPH,
			"uptime_min": int(h * 60), "spent": fmt.Sprintf("%.2f", h*a.st.DPH)}
		if a.phase == "ready" { s["idle_min"] = int(time.Since(time.Unix(a.lastUse.Load(), 0)).Minutes()) }
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s)
}

func (a *App) hConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "POST only", 405); return }
	var in map[string]any
	json.NewDecoder(r.Body).Decode(&in)
	if v, ok := in["vast_api_key"].(string); ok && strings.TrimSpace(v) != "" { a.cfg.VastKey = strings.TrimSpace(v) }
	if v, ok := in["gpu"].(string); ok && v != "" { a.cfg.GPU = v }
	if v, ok := in["max_dph"].(float64); ok && v > 0 { a.cfg.MaxDPH = v }
	if v, ok := in["datacenter_only"].(bool); ok { a.cfg.Datacenter = v }
	if v, ok := in["idle_minutes"].(float64); ok && v >= 0 { a.cfg.IdleMin = int(v) }
	a.saveConfig()
	a.logf("settings saved: %s, max $%.2f/h, datacenter=%v, idle %d min", a.cfg.GPU, a.cfg.MaxDPH, a.cfg.Datacenter, a.cfg.IdleMin)
	w.Write([]byte("{}"))
}

func atoi(s string) int { n := 0; fmt.Sscan(s, &n); return n }
func shorten(s string) string { if len(s) > 90 { return s[:90] }; return s }
