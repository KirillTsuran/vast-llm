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

// ---------- files written by VastLLM 1.x stay readable ----------

func TestReadsFilesOfVersion1(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"vast_api_key":"k","gpu":"RTX 4090","cheapest_grace_seconds":7,
		"race_two_hosts":false,"local_port":8181,"image":"ghcr.io/kirilltsuran/vast-llm:latest","model":"qwen3.8-27b-uncensored","usd_rub":83.4839}`), 0o600)
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"instance_id":0,"gpu":"","created":"0001-01-01T00:00:00","host_key":"",
		"bad_machines":[143971,147516],"pending":[{"id":5,"machine":9,"gpu":"RTX 3090","geo":"SE","usd_per_hour":0.35,"created":"2026-10-05T13:08:11.1234567Z"}]}`), 0o600)
	c := New(dir)
	c.loadState()
	if hint := c.loadConfig(); hint != "" {
		t.Fatalf("config with a key must load clean, got %q", hint)
	}
	cfg := c.Config()
	if cfg.GPU != "RTX 4090" || cfg.Grace != 7 || cfg.Race || cfg.LocalPort != 8181 || cfg.GrafanaPort != 3000 || !cfg.Autostart {
		t.Errorf("config: %+v", cfg)
	}
	rewritten, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if strings.Contains(string(rewritten), "image") || strings.Contains(string(rewritten), `"model"`) {
		t.Errorf("keys of version 1 must go away on rewrite: %s", rewritten)
	}
	if len(c.st.BadMachines) != 2 || len(c.st.Pending) != 1 || c.st.Pending[0].Created.Year() != 2026 || !c.st.Created.IsZero() {
		t.Errorf("state: %+v", c.st)
	}
}

func TestBrokenStateIsReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"instance_id":`), 0o600)
	c := New(dir)
	var logged []string
	c.OnLog = func(s string) { logged = append(logged, s) }
	c.loadState()
	if len(logged) != 1 || !strings.Contains(logged[0], "state.json не читается") || c.st.ID != 0 {
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

// ---------- offers ----------

func TestUsableOffers(t *testing.T) {
	keep, dropped := usable([]Offer{
		{ID: 1, Machine: 10, Dph: 0.40, Inet: 6862, Geo: "The Netherlands, NL"},
		{ID: 2, Machine: 11, Dph: 0.235, InetCost: 0.0026, Inet: 4186, Geo: "Ontario, CA"}, // cheaper rent, but $0.20 of traffic: 0.438
		{ID: 3, Machine: 12, Dph: 0.40, Inet: 900, Geo: "Spain, ES"},
		{ID: 4, Machine: 13, Dph: 0.308, InetCost: 0.052, Geo: "Oman, OM"}, // the download alone is $4
		{ID: 5, Machine: 14, Dph: 0.10, Geo: "Sichuan, CN"},
		{ID: 6, Machine: 99, Dph: 0.10, Geo: "US"},
	}, []int64{99})
	if got := fmt.Sprint(len(keep), keep[0].ID, keep[1].ID, keep[2].ID); got != "3 1 3 2" {
		t.Errorf("cheapest first hour first, faster network wins a tie: %v", keep)
	}
	if len(dropped) != 3 || !strings.Contains(dropped[0], "дорогой трафик") {
		t.Errorf("dropped: %v", dropped)
	}
}

// ---------- Vast API ----------

func testVast(h http.HandlerFunc) (*Vast, *httptest.Server) {
	srv := httptest.NewServer(h)
	v := newVast(func() string { return "secret" }, func(string, ...any) {})
	v.Base, v.Pause = srv.URL, time.Millisecond
	return v, srv
}

func TestVastRetriesOnlyServerErrors(t *testing.T) {
	calls := 0
	v, srv := testVast(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"credit":1.5,"balance":0.25}`)
	})
	defer srv.Close()
	if credit, err := v.Credit(context.Background()); err != nil || credit != 1.75 || calls != 3 {
		t.Errorf("credit %v err %v calls %d", credit, err, calls)
	}

	calls = 0
	v, srv2 := testVast(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "no such offer", http.StatusBadRequest)
	})
	defer srv2.Close()
	if _, err := v.Rent(context.Background(), 7, "img", "cHVi"); err == nil || !strings.Contains(err.Error(), "HTTP 400: no such offer") || calls != 1 {
		t.Errorf("a 4xx answer is final: err %v calls %d", err, calls)
	}

	calls = 0
	v, srv3 := testVast(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) })
	defer srv3.Close()
	if _, err := v.Instances(context.Background()); err == nil || calls != 4 {
		t.Errorf("4 attempts, then the error: err %v calls %d", err, calls)
	}
}

