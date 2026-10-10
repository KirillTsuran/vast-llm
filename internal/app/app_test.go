package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestReadsFilesOfVastVersions(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"vast_api_key":"k","gpu":"RTX 4090","cheapest_grace_seconds":7,
		"race_two_hosts":false,"local_port":8181,"image":"ghcr.io/kirilltsuran/vast-llm:latest","model":"qwen3.8-27b-uncensored","usd_rub":83.4839}`), 0o600)
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"instance_id":0,"gpu":"","created":"0001-01-01T00:00:00","host_key":"",
		"bad_machines":[143971,147516],"pending":[{"id":5,"machine":9,"gpu":"RTX 3090","geo":"SE","usd_per_hour":0.35,"created":"2026-10-05T13:08:11.1234567Z"}]}`), 0o600)
	c := New(dir)
	var logged []string
	c.OnLog = func(s string) { logged = append(logged, s) }
	c.loadState()
	if hint := c.loadConfig(); !strings.Contains(hint, "quickpod_api_key") {
		t.Errorf("a config of Vast asks for the QuickPod key, got %q", hint)
	}
	cfg := c.Config()
	if cfg.GPU != "RTX 4090" || cfg.Grace != 7 || cfg.Race || cfg.LocalPort != 8181 || cfg.GrafanaPort != 3000 || !cfg.Autostart ||
		!cfg.FastOnly || cfg.WaitFastMin != 30 {
		t.Errorf("config: %+v", cfg)
	}
	rewritten, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	for _, gone := range []string{`"image"`, `"model"`, "vast_api_key", "datacenter_only"} {
		if strings.Contains(string(rewritten), gone) {
			t.Errorf("%s must go away on rewrite: %s", gone, rewritten)
		}
	}
	if c.st.ID != "" || len(c.st.Blocked) != 0 || len(c.st.Pending) != 0 {
		t.Errorf("the machines and blacklist of Vast mean nothing at QuickPod: state %+v", c.st)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "state.json не от этой версии — начинаю с чистого") {
		t.Errorf("a state.json without provider is not read, and the journal says so: %q", logged)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := New(dir)
	c.st = State{ID: "3f2a9c10-aaaa", Machine: 303, Blocked: []Block{{7, time.Now().Add(time.Hour).UTC(), "SSH"}},
		Pending: []Rented{{ID: "b1-x", Machine: 8, Created: now()}}}
	c.saveState()
	raw, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	var written struct {
		Blocked []struct {
			Until string `json:"until"`
		} `json:"blocked"`
	}
	if err := json.Unmarshal(raw, &written); err != nil || len(written.Blocked) != 1 || !strings.HasSuffix(written.Blocked[0].Until, "Z") {
		t.Errorf("times are written as RFC3339 in UTC, with Z: %s (%v)", raw, err)
	}
	again := New(dir)
	again.loadState()
	if again.st.ID != "3f2a9c10-aaaa" || again.st.Provider != provider || len(again.st.Pending) != 1 || again.st.Pending[0].ID != "b1-x" ||
		len(again.st.Blocked) != 1 || again.st.Blocked[0].Machine != 7 || again.st.Blocked[0].Reason != "SSH" {
		t.Errorf("state after a restart: %+v", again.st)
	}
}

func TestBlocksExpire(t *testing.T) {
	c := New(t.TempDir())
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockLocked(1, false, "SSH")
	c.blockLocked(2, true, "процессор без AVX2")
	c.st.Blocked = append(c.st.Blocked, Block{3, time.Now().Add(-time.Minute).UTC(), "old"})
	if got := fmt.Sprint(c.blockedLocked()); got != "[1 2]" {
		t.Errorf("an expired block is dropped: %s", got)
	}
	if len(c.st.Blocked) != 2 || c.st.Blocked[0].Until.Sub(time.Now()) > 4*time.Hour || c.st.Blocked[1].Until.Year() < time.Now().Year()+5 {
		t.Errorf("a failure blocks for hours, a machine that can never run the engine for good: %+v", c.st.Blocked)
	}
}

func TestBrokenStateIsReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"pod_uuid":`), 0o600)
	c := New(dir)
	var logged []string
	c.OnLog = func(s string) { logged = append(logged, s) }
	c.loadState()
	if len(logged) != 1 || !strings.Contains(logged[0], "state.json не читается") || c.st.ID != "" {
		t.Errorf("logged %q, state %+v", logged, c.st)
	}
}

func TestFirstStartCreatesConfigAndKey(t *testing.T) {
	dir := t.TempDir()
	c := New(dir)
	if hint := c.loadConfig(); !strings.Contains(hint, "создан config.json") {
		t.Errorf("hint %q", hint)
	}
	_, pub, err := loadKey(filepath.Join(dir, "ssh_key.pem"))
	if err != nil || !strings.HasPrefix(pub, "ecdsa-sha2-nistp256 ") || !strings.HasSuffix(pub, " "+Label) {
		t.Fatalf("pub %q err %v", pub, err)
	}
	_, again, _ := loadKey(filepath.Join(dir, "ssh_key.pem"))
	if again != pub {
		t.Error("the key must be created once and then reused")
	}
}

// the app shows the model name that the image serves
func TestModelMatchesImage(t *testing.T) {
	raw, err := os.ReadFile("../../image/model.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct{ Name string }
	if err := json.Unmarshal(raw, &m); err != nil || m.Name != Model {
		t.Errorf("image/model.json says %q (%v), the app says %q", m.Name, err, Model)
	}
}

func TestUsableOffers(t *testing.T) {
	base := Offer{GPU: "RTX 3090", GPUs: 1, RAM: 62, Threads: 16, PCIe: "3", Lanes: "16", Inet: 900, FreeDisk: 800, Reliability: 97}
	with := func(id int64, f func(o *Offer)) Offer {
		o := base
		o.ID, o.Machine = id, id+100
		f(&o)
		return o
	}
	keep, dropped := usable([]Offer{
		with(1, func(o *Offer) { o.Dph = 0.18 }),
		with(2, func(o *Offer) { o.Dph = 0.18; o.RAM, o.Threads, o.PCIe = 125, 24, "4" }), // the ~110 tok/s configuration
		with(3, func(o *Offer) { o.Dph = 0.15; o.Threads = 20 }),
		with(4, func(o *Offer) { o.Dph = 0.15; o.Threads = 24 }),
		with(5, func(o *Offer) { o.Dph = 0.10; o.RAM = 31 }),
		with(6, func(o *Offer) { o.Dph = 0.10; o.Threads = 8 }),
		with(7, func(o *Offer) { o.Dph = 0.10; o.Inet = 61 }),
		with(8, func(o *Offer) { o.Dph = 0.10; o.Reliability = 90 }),
		with(9, func(o *Offer) { o.Dph = 0.10; o.FreeDisk = 100 }),
		with(10, func(o *Offer) { o.Dph = 0.10; o.Machine = 99 }),
		with(11, func(o *Offer) { o.Dph = 0.12; o.GPU = "RTX 3090 Ti" }), // the same VRAM, faster: taken for an RTX 3090
		with(12, func(o *Offer) { o.Dph = 0.05; o.GPUs = 2 }),
		with(13, func(o *Offer) { o.Dph = 0.05; o.Occupied = true }),
		with(14, func(o *Offer) {
			o.Dph = 0.18
			o.RAM, o.Threads = 125, 24
			o.CPU = "Intel(R) Xeon(R) CPU E5-2630 v2 @ 2.60GHz"
		}),
		with(15, func(o *Offer) { o.Dph = 0.05; o.Lanes = "4" }),
		with(16, func(o *Offer) { o.Dph = 0.05; o.GPU = "RTX 4090" }),
	}, "RTX 3090", []int64{99})
	var got []int64
	for _, o := range keep {
		got = append(got, o.ID)
	}
	if fmt.Sprint(got) != "[2 11 4 3 1]" {
		t.Errorf("the fast configuration first, then the cheapest, then more cores: %v", got)
	}
	all := strings.Join(dropped, "|")
	for _, why := range []string{"без AVX2", "мало RAM", "мало ядер", "медленная сеть", "надёжность", "мало диска", "чёрном списке", "PCIe x4"} {
		if !strings.Contains(all, why) {
			t.Errorf("no offer dropped for %q: %v", why, dropped)
		}
	}
	if !(Offer{RAM: 125, Threads: 24, PCIe: "4.0", Lanes: "x16"}).fast() || (Offer{RAM: 125, Threads: 24, PCIe: "3", Lanes: "16"}).fast() {
		t.Error(`"4.0" and "x16" are PCIe 4.0 x16; PCIe 3 is not the fast configuration`)
	}
}

// the CPU names QuickPod listed on 2026-10-11, and the spellings other hosts use
func TestLacksAVX2(t *testing.T) {
	for cpu, old := range map[string]bool{
		"Intel(R) Xeon(R) CPU E5-2630 v2 @ 2.60GHz": true, "Intel(R) Xeon(R) CPU E5-2670 v2 @ 2.50GHz": true,
		"Intel(R) Xeon(R) CPU E5-2670 0 @ 2.60GHz": true, "Intel(R) Xeon(R) CPU E5-2680 V2 @ 2.80GHz": true,
		"Intel Xeon E5-2630v2": true, "Intel(R) Xeon(R) CPU E5-2697 v2 2.70GHz": true, "Intel Xeon E5-2630 v2 (12) @ 2.6GHz": true,
		"Intel(R) Xeon(R) CPU E7- 4870 @ 2.40GHz": true, "Intel(R) Xeon(R) CPU E3-1230 V2 @ 3.30GHz": true, "Intel(R) Xeon(R) CPU E3-1230 @ 3.2GHz": true,
		"Intel(R) Core(TM) i7-3770 CPU @ 3.40GHz": true, "Intel(R) Core(TM) i7-4930K CPU @ 3.40GHz": true, "Intel(R) Pentium(R) Gold G5400": true,
		"Intel(R) Xeon(R) CPU X5670 @ 2.93GHz": true, "AMD FX(tm)-8350 Eight-Core Processor": true, "AMD Phenom(tm) II X6 1090T": true,
		"Intel(R) Xeon(R) CPU E5-2697 v4 @ 2.30GHz": false, "Intel(R) Xeon(R) CPU E5-2620 v3 @ 2.40GHz": false, "Intel Xeon E5-2697v4": false,
		"Intel(R) Xeon(R) CPU E5-2697A v4 @ 2.60GHz": false, "Intel(R) Xeon(R) CPU E3-1230 v3 @ 3.30GHz": false,
		"AMD Ryzen 9 5900X 12-Core Processor": false, "AMD Ryzen 9 5950X 16-Core Processor": false, "AMD EPYC 9B14 96-Core Processor": false,
		"Intel(R) Core(TM) i5-10400F CPU @ 2.90GHz": false, "Intel(R) Core(TM) i7-6700 CPU @ 3.40GHz": false, "Intel(R) Core(TM) i7-4790K CPU @ 4.00GHz": false,
		"Intel(R) Xeon(R) W-2223 CPU @ 3.60GHz": false, "Intel(R) Xeon(R) Gold 6240R CPU @ 2.40GHz": false, "11th Gen Intel(R) Core(TM) i7-11700 @ 2.50GHz": false,
		"AMD Ryzen 7 2700 Eight-Core Processor": false, "Intel(R) Core(TM) i3-8100 CPU @ 3.60GHz": false, "13th Gen Intel(R) Core(TM) i9-13900K": false,
	} {
		if lacksAVX2(cpu) != old {
			t.Errorf("%q: without AVX2 = %v, want %v", cpu, !old, old)
		}
	}
}

func TestSSHPort(t *testing.T) {
	for ports, want := range map[string]int{
		// port_mappings and Ports of a real QuickPod pod (2026-10-10), as podFrom joins them
		`22 -> <a target="_new" href="http://114.23.254.176:57450">114.23.254.176:57450</a> | ` +
			"0.0.0.0:57450->22/tcp, 0.0.0.0:57451->22/tcp, [::]:57450->22/tcp": 57450,
		"0.0.0.0:40022->22/tcp, :::40022->22/tcp, 0.0.0.0:40023->8080/tcp": 40022,
		`{"22/tcp":[{"HostIp":"0.0.0.0","HostPort":"41022"}]}`:             41022,
		`{"22": 42022, "8888": 42023}`:                                     42022,
		"22:43022,8888:43023":                                              43022,
		"22:22":                                                            0, // not mapped yet
		"":                                                                 0,
	} {
		if got := (Pod{Ports: ports}).sshPort(); got != want {
			t.Errorf("%q: port %d, want %d", ports, got, want)
		}
	}
}

func testQuickPod(h http.HandlerFunc) (*QuickPod, *httptest.Server) {
	srv := httptest.NewServer(h)
	q := newQuickPod(func() string { return "qpk_secret" }, func(string, ...any) {})
	q.Base, q.Pause = srv.URL, time.Millisecond
	return q, srv
}

func TestQuickPodRetriesOnlyServerErrors(t *testing.T) {
	calls := 0
	q, srv := testQuickPod(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-API-Key") != "qpk_secret" || r.URL.Path != "/update/api/me" {
			t.Errorf("%s, key %q", r.URL.Path, r.Header.Get("X-API-Key"))
		}
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"credit":10,"credit_limit":500}`)
	})
	defer srv.Close()
	if credit, err := q.Credit(context.Background()); err != nil || credit != 10 || calls != 3 {
		t.Errorf("a GET is retried: credit %v err %v calls %d", credit, err, calls)
	}

	creates := 0
	q, srv2 := testQuickPod(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/update/api/createpod" {
			creates++
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `[]`)
	})
	defer srv2.Close()
	if _, err := q.Rent(context.Background(), 7, "tpl", "cHVi", "vast-llm-1"); err == nil || creates != 1 {
		t.Errorf("a createpod is never sent twice (a second machine would run unseen): err %v, sent %d", err, creates)
	}

	q, srv3 := testQuickPod(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/update/api/createpod" {
			http.Error(w, `{"error":"offer is busy"}`, http.StatusNotAcceptable)
			return
		}
		io.WriteString(w, `[]`)
	})
	defer srv3.Close()
	var refused *HTTPError
	if _, err := q.Rent(context.Background(), 7, "tpl", "cHVi", "vast-llm-1"); !errors.As(err, &refused) || refused.Status != 406 {
		t.Errorf("a refusal comes back as it is: %v", err)
	}

	calls = 0
	q, srv4 := testQuickPod(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) })
	defer srv4.Close()
	if _, err := q.Pods(context.Background()); err == nil || calls != 4 {
		t.Errorf("4 attempts, then the error: err %v calls %d", err, calls)
	}
}

