package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What the engine and the model need from a machine (the configuration measured on RTX 3090: 21.9 GiB of VRAM,
// 55.6 GiB of container RAM, ~78 GB of model files plus the prepared pack).
const (
	diskGB      = 150
	minRAMMB    = 64000
	minThreads  = 16
	minCUDA     = 13.0 // driver 580+, the engine is built with CUDA 13
	minInetMbps = 500
	// the model is downloaded on every Up, and most hosts charge for traffic: it is part of the price of a machine,
	// and a host where the download alone costs more than ~$0.30 is not taken at all
	modelGB         = 78
	maxInetUSDPerGB = 0.004
)

// Vast is the console.vast.ai REST client.
type Vast struct {
	Key   func() string
	Base  string
	HTTP  *http.Client
	Pause time.Duration // before the 2nd attempt; grows linearly
	Debug func(format string, a ...any)
}

func newVast(key func() string, debug func(string, ...any)) *Vast {
	return &Vast{Key: key, Base: "https://console.vast.ai/api", HTTP: &http.Client{Timeout: 40 * time.Second}, Pause: 3 * time.Second, Debug: debug}
}

// call retries network errors, 5xx and 429 (4 attempts); any other answer is final.
func (v *Vast) call(ctx context.Context, method, path string, in, out any) error {
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
		status, text, err := v.once(ctx, method, path, body)
		switch {
		case err != nil:
			last = err
			v.Debug("vast %s %s ошибка: %v", method, path, err)
		case status/100 == 2:
			v.Debug("vast %s %s -> %d за %d мс, попытка %d", method, path, status, time.Since(started).Milliseconds(), try)
			if out == nil || len(bytes.TrimSpace(text)) == 0 {
				return nil
			}
			return json.Unmarshal(text, out)
		default:
			last = fmt.Errorf("HTTP %d: %.300s", status, text)
			v.Debug("vast %s %s -> %d", method, path, status)
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
		case <-time.After(v.Pause * time.Duration(try)):
		}
	}
}

func (v *Vast) once(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, v.Base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+v.Key())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(resp.Body)
	return resp.StatusCode, text, err
}

// Instance is a rented machine as Vast reports it.
type Instance struct {
	ID       int64   `json:"id"`
	Label    string  `json:"label"`
	Status   string  `json:"actual_status"`
	Intended string  `json:"intended_status"`
	IP       string  `json:"public_ipaddr"`
	Machine  int64   `json:"machine_id"`
	GPU      string  `json:"gpu_name"`
	Geo      string  `json:"geolocation"`
	Dph      float64 `json:"dph_total"`
	Ports    map[string][]struct {
		HostPort string `json:"HostPort"`
	} `json:"ports"`
}

// sshPort is the external port mapped to the container's own sshd, 0 while Vast has not published it.
func (i *Instance) sshPort() int {
	if m := i.Ports["22/tcp"]; len(m) > 0 {
		port, _ := strconv.Atoi(m[0].HostPort)
		return port
	}
	return 0
}

// Instances lists the account's machines. The trailing slash matters: without it Vast answers 301.
func (v *Vast) Instances(ctx context.Context) ([]Instance, error) {
	var r struct {
		Instances []Instance `json:"instances"`
	}
	return r.Instances, v.call(ctx, http.MethodGet, "/v1/instances/", nil, &r)
}

// Instance returns nil when Vast no longer lists the machine.
func (v *Vast) Instance(ctx context.Context, id int64) (*Instance, error) {
	all, err := v.Instances(ctx)
	for i := range all {
		if all[i].ID == id {
			return &all[i], err
		}
	}
	return nil, err
}

// Offer is a machine that can be rented.
type Offer struct {
	ID       int64   `json:"id"`
	Machine  int64   `json:"machine_id"`
	Dph      float64 `json:"dph_total"`
	InetCost float64 `json:"inet_down_cost"`
	Inet     float64 `json:"inet_down"`
	Geo      string  `json:"geolocation"`
	GPU      string  `json:"gpu_name"`
}