func TestRentRequest(t *testing.T) {
	v, srv := testVast(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.Method != http.MethodPut || r.URL.Path != "/v0/asks/7/" || body["image"] != "img" || body["disk"] != float64(diskGB) ||
			body["runtype"] != "args" || body["env"] != "-e PUBKEY_B64=cHVi -p 22:22" || body["label"] != Label {
			t.Errorf("%s %s %v", r.Method, r.URL.Path, body)
		}
		io.WriteString(w, `{"success":true,"new_contract":42}`)
	})
	defer srv.Close()
	if id, err := v.Rent(context.Background(), 7, "img", "cHVi"); id != 42 || err != nil {
		t.Errorf("id %d err %v", id, err)
	}
}

// ---------- the two-host race ----------

func TestPickWinner(t *testing.T) {
	cheap, pricey := Rented{ID: 1, Dph: 0.30}, Rented{ID: 2, Dph: 0.40}
	boom := errors.New("boom")
	cases := []struct {
		name   string
		feed   func(ch chan<- dialed)
		grace  time.Duration
		winner int64
		lost   int
	}{
		{"cheapest first wins at once", func(ch chan<- dialed) { ch <- dialed{r: cheap} }, time.Hour, 1, 0},
		{"pricier first, cheapest inside the grace", func(ch chan<- dialed) { ch <- dialed{r: pricey}; ch <- dialed{r: cheap} }, time.Hour, 1, 1},
		{"pricier first, cheapest too slow", func(ch chan<- dialed) { ch <- dialed{r: pricey} }, 20 * time.Millisecond, 2, 0},
		{"pricier first, cheapest fails inside the grace", func(ch chan<- dialed) { ch <- dialed{r: pricey}; ch <- dialed{r: cheap, err: boom} }, time.Hour, 2, 1},
		{"cheapest fails, pricier wins without waiting", func(ch chan<- dialed) { ch <- dialed{r: cheap, err: boom}; ch <- dialed{r: pricey} }, time.Hour, 2, 1},
		{"both fail", func(ch chan<- dialed) { ch <- dialed{r: cheap, err: boom}; ch <- dialed{r: pricey, err: boom} }, time.Hour, 0, 2},
	}
	for _, tc := range cases {
		ch := make(chan dialed, 2)
		tc.feed(ch)
		win, lost := pickWinner(context.Background(), []Rented{cheap, pricey}, ch, tc.grace, func(string, ...any) {})
		got := int64(0)
		if win != nil {
			got = win.r.ID
		}
		if got != tc.winner || len(lost) != tc.lost {
			t.Errorf("%s: winner %d (want %d), lost %d (want %d)", tc.name, got, tc.winner, len(lost), tc.lost)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if win, _ := pickWinner(ctx, []Rented{cheap}, make(chan dialed), time.Hour, func(string, ...any) {}); win != nil {
		t.Error("a cancelled Up has no winner")
	}
}

// ---------- SSH tunnel against an in-process sshd ----------

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

// ---------- Startup, Up and Down against a fake Vast and the in-process sshd ----------

type fakeVast struct {
	mu        sync.Mutex
	sshPort   string
	next      int64
	instances map[int64]Instance
	rented    []map[string]any
	deleted   []int64
}

func (f *fakeVast) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/v0/users/current/":
		io.WriteString(w, `{"credit":5}`)
	case r.URL.Path == "/rate":
		io.WriteString(w, `<ValCurs><Valute><CharCode>EUR</CharCode><Value>97,10</Value></Valute><Valute><CharCode>USD</CharCode><Value>83,4839</Value></Valute></ValCurs>`)
	case r.URL.Path == "/v0/bundles/":
		io.WriteString(w, `{"offers":[{"id":101,"machine_id":1,"dph_total":0.40,"inet_down":900,"geolocation":"SE","gpu_name":"RTX 3090"},
			{"id":102,"machine_id":2,"dph_total":0.30,"inet_down":800,"geolocation":"PL","gpu_name":"RTX 3090"},
			{"id":103,"machine_id":3,"dph_total":0.90,"inet_down":800,"geolocation":"US","gpu_name":"RTX 3090"}]}`)
	case strings.HasPrefix(r.URL.Path, "/v0/asks/"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.rented = append(f.rented, body)
		f.next++
		offer, _ := strconv.ParseInt(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v0/asks/"), "/"), 10, 64)
		inst := Instance{ID: f.next, Label: Label, Status: "running", IP: "127.0.0.1", Machine: offer - 100, GPU: "RTX 3090"}
		json.Unmarshal([]byte(`{"22/tcp":[{"HostPort":"`+f.sshPort+`"}]}`), &inst.Ports)
		f.instances[inst.ID] = inst
		fmt.Fprintf(w, `{"success":true,"new_contract":%d}`, inst.ID)
	case r.URL.Path == "/v1/instances/":
		list := []Instance{}
		for _, i := range f.instances {
			list = append(list, i)
		}
		json.NewEncoder(w).Encode(map[string]any{"instances": list})
	case r.Method == http.MethodDelete:
		id, _ := strconv.ParseInt(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v0/instances/"), "/"), 10, 64)
		delete(f.instances, id)
		f.deleted = append(f.deleted, id)
	default:
		http.NotFound(w, r)
	}
}

func TestUpAndDown(t *testing.T) {
	dir := t.TempDir()
	apiPort, grafanaPort := freePort(t), freePort(t)
	os.WriteFile(filepath.Join(dir, "config.json"), fmt.Appendf(nil, `{"vast_api_key":"k","cheapest_grace_seconds":1,"local_port":%d,"grafana_port":%d,"autostart":false}`, apiPort, grafanaPort), 0o600)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "pong") }))
	defer engine.Close()
	signer, pub, _ := loadKey(filepath.Join(dir, "ssh_key.pem"))
	var stateMu sync.Mutex
	remoteState := "downloading 10 из 78 ГБ"
	addr := sshd(t, signer.PublicKey(), strings.TrimPrefix(engine.URL, "http://"), func(cmd string) string {
		stateMu.Lock()
		defer stateMu.Unlock()
		if strings.HasPrefix(cmd, "cat /opt/llm/state") {
			defer func() { remoteState = "ready" }()
			return remoteState + "\n"
		}
		return "RTX 3090, 350.00 W\n"
	})
	_, sshPort, _ := net.SplitHostPort(addr)
	fake := &fakeVast{sshPort: sshPort, instances: map[int64]Instance{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	c := New(dir)
	c.vast.Base, c.vast.Pause, c.poll, c.rates = srv.URL, time.Millisecond, 10*time.Millisecond, srv.URL+"/rate"
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
	if v.Machines[0].Dph != 0.30 {
		t.Errorf("the cheapest machine (0.30) must win inside the grace, got %.2f", v.Machines[0].Dph)
	}
	fake.mu.Lock()
	if len(fake.rented) != 2 {
		t.Errorf("the race rents the 2 cheapest, rented %d", len(fake.rented))
	}
	wantEnv := "-e PUBKEY_B64=" + base64.StdEncoding.EncodeToString([]byte(pub)) + " -p 22:22"
	if fake.rented[0]["env"] != wantEnv || fake.rented[0]["image"] != defaultImage {
		t.Errorf("rent body: %v", fake.rented[0])
	}
	fake.mu.Unlock()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", apiPort))
	if err != nil {
		t.Fatalf("the API must answer through the tunnel: %v", err)
	}
	resp.Body.Close()

	// the race loser is deleted in the background and leaves state.json
	deadline := time.Now().Add(5 * time.Second)
	for len(c.View().Machines) != 1 || func() bool { fake.mu.Lock(); defer fake.mu.Unlock(); return len(fake.deleted) != 1 }() {
		if time.Now().After(deadline) {
			t.Fatalf("loser not deleted: %+v", c.View().Machines)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// a restart of the program finds the machine in state.json and reconnects
	c.closeTunnel()
	again := New(dir)
	again.vast.Base, again.vast.Pause, again.poll, again.rates = srv.URL, time.Millisecond, 10*time.Millisecond, srv.URL+"/rate"
	again.Startup()
	if v := again.View(); v.Phase != "ready" || !v.Tunnel || v.Machines[0].ID != c.View().Machines[0].ID {
		t.Fatalf("after restart: %+v", v)
	}

	again.Down("тест")
	if v := again.View(); v.Phase != "off" || len(v.Machines) != 0 || v.Tunnel {
		t.Fatalf("after Down: %+v", v)
	}
	fake.mu.Lock()
	if len(fake.instances) != 0 {
		t.Errorf("machines left at Vast: %v", fake.instances)
	}
	fake.mu.Unlock()
	c.tasks.Wait()
	again.tasks.Wait()
	mu.Lock()
	if got := strings.Join(phases, " "); got != "off renting booting downloading ready" {
		t.Errorf("phases of the first Up: %s", got)
	}
	mu.Unlock()
}