func TestRentRequest(t *testing.T) {
	q, srv := testQuickPod(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.Method != http.MethodPost || r.URL.Path != "/update/api/createpod" || body["offers_id"] != float64(7) || body["template_uuid"] != "tpl" ||
			body["disk_size"] != strconv.Itoa(diskGB) || body["docker_options"] != "-p 22:22 -e PUBKEY_B64=cHVi" || body["altname"] != "vast-llm-ab12" {
			t.Errorf("%s %s %v", r.Method, r.URL.Path, body)
		}
		io.WriteString(w, `{"status":"success","pod_uuid":"9b1c-42"}`)
	})
	defer srv.Close()
	if id, err := q.Rent(context.Background(), 7, "tpl", "cHVi", "vast-llm-ab12"); id != "9b1c-42" || err != nil {
		t.Errorf("id %q err %v", id, err)
	}

	// the answer was lost (a timeout, a 502) but QuickPod created the machine: it is found by this rent's own name
	for _, answer := range []int{http.StatusOK, http.StatusBadGateway} {
		q, srv2 := testQuickPod(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/update/api/createpod":
				w.WriteHeader(answer)
				io.WriteString(w, `{"status":"success","message":"Pod created"}`)
			case "/update/api/gpu_pods":
				io.WriteString(w, `[{"Names":"other","altname":"vast-llm-zz99","offers_id":7},{"Names":"mine","altname":"vast-llm-ab12","offers_id":7}]`)
			}
		})
		if id, err := q.Rent(context.Background(), 7, "tpl", "cHVi", "vast-llm-ab12"); id != "mine" || err != nil {
			t.Errorf("HTTP %d: found by its name: id %q err %v", answer, id, err)
		}
		srv2.Close()
	}
}