type cmp map[string]any

func (v *Vast) Offers(ctx context.Context, gpu string, datacenter bool) ([]Offer, error) {
	q := map[string]any{
		"type": "on-demand", "limit": 200, "order": [][]string{{"dph_total", "asc"}}, "allocated_storage": diskGB,
		"num_gpus": cmp{"eq": 1}, "gpu_name": cmp{"eq": gpu}, "rentable": cmp{"eq": true}, "rented": cmp{"eq": false},
		"verified": cmp{"eq": true}, "cuda_max_good": cmp{"gte": minCUDA}, "disk_space": cmp{"gte": diskGB},
		"cpu_ram": cmp{"gte": minRAMMB}, "cpu_cores_effective": cmp{"gte": minThreads}, "inet_down": cmp{"gte": minInetMbps},
		"direct_port_count": cmp{"gte": 1}, "reliability": cmp{"gte": 0.98},
	}
	if datacenter {
		q["datacenter"] = cmp{"eq": true}
	}
	var r struct {
		Offers []Offer `json:"offers"`
	}
	return r.Offers, v.call(ctx, http.MethodPost, "/v0/bundles/", q, &r)
}

// firstHour is what an Up on this machine costs in its first hour: the rent plus the download of the model.
func (o Offer) firstHour() float64 { return o.Dph + o.InetCost*modelGB }

// usable drops offers with expensive traffic, blacklisted machines and China; the cheapest first hour comes first
// (the faster network wins a tie).
func usable(offers []Offer, bad []int64) (keep []Offer, dropped []string) {
	for _, o := range offers {
		reason := ""
		switch {
		case o.InetCost > maxInetUSDPerGB:
			reason = fmt.Sprintf("дорогой трафик, скачивание модели $%.2f", o.InetCost*modelGB)
		case contains(bad, o.Machine):
			reason = "машина в чёрном списке"
		case strings.Contains(o.Geo, "CN"):
			reason = "Китай"
		}
		if reason == "" {
			keep = append(keep, o)
		} else {
			dropped = append(dropped, fmt.Sprintf("%d (%s, $%.3f/ч): %s", o.ID, o.Geo, o.Dph, reason))
		}
	}
	sort.SliceStable(keep, func(i, j int) bool {
		if a, b := keep[i].firstHour(), keep[j].firstHour(); a != b {
			return a < b
		}
		return keep[i].Inet > keep[j].Inet
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

// Rent accepts an offer. The container runs llmd, which starts its own sshd with our key on the mapped port 22.
func (v *Vast) Rent(ctx context.Context, offer int64, image, pubKeyB64 string) (int64, error) {
	body := map[string]any{
		"image": image, "label": Label, "disk": diskGB, "runtype": "args", "args": []string{"/usr/local/bin/llmd"},
		"target_state": "running", "cancel_unavail": true, "env": "-e PUBKEY_B64=" + pubKeyB64 + " -p 22:22",
	}
	var r struct {
		Success bool  `json:"success"`
		ID      int64 `json:"new_contract"`
	}
	if err := v.call(ctx, http.MethodPut, fmt.Sprintf("/v0/asks/%d/", offer), body, &r); err != nil {
		return 0, err
	}
	if !r.Success || r.ID == 0 {
		return 0, fmt.Errorf("offer not accepted")
	}
	return r.ID, nil
}

func (v *Vast) Destroy(ctx context.Context, id int64) error {
	return v.call(ctx, http.MethodDelete, fmt.Sprintf("/v0/instances/%d/", id), nil, nil)
}

// Credit is the account balance in dollars; Vast stops every machine when it runs out.
func (v *Vast) Credit(ctx context.Context) (float64, error) {
	var u struct {
		Credit  float64 `json:"credit"`
		Balance float64 `json:"balance"`
	}
	err := v.call(ctx, http.MethodGet, "/v0/users/current/", nil, &u)
	return u.Credit + u.Balance, err
}
