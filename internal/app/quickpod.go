package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What the engine and the model need from a machine (measured on RTX 3090: 21.9 GiB of VRAM, ~46 GiB of RAM for
// the engine with one request, ~78 GB of model files plus the prepared pack).
const (
	diskGB     = 150
	minRAMGB   = 62 // the experts (40 GiB) are pinned in RAM; below this the engine falls back to the much slower SSD path
	minThreads = 16 // 8 physical cores: below that the answer slows down (4 cores: -15%, 2 cores: -38%)
	minInet    = 300
	// QuickPod bills traffic at $4 per TB: the 78 GB download of every Up costs ~$0.31 on any machine
	modelGB = 78
	// the configuration the Strata setup with ~110 tok/s on one RTX 3090 runs on: all experts in RAM with room to spare,
	// PCIe 4.0 x16 for the experts read by the GPU, 12 cores for the misses of the expert cache
	fastRAMGB     = 100
	fastThreads   = 20
	minReliablePc = 95
)

// QuickPod is the api.quickpod.org client: the public API lists the machines, the secure-key API rents and deletes.
type QuickPod struct {
	Key   func() string
	Base  string
	HTTP  *http.Client
	Pause time.Duration // before the 2nd attempt; grows linearly
	Debug func(format string, a ...any)
}

func newQuickPod(key func() string, debug func(string, ...any)) *QuickPod {
	return &QuickPod{Key: key, Base: "https://api.quickpod.org", HTTP: &http.Client{Timeout: 40 * time.Second}, Pause: 3 * time.Second, Debug: debug}
}

// call retries network errors, 5xx and 429 (4 attempts); any other answer is final.
func (q *QuickPod) call(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	var last error
	for try := 1; ; try++ {
		started := time.Now()
		status, text, err := q.once(ctx, method, path, body)
		switch {
		case err != nil:
			last = err
			q.Debug("quickpod %s %s ошибка: %v", method, path, err)
		case status/100 == 2:
			q.Debug("quickpod %s %s -> %d за %d мс, попытка %d", method, path, status, time.Since(started).Milliseconds(), try)
			if out == nil || len(bytes.TrimSpace(text)) == 0 {
				return nil
			}
			return json.Unmarshal(text, out)
		default:
			last = fmt.Errorf("HTTP %d: %.300s", status, text)
			q.Debug("quickpod %s %s -> %d", method, path, status)
			if status < 500 && status != http.StatusTooManyRequests {
				return last
			}
		}
		if try == 4 {
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(q.Pause * time.Duration(try)):
		}
	}
}

func (q *QuickPod) once(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, q.Base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if key := q.Key(); key != "" { // the way QuickPod's own CLI sends a secure API key
		req.Header.Set("X-API-Key", key)
		req.Header.Set("Authorization", "ApiKey "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := q.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(resp.Body)
	return resp.StatusCode, text, err
}

// ---------- loose JSON: the API's numbers and strings are not typed consistently ----------

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func num(v any) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(str(v)), 64)
	return f
}

func sub(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// ---------- pods ----------

// Pod is a rented machine as QuickPod reports it.
type Pod struct {
	UUID     string
	Name     string // altname: Label for the machines of this program
	State    string // Docker's: running, created, exited, ...
	Intended string
	IP       string
	Machine  int64
	Offer    int64
	GPU      string
	Geo      string
	Dph      float64
	Ports    string // the port mappings, whatever their format: sshPort reads the one of port 22
}

func podFrom(m map[string]any) Pod {
	machine, offer := sub(m["_machines"]), sub(m["_offers"])
	ports := str(m["port_mappings"])
	if p := str(m["Ports"]); p != "" {
		ports += " " + p
	}
	return Pod{
		UUID: firstNonEmpty(str(m["pod_uuid"]), str(m["Names"])), Name: str(m["altname"]), State: strings.ToLower(str(m["State"])),
		Intended: strings.ToLower(str(m["intended_state"])), IP: firstNonEmpty(str(m["public_ipaddr"]), str(machine["public_ipaddr"])),
		Machine: int64(num(m["machines_id"])), Offer: int64(num(m["offers_id"])), GPU: shortGPU(str(offer["gpu_type"])), Geo: str(machine["geolocation"]),
		Dph: num(m["hourly_cost"]), Ports: ports,
	}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

var sshMapping = []*regexp.Regexp{
	regexp.MustCompile(`(\d+)\s*->\s*22(?:/tcp)?\b`),                               // Docker's "0.0.0.0:40022->22/tcp"
	regexp.MustCompile(`"22(?:/tcp)?"\s*:\s*\[\s*\{[^}]*"HostPort"\s*:\s*"(\d+)"`), // Docker's {"22/tcp":[{"HostPort":"40022"}]}
	regexp.MustCompile(`"22(?:/tcp)?"\s*:\s*"?(\d+)"?`),                            // {"22": 40022}
	regexp.MustCompile(`\b22(?:/tcp)?\s*(?:=>|:)\s*(\d+)\b`),                       // "22:40022"
}

// sshPort is the external port mapped to the container's own sshd, 0 while QuickPod has not published it.
func (p Pod) sshPort() int {
	for _, re := range sshMapping {
		if m := re.FindStringSubmatch(p.Ports); m != nil {
			if port, _ := strconv.Atoi(m[1]); port > 0 && port != 22 {
				return port
			}
		}
	}
	return 0
}

func (p Pod) running() bool { return p.State == "running" && p.IP != "" }

// stopped: QuickPod stops (does not delete) the pods of an account whose credit ran out; the disk is still billed.
func (p Pod) stopped() bool {
	return p.Intended == "stopped" || p.State == "exited" || p.State == "stopped" || p.State == "dead"
}

// Pods lists the account's GPU pods that still exist.
func (q *QuickPod) Pods(ctx context.Context) ([]Pod, error) {
	var raw []map[string]any
	if err := q.call(ctx, http.MethodGet, "/update/api/gpu_pods", nil, &raw); err != nil {
		return nil, err
	}
	var pods []Pod
	for _, m := range raw {
		if destroyed, _ := m["destroyed"].(bool); !destroyed {
			pods = append(pods, podFrom(m))
		}
	}
	return pods, nil
}

// Pod returns nil when QuickPod no longer lists the machine.
func (q *QuickPod) Pod(ctx context.Context, uuid string) (*Pod, error) {
	all, err := q.Pods(ctx)
	for i := range all {
		if all[i].UUID == uuid {
			return &all[i], err
		}
	}
	return nil, err
}

// ---------- offers ----------

// Offer is a machine that can be rented.
type Offer struct {
	ID          int64
	Machine     int64
	Dph         float64
	GPU         string
	RAM         float64 // GB
	Threads     int
	CPU         string
	PCIe        string // generation
	Lanes       string
	Geo         string
	Inet        float64 // Mbit/s down
	FreeDisk    float64 // GB
	Reliability float64 // percent
	Verified    bool
	GPUs        int
	Occupied    bool
}

// shortGPU turns QuickPod's "NVIDIA GeForce RTX 3090" into the window's "RTX 3090".
func shortGPU(name string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(name, "NVIDIA "), "GeForce "))
}

// Offers lists the GPU machines QuickPod has free (the public API: no key needed).
func (q *QuickPod) Offers(ctx context.Context) ([]Offer, error) {
	var raw []map[string]any
	if err := q.call(ctx, http.MethodGet, "/rentable", nil, &raw); err != nil {
		return nil, err
	}
	offers := make([]Offer, 0, len(raw))
	for _, m := range raw {
		machine := sub(m["_machines"])
		occupied, _ := m["occupied"].(bool)
		verified := strings.EqualFold(str(machine["verification"]), "true") || strings.EqualFold(str(machine["verification"]), "verified")
		offers = append(offers, Offer{
			ID: int64(num(m["id"])), Machine: int64(num(m["machines_id"])), Dph: num(m["hourly_cost"]), GPU: shortGPU(str(m["gpu_type"])),
			RAM: num(m["memory"]), Threads: int(num(m["cpus"])), CPU: strings.TrimSpace(str(machine["cpu_name"])), PCIe: str(m["gpu_pcie"]),
			Lanes: str(m["gpu_lanes"]), Geo: str(machine["geolocation"]), Inet: num(machine["inet_down"]), FreeDisk: num(machine["avail_disk_space"]),
			Reliability: num(machine["reliability"]), Verified: verified, GPUs: int(num(m["num_gpus"])), Occupied: occupied,
		})
	}
	return offers, nil
}

// fast: the configuration of the ~110 tok/s setup (all experts in RAM with room, PCIe 4.0+ x16, 10+ cores).
func (o Offer) fast() bool {
	gen, _ := strconv.Atoi(o.PCIe)
	return o.RAM >= fastRAMGB && gen >= 4 && o.Lanes == "16" && o.Threads >= fastThreads
}

// usable keeps the free single-GPU machines of the chosen card the model runs well on. The fast configuration comes
// first, then the cheapest; among equals the one with more cores, then the faster network.
func usable(offers []Offer, gpu string, bad []int64) (keep []Offer, dropped []string) {
	for _, o := range offers {
		if o.GPU != gpu || o.GPUs != 1 || o.Occupied {
			continue
		}
		reason := ""
		switch {
		case o.RAM < minRAMGB:
			reason = fmt.Sprintf("мало RAM: %.0f ГБ", o.RAM)
		case o.Threads < minThreads:
			reason = fmt.Sprintf("мало ядер: %d потоков", o.Threads)
		case o.FreeDisk > 0 && o.FreeDisk < diskGB:
			reason = fmt.Sprintf("мало диска: %.0f ГБ", o.FreeDisk)
		case o.Inet > 0 && o.Inet < minInet:
			reason = fmt.Sprintf("медленная сеть: %.0f Мбит/с, модель качалась бы %.0f мин", o.Inet, modelGB*8000/o.Inet/60)
		case o.Reliability > 0 && o.Reliability < minReliablePc:
			reason = fmt.Sprintf("надёжность %.1f%%", o.Reliability)
		case contains(bad, o.Machine):
			reason = "машина в чёрном списке"
		}
		if reason == "" {
			keep = append(keep, o)
		} else {
			dropped = append(dropped, fmt.Sprintf("%d (%s, %s, $%.3f/ч): %s", o.ID, o.CPU, o.Geo, o.Dph, reason))
		}
	}
	sort.SliceStable(keep, func(i, j int) bool {
		a, b := keep[i], keep[j]
		switch {
		case a.fast() != b.fast():
			return a.fast()
		case a.Dph != b.Dph:
			return a.Dph < b.Dph
		case a.Threads != b.Threads:
			return a.Threads > b.Threads
		}
		return a.Inet > b.Inet
	})
	return keep, dropped
}

func contains(list []int64, x int64) bool {
	for _, v := range list {
		if v == x {
			return true
		}
	}
	return false
}

// Rent creates a pod on an offer from the account's template (the image runs llmd, which starts its own sshd with our
// key on port 22). It returns the pod's UUID.
func (q *QuickPod) Rent(ctx context.Context, offer int64, template, pubKeyB64 string) (string, error) {
	body := map[string]any{
		"offers_id": offer, "template_uuid": template, "disk_size": strconv.Itoa(diskGB), "altname": Label,
		"docker_options": "-p 22:22 -e PUBKEY_B64=" + pubKeyB64,
	}
	var r map[string]any
	if err := q.call(ctx, http.MethodPost, "/update/api/createpod", body, &r); err != nil {
		return "", err
	}
	if uuid := firstNonEmpty(str(r["pod_uuid"]), str(sub(r["data"])["pod_uuid"])); uuid != "" {
		return uuid, nil
	}
	// the pod may exist without its UUID in the answer: find it, or it would run (and be billed) unseen until a restart
	pods, err := q.Pods(ctx)
	for _, p := range pods {
		if p.Name == Label && p.Offer == offer && p.UUID != "" {
			return p.UUID, nil
		}
	}
	return "", fmt.Errorf("QuickPod не вернул pod_uuid (%.300s), в списке машин её нет (%v)", str(r), err)
}

func (q *QuickPod) Destroy(ctx context.Context, uuid string) error {
	return q.call(ctx, http.MethodGet, "/update/api/destroypod?pod_uuid="+url.QueryEscape(uuid), nil, nil)
}

// Credit is the account balance in dollars; QuickPod stops every pod when it runs out.
func (q *QuickPod) Credit(ctx context.Context) (float64, error) {
	var u map[string]any
	err := q.call(ctx, http.MethodGet, "/update/api/me", nil, &u)
	return num(u["credit"]), err
}