// gpu_pods as the real API answers it (2026-10-10): no pod_uuid, the container's name is the UUID
func TestOffersAndPodsParse(t *testing.T) {
	q, srv := testQuickPod(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rentable":
			io.WriteString(w, `[{"id":7151,"machines_id":303,"gpu_type":"NVIDIA GeForce RTX 3090","hourly_cost":0.18,"memory":125,"cpus":24,
				"gpu_pcie":"4","gpu_lanes":"16","num_gpus":1,"occupied":false,"_machines":{"cpu_name":"AMD Ryzen 9 5900X 12-Core Processor ",
				"geolocation":"US","inet_down":909.49,"avail_disk_space":3288,"reliability":97.6,"verification":true}}]`)
		case "/update/api/gpu_pods":
			io.WriteString(w, `[{"id":3501268,"Names":"aa-1","altname":"vast-llm-1f2e","State":"running","public_ipaddr":"1.2.3.4","machines_id":303,
				"offers_id":7151,"hourly_cost":"0.2","Ports":"0.0.0.0:40022->22/tcp, [::]:40022->22/tcp",
				"port_mappings":"22 -> <a target=\"_new\" href=\"http://1.2.3.4:40022\">1.2.3.4:40022</a> |","_offers":{"gpu_type":"NVIDIA GeForce RTX 3090"},
				"_machines":{"geolocation":"US"}},{"Names":["/bb-2"],"altname":"vast-llm","destroyed":false,"State":"exited","intended_state":"stopped",
				"port_mappings":"22 -> <a href=\"http://5.6.7.8:41022\">5.6.7.8:41022</a> |"},{"Names":"cc-3","altname":"vast-llm","destroyed":true}]`)
		}
	})
	defer srv.Close()
	offers, err := q.Offers(context.Background())
	if err != nil || len(offers) != 1 {
		t.Fatalf("offers %v err %v", offers, err)
	}
	if o := offers[0]; o.GPU != "RTX 3090" || o.RAM != 125 || o.Threads != 24 || !o.fast() || o.CPU != "AMD Ryzen 9 5900X 12-Core Processor" || !o.Verified || o.Inet < 909 {
		t.Errorf("offer %+v", o)
	}
	pods, err := q.Pods(context.Background())
	if err != nil || len(pods) != 2 {
		t.Fatalf("a destroyed pod is not listed: %v %v", pods, err)
	}
	if p := pods[0]; p.UUID != "aa-1" || !p.ours() || !p.running() || p.sshPort() != 40022 || p.Dph != 0.2 || p.GPU != "RTX 3090" || p.Machine != 303 || p.Offer != 7151 {
		t.Errorf("pod %+v", p)
	}
	if p := pods[1]; p.UUID != "bb-2" || !p.ours() || !p.stopped() || p.IP != "5.6.7.8" || p.sshPort() != 41022 {
		t.Errorf("a list of names, the IP from port_mappings, a pod stopped at zero credit: %+v", p)
	}
}

func TestPickWinner(t *testing.T) {
	best, second := Rented{ID: "best", Dph: 0.18}, Rented{ID: "second", Dph: 0.30}
	boom := errors.New("boom")
	cases := []struct {
		name   string
		feed   func(ch chan<- dialed)
		grace  time.Duration
		winner string
		lost   int
	}{
		{"best first wins at once", func(ch chan<- dialed) { ch <- dialed{r: best} }, time.Hour, "best", 0},
		{"second first, best inside the grace", func(ch chan<- dialed) { ch <- dialed{r: second}; ch <- dialed{r: best} }, time.Hour, "best", 1},
		{"second first, best too slow", func(ch chan<- dialed) { ch <- dialed{r: second} }, 20 * time.Millisecond, "second", 0},
		{"second first, best fails inside the grace", func(ch chan<- dialed) { ch <- dialed{r: second}; ch <- dialed{r: best, err: boom} }, time.Hour, "second", 1},
		{"best fails, second wins without waiting", func(ch chan<- dialed) { ch <- dialed{r: best, err: boom}; ch <- dialed{r: second} }, time.Hour, "second", 1},
		{"both fail", func(ch chan<- dialed) { ch <- dialed{r: best, err: boom}; ch <- dialed{r: second, err: boom} }, time.Hour, "", 2},
	}
	for _, tc := range cases {
		ch := make(chan dialed, 2)
		tc.feed(ch)
		win, lost := pickWinner(context.Background(), []Rented{best, second}, ch, tc.grace, func(string) {})
		got := ""
		if win != nil {
			got = win.r.ID
		}
		if got != tc.winner || len(lost) != tc.lost {
			t.Errorf("%s: winner %q (want %q), lost %d (want %d)", tc.name, got, tc.winner, len(lost), tc.lost)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if win, _ := pickWinner(ctx, []Rented{best}, make(chan dialed), time.Hour, func(string) {}); win != nil {
		t.Error("a cancelled Up has no winner")
	}
}

// sshd accepts only `allowed`, answers exec through `run` and sends every forwarded connection to `target`.
func sshd(t *testing.T, allowed ssh.PublicKey, target string, run func(cmd string) string) (addr string) {
	_, hostKey, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostKey)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), allowed.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("denied")
	}}
	cfg.AddHostKey(hostSigner)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go func() {
					for r := range reqs {
						r.Reply(true, nil)
					}
				}()
				for nc := range chans {
					ch, rq, _ := nc.Accept()
					if nc.ChannelType() == "direct-tcpip" {
						go ssh.DiscardRequests(rq)
						go func() {
							defer ch.Close()
							remote, err := net.Dial("tcp", target)
							if err != nil {
								return
							}
							defer remote.Close()
							go io.Copy(remote, ch)
							io.Copy(ch, remote)
						}()
						continue
					}
					go func() {
						for r := range rq {
							var p struct{ Cmd string }
							ssh.Unmarshal(r.Payload, &p)
							r.Reply(true, nil)
							io.WriteString(ch, run(p.Cmd))
							ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							ch.Close()
						}
					}()
				}
			}()
		}
	}()
	return l.Addr().String()
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestTunnel(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "engine says hi") }))
	defer engine.Close()
	signer, _, err := loadKey(filepath.Join(t.TempDir(), "ssh_key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	addr := sshd(t, signer.PublicKey(), strings.TrimPrefix(engine.URL, "http://"), func(cmd string) string { return "ran: " + cmd + "\n" })

	client, hostKey, err := dialSSH(context.Background(), addr, signer, "")
	if err != nil || hostKey == "" || strings.HasPrefix(hostKey, "SHA256:") {
		t.Fatalf("first connect pins the key: %q %v", hostKey, err)
	}
	if _, _, err := dialSSH(context.Background(), addr, signer, "someone-else"); err == nil || !strings.Contains(err.Error(), "изменился") {
		t.Errorf("a different host key must be refused, got %v", err)
	}
	stranger, _, _ := loadKey(filepath.Join(t.TempDir(), "ssh_key.pem"))
	if _, _, err := dialSSH(context.Background(), addr, stranger, hostKey); err == nil {
		t.Error("a key the machine does not know must be refused")
	}

	dropped := make(chan error, 1)
	tun := newTunnel(client, func(err error) { dropped <- err })
	port := freePort(t)
	if err := tun.forward(port, 8080, true); err != nil {
		t.Fatal(err)
	}
	if err := tun.forward(port, 8080, true); err == nil {
		t.Error("a busy local port must be an error")
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "engine says hi" || tun.requests.Load() != 1 {
		t.Errorf("body %q, counted %d", body, tun.requests.Load())
	}
	if out, err := tun.exec("cat /opt/llm/state"); out != "ran: cat /opt/llm/state" || err != nil {
		t.Errorf("exec: %q %v", out, err)
	}

	client.Conn.Close() // the machine goes away
	select {
	case <-dropped:
	case <-time.After(5 * time.Second):
		t.Fatal("a dead session must be reported")
	}
	if tun.alive() {
		t.Error("tunnel must not be alive after the session ended")
	}
	tun.close()
}

// fakeQuickPod answers like the real API: gpu_pods name a pod by "Names" (no pod_uuid), createpod returns the UUID.
type fakeQuickPod struct {
	mu        sync.Mutex
	sshPort   string
	next      int
	pods      map[string]map[string]any
	rented    []map[string]any
	deleted   []string
	slowOnly  int // the first this many /rentable answers list no fast machine
	rentables int
}

func (f *fakeQuickPod) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	machine := func(cpu string) string {
		return fmt.Sprintf(`{"cpu_name":%q,"geolocation":"US","inet_down":900,"avail_disk_space":900,"reliability":97,"verification":true}`, cpu)
	}
	switch {
	case r.URL.Path == "/update/api/me":
		io.WriteString(w, `{"credit":10}`)
	case r.URL.Path == "/rate":
		io.WriteString(w, `<ValCurs><Valute><CharCode>EUR</CharCode><Value>97,10</Value></Valute><Valute><CharCode>USD</CharCode><Value>83,4839</Value></Valute></ValCurs>`)
	case r.URL.Path == "/rentable":
		f.rentables++
		slow := fmt.Sprintf(`{"id":103,"machines_id":3,"gpu_type":"NVIDIA GeForce RTX 3090","hourly_cost":0.10,"memory":62,"cpus":16,"gpu_pcie":"3","gpu_lanes":"16","num_gpus":1,"_machines":%s}`, machine("i7-7700"))
		if f.rentables <= f.slowOnly {
			fmt.Fprintf(w, `[%s]`, slow)
			return
		}
		fmt.Fprintf(w, `[{"id":101,"machines_id":1,"gpu_type":"NVIDIA GeForce RTX 3090","hourly_cost":0.15,"memory":125,"cpus":24,"gpu_pcie":"4","gpu_lanes":"16","num_gpus":1,"_machines":%s},
			{"id":102,"machines_id":2,"gpu_type":"NVIDIA GeForce RTX 3090","hourly_cost":0.18,"memory":125,"cpus":24,"gpu_pcie":"4","gpu_lanes":"16","num_gpus":1,"_machines":%s},
			%s]`, machine("AMD Ryzen 9 5950X"), machine("AMD Ryzen 9 5900X"), slow)
	case r.URL.Path == "/update/api/createpod":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.rented = append(f.rented, body)
		f.next++
		uuid := fmt.Sprintf("pod%d-%d", f.next, int(body["offers_id"].(float64)))
		f.pods[uuid] = map[string]any{"Names": uuid, "altname": body["altname"], "State": "running", "public_ipaddr": "127.0.0.1",
			"machines_id": body["offers_id"].(float64) - 100, "offers_id": body["offers_id"], "hourly_cost": 0.18, "Ports": "0.0.0.0:" + f.sshPort + "->22/tcp",
			"_offers": map[string]any{"gpu_type": "NVIDIA GeForce RTX 3090"}, "_machines": map[string]any{"geolocation": "US"}}
		fmt.Fprintf(w, `{"status":"success","pod_uuid":%q}`, uuid)
	case r.URL.Path == "/update/api/gpu_pods":
		list := []map[string]any{}
		for _, p := range f.pods {
			list = append(list, p)
		}
		json.NewEncoder(w).Encode(list)
	case r.URL.Path == "/update/api/destroypod":
		uuid := r.URL.Query().Get("pod_uuid")
		if _, ok := f.pods[uuid]; ok {
			delete(f.pods, uuid)
			f.deleted = append(f.deleted, uuid)
		}
		io.WriteString(w, `{"message":"Pod destroyed"}`)
	default:
		http.NotFound(w, r)
	}
}

// testRig: a config, a fake QuickPod, an sshd whose `cat /opt/llm/state` answers come from states (the last repeats).
type testRig struct {
	dir     string
	apiPort int
	fake    *fakeQuickPod
	srv     *httptest.Server
	pub     string
}

func newRig(t *testing.T, config string, states ...string) *testRig {
	dir := t.TempDir()
	apiPort, grafanaPort := freePort(t), freePort(t)
	os.WriteFile(filepath.Join(dir, "config.json"), fmt.Appendf(nil, `{"quickpod_api_key":"k","quickpod_template_uuid":"tpl-1",%s
		"cheapest_grace_seconds":1,"local_port":%d,"grafana_port":%d,"autostart":false}`, config, apiPort, grafanaPort), 0o600)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "pong") }))
	t.Cleanup(engine.Close)
	signer, pub, _ := loadKey(filepath.Join(dir, "ssh_key.pem"))
	var mu sync.Mutex
	addr := sshd(t, signer.PublicKey(), strings.TrimPrefix(engine.URL, "http://"), func(cmd string) string {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(cmd, "cat /opt/llm/state") {
			s := states[0]
			if len(states) > 1 {
				states = states[1:]
			}
			return s + "\n"
		}
		return "RTX 3090, 350.00 W\n"
	})
	_, sshPort, _ := net.SplitHostPort(addr)
	fake := &fakeQuickPod{sshPort: sshPort, pods: map[string]map[string]any{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return &testRig{dir, apiPort, fake, srv, pub}
}

func (rig *testRig) core() *Core {
	c := New(rig.dir)
	c.qp.Base, c.qp.Pause, c.poll, c.waitPoll, c.rates = rig.srv.URL, time.Millisecond, 10*time.Millisecond, 10*time.Millisecond, rig.srv.URL+"/rate"
	return c
}

func TestUpAndDown(t *testing.T) {
	rig := newRig(t, `"race_two_hosts":true,`, "downloading 10 из 78 ГБ", "ready")
	c := rig.core()
	var phases []string
	var mu sync.Mutex
	c.OnChange = func() {
		v := c.View()
		mu.Lock()
		if len(phases) == 0 || phases[len(phases)-1] != v.Phase {
			phases = append(phases, v.Phase)
		}
		mu.Unlock()
	}
	c.Startup()
	c.tasks.Wait()
	if rate := c.Config().UsdRub; rate != 83.4839 {
		t.Errorf("the dollar rate comes from the bank's XML, got %v", rate)
	}
	if v := c.View(); v.Phase != "off" || len(v.Machines) != 0 {
		t.Fatalf("after Startup with nothing rented: %+v", v)
	}

	c.Up()
	v := c.View()
	if v.Phase != "ready" || !v.Tunnel || len(v.Machines) != 1 || !v.Machines[0].Main {
		t.Fatalf("after Up: %+v", v)
	}
	if v.Machines[0].Machine != 1 {
		t.Errorf("the cheaper of the two fast machines (1) must win inside the grace, got machine %d", v.Machines[0].Machine)
	}
	rig.fake.mu.Lock()
	if len(rig.fake.rented) != 2 || rig.fake.rented[0]["offers_id"] != float64(101) || rig.fake.rented[1]["offers_id"] != float64(102) {
		t.Errorf("the race rents the 2 fast RTX 3090 (the slow one never), rented %v", rig.fake.rented)
	}
	wantOpts := "-p 22:22 -e PUBKEY_B64=" + base64.StdEncoding.EncodeToString([]byte(rig.pub))
	name, _ := rig.fake.rented[0]["altname"].(string)
	if rig.fake.rented[0]["docker_options"] != wantOpts || rig.fake.rented[0]["template_uuid"] != "tpl-1" || !strings.HasPrefix(name, Label+"-") ||
		rig.fake.rented[1]["altname"] == name {
		t.Errorf("rent body: %v; every rent has its own name", rig.fake.rented)
	}
	rig.fake.mu.Unlock()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", rig.apiPort))
	if err != nil {
		t.Fatalf("the API must answer through the tunnel: %v", err)
	}
	resp.Body.Close()

	// the race loser is deleted in the background and leaves state.json
	deadline := time.Now().Add(5 * time.Second)
	for len(c.View().Machines) != 1 || func() bool { rig.fake.mu.Lock(); defer rig.fake.mu.Unlock(); return len(rig.fake.deleted) != 1 }() {
		if time.Now().After(deadline) {
			t.Fatalf("loser not deleted: %+v", c.View().Machines)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// a restart of the program finds the machine in state.json and reconnects
	c.closeTunnel()
	again := rig.core()
	again.Startup()
	if v := again.View(); v.Phase != "ready" || !v.Tunnel || v.Machines[0].ID != c.View().Machines[0].ID {
		t.Fatalf("after restart: %+v", v)
	}

	again.Down("тест")
	if v := again.View(); v.Phase != "off" || len(v.Machines) != 0 || v.Tunnel {
		t.Fatalf("after Down: %+v", v)
	}
	rig.fake.mu.Lock()
	if len(rig.fake.pods) != 0 {
		t.Errorf("machines left at QuickPod: %v", rig.fake.pods)
	}
	rig.fake.mu.Unlock()
	c.tasks.Wait()
	again.tasks.Wait()
	mu.Lock()
	if got := strings.Join(phases, " "); got != "off renting booting downloading ready" {
		t.Errorf("phases of the first Up: %s", got)
	}
	mu.Unlock()
}

// llmd says the machine cannot run the engine: Up deletes it at once (it is billed), never rents it again, takes the next
func TestFailedMachineIsReplaced(t *testing.T) {
	rig := newRig(t, "", "failed процессор без AVX2", "ready")
	c := rig.core()
	c.Startup()
	c.Up()
	v := c.View()
	if v.Phase != "ready" || len(v.Machines) != 1 || v.Machines[0].Machine != 2 {
		t.Fatalf("the next fast machine (2) must be up: %+v", v)
	}
	rig.fake.mu.Lock()
	if len(rig.fake.deleted) != 1 || !strings.HasSuffix(rig.fake.deleted[0], "-101") || len(rig.fake.pods) != 1 {
		t.Errorf("the failed machine must be deleted at once: deleted %v, left %v", rig.fake.deleted, rig.fake.pods)
	}
	rig.fake.mu.Unlock()
	c.mu.Lock()
	if len(c.st.Blocked) != 1 || c.st.Blocked[0].Machine != 1 || c.st.Blocked[0].Until.Year() < time.Now().Year()+5 {
		t.Errorf("a machine without AVX2 is never rented again: %+v", c.st.Blocked)
	}
	c.mu.Unlock()
	c.Down("тест")
	c.tasks.Wait()
}

// a machine of this program that state.json does not know (a rent whose answer was lost) is deleted by Down too
func TestDownDeletesUnseenMachines(t *testing.T) {
	rig := newRig(t, "", "ready")
	rig.fake.pods["lost-1"] = map[string]any{"Names": "lost-1", "altname": Label + "-dead", "State": "running"}
	rig.fake.pods["someone-else"] = map[string]any{"Names": "someone-else", "altname": "my-notebook", "State": "running"}
	c := rig.core()
	c.loadConfig()
	c.Down("тест")
	rig.fake.mu.Lock()
	defer rig.fake.mu.Unlock()
	if _, ok := rig.fake.pods["lost-1"]; ok || len(rig.fake.pods) != 1 || c.View().Phase != "off" {
		t.Errorf("the unseen machine must go, another program's must stay: %v, phase %s", rig.fake.pods, c.View().Phase)
	}
}

// fast_only: no fast machine yet - Up waits (renting nothing) and takes the fast one when it appears; past
// wait_fast_minutes it stops and says what is free
func TestFastOnlyWaits(t *testing.T) {
	rig := newRig(t, `"wait_fast_minutes":1,`, "ready")
	rig.fake.slowOnly = 3
	c := rig.core()
	c.loadConfig()
	offers, err := c.pickOffers(context.Background(), c.Config())
	if err != nil || len(offers) != 2 || !offers[0].fast() || rig.fake.rentables != 4 {
		t.Errorf("waited for the fast machines: %v, err %v, looked %d times", offers, err, rig.fake.rentables)
	}
	if c.View().Phase != "waiting" {
		t.Errorf("the window shows the wait: %s", c.View().Phase)
	}
	rig.fake.mu.Lock()
	rig.fake.rentables, rig.fake.slowOnly = 0, 100
	rig.fake.mu.Unlock()
	cfg := c.Config()
	cfg.WaitFastMin = 0
	if _, err := c.pickOffers(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "fast_only") || !strings.Contains(err.Error(), "i7-7700") {
		t.Errorf("past the wait: what is free and how to take it, got %v", err)
	}
	cfg.FastOnly = false
	if offers, err := c.pickOffers(context.Background(), cfg); err != nil || len(offers) != 1 || offers[0].fast() {
		t.Errorf("without fast_only the slow machine is taken: %v %v", offers, err)
	}
	if len(rig.fake.rented) != 0 {
		t.Error("waiting rents nothing")
	}
}

// reconcile reads the QuickPod key, which takes c.mu itself, so it must not read the key while it holds the lock
func TestReconcileDoesNotDeadlock(t *testing.T) {
	rig := newRig(t, "", "ready")
	c := rig.core()
	c.loadConfig() // the key of config.json goes into cfg
	done := make(chan struct{})
	go func() {
		c.reconcile()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile does not return: it asks for the key while it holds c.mu")
	}
}

// the Bank of Russia answers with an error: the last known rate stays, and the journal says so
func TestRateKeepsLastKnownOnFailure(t *testing.T) {
	bank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer bank.Close()
	c := New(t.TempDir())
	c.rates = bank.URL
	var logged []string
	c.OnLog = func(s string) { logged = append(logged, s) }
	c.RefreshRate()
	if c.Config().UsdRub != defaultConfig().UsdRub || len(logged) != 1 || !strings.Contains(logged[0], "курс ЦБ недоступен") {
		t.Errorf("a failed rate keeps the last known one and says so: rate %v, log %q", c.Config().UsdRub, logged)
	}
}
