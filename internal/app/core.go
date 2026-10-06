package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// Core owns the rented machines and the tunnel. Up, Down and the reconnect of Tick never run at the same time.
type Core struct {
	Dir      string       // folder of the exe: config.json, state.json, ssh_key.pem, logs\
	OnChange func()       // something shown in the window changed
	OnLog    func(string) // a line for the window's journal

	vast   *Vast
	debug  atomic.Bool
	poll   time.Duration // pause between polls of Vast and of the machine
	rates  string        // where the dollar rate comes from
	tasks  sync.WaitGroup
	busy   chan struct{}
	signer ssh.Signer
	pub    string

	mu              sync.Mutex // guards everything below
	cfg             Config
	st              State
	phase, message  string
	credit          *float64
	tun             *tunnel
	cancelUp        context.CancelFunc
	lastCredit      time.Time
	lastStats       time.Time
	lastClean       time.Time
	lowCreditWarned bool
}

func New(dir string) *Core {
	c := &Core{Dir: dir, cfg: defaultConfig(), phase: "off", poll: 5 * time.Second, busy: make(chan struct{}, 1), lastStats: time.Now(),
		rates: "https://www.cbr.ru/scripts/XML_daily.asp"}
	c.vast = newVast(func() string { return strings.TrimSpace(c.Config().VastKey) }, c.debugf)
	return c
}

// Machine is a rented machine as the window shows it; Main is the one the tunnel goes to.
type Machine struct {
	Rented
	Main bool
}

// View is a consistent copy of everything the window shows.
type View struct {
	Phase, Message string
	Cfg            Config
	Machines       []Machine
	Credit         *float64
	Tunnel         bool
	IdleMin        int
}

func (c *Core) View() View {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := View{Phase: c.phase, Message: c.message, Cfg: c.cfg, Credit: c.credit, Tunnel: c.tun.alive()}
	if c.st.ID != 0 {
		v.Machines = append(v.Machines, Machine{Rented{c.st.ID, c.st.Machine, c.st.GPU, c.st.Geo, c.st.Dph, c.st.Created}, true})
	}
	for _, r := range c.st.Pending {
		v.Machines = append(v.Machines, Machine{r, false})
	}
	if c.tun != nil {
		v.IdleMin = int(time.Since(time.Unix(c.tun.lastUse.Load(), 0)).Minutes())
	}
	return v
}

func (c *Core) Config() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// Go runs fn in the background; a panic there is written to the journal instead of silently ending the program.
func (c *Core) Go(name string, fn func()) {
	c.tasks.Add(1)
	go func() {
		defer c.tasks.Done()
		defer func() {
			if r := recover(); r != nil {
				c.logf("ОШИБКА в фоновой задаче %s: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

func (c *Core) set(phase, msg string) {
	c.mu.Lock()
	changed := c.phase != phase || c.message != msg
	c.phase, c.message = phase, msg
	c.mu.Unlock()
	if changed {
		c.logf("[%s] %s", phase, msg)
		c.changed()
	}
}

func (c *Core) changed() {
	if c.OnChange != nil {
		c.OnChange()
	}
}

func (c *Core) sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func image() string {
	if v := os.Getenv("VASTLLM_IMAGE"); v != "" {
		return v
	}
	return defaultImage
}

// SetGPU is used by the next Up; a machine already rented keeps its GPU until Down.
func (c *Core) SetGPU(gpu string) {
	cfg := c.Config()
	if gpu == cfg.GPU {
		return
	}
	cfg.GPU = gpu
	c.setConfig(cfg)
	note := ""
	if v := c.View(); len(v.Machines) > 0 && v.Machines[0].Main && v.Machines[0].GPU != gpu {
		note = "; сейчас арендована " + v.Machines[0].GPU + " — новая будет после Down → Up"
	}
	c.Log("выбрана видеокарта " + gpu + note)
	c.changed()
}

var usdRate = regexp.MustCompile(`(?s)<CharCode>USD</CharCode>.*?<Value>([0-9,]+)</Value>`)

// RefreshRate takes today's dollar rate from the Bank of Russia; the last known one stays on failure.
func (c *Core) RefreshRate() {
	cfg := c.Config()
	rate, err := func() (float64, error) {
		req, _ := http.NewRequest(http.MethodGet, c.rates, nil)
		req.Header.Set("User-Agent", "VastLLM") // the bank answers 403 to Go's default User-Agent
		resp, err := c.vast.HTTP.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return 0, err
		}
		m := usdRate.FindSubmatch(raw)
		if resp.StatusCode != http.StatusOK || m == nil {
			return 0, fmt.Errorf("HTTP %d, курс доллара не найден", resp.StatusCode)
		}
		return strconv.ParseFloat(strings.Replace(string(m[1]), ",", ".", 1), 64)
	}()
	if err != nil || rate <= 0 {
		c.logf("курс ЦБ недоступен (%v), использую %.2f", err, cfg.UsdRub)
		return
	}
	cfg.UsdRub = rate
	c.setConfig(cfg)
	c.logf("курс ЦБ: %.2f ₽/$", rate)
	c.changed()
}

// ---------- SSH ----------

// dial waits for the container and opens SSH to its own sshd (port 22 mapped to a random external port).
func (c *Core) dial(ctx context.Context, id int64, pinned string, boot time.Duration) (*ssh.Client, string, error) {
	deadline := time.Now().Add(boot)
	var running time.Time
	last := errors.New("машина не запустилась вовремя")
	lastAPI := ""
	for time.Now().Before(deadline) {
		inst, err := c.vast.Instance(ctx, id)
		switch {
		case ctx.Err() != nil:
			return nil, "", ctx.Err()
		case err != nil:
			if err.Error() != lastAPI {
				lastAPI = err.Error()
				c.Log("Vast API: " + lastAPI)
			}
		case inst == nil:
			return nil, "", errors.New("машина исчезла")
		case inst.Status == "running" && inst.IP != "":
			if running.IsZero() {
				running = time.Now()
			}
			if port := inst.sshPort(); port > 0 {
				addr := net.JoinHostPort(inst.IP, strconv.Itoa(port))
				client, hostKey, err := dialSSH(ctx, addr, c.signer, pinned)
				if err == nil {
					c.debugf("ssh %s машины %d: подключено", addr, id)
					return client, hostKey, nil
				}
				last = err
				c.debugf("ssh %s машины %d: %v", addr, id, err)
			}
			if time.Since(running) > 3*time.Minute {
				return nil, "", fmt.Errorf("SSH недоступен 3 мин после старта: %w", last)
			}
		}
		if err := c.sleep(ctx, c.poll); err != nil {
			return nil, "", err
		}
	}
	return nil, "", last
}

// attach replaces the tunnel: the API port must be free, Grafana's is optional.
func (c *Core) attach(client *ssh.Client) error {
	cfg := c.Config()
	c.closeTunnel()
	t := newTunnel(client, func(err error) { c.logf("туннель: SSH-сессия оборвалась — %v", err) })
	if err := t.forward(cfg.LocalPort, 8080, true); err != nil {
		t.close()
		return fmt.Errorf("порт %d на этом ПК занят: %w", cfg.LocalPort, err)
	}
	if err := t.forward(cfg.GrafanaPort, 3000, false); err != nil {
		c.logf("Grafana: порт %d занят (%v)", cfg.GrafanaPort, err)
	}
	c.mu.Lock()
	c.tun = t
	c.mu.Unlock()
	// the card's power limit matters: the same GPU capped by its host runs up to 2x slower
	gpu, err := t.exec("nvidia-smi --query-gpu=name,power.limit,power.default_limit,memory.total,driver_version --format=csv,noheader")
	if err != nil {
		gpu = "не удалось узнать (" + err.Error() + ")"
	}
	c.Log("видеокарта машины: " + gpu)
	return nil
}

func (c *Core) closeTunnel() {
	c.mu.Lock()
	t := c.tun
	c.tun = nil
	c.mu.Unlock()
	t.close()
}

func (c *Core) tunnel() *tunnel {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tun
}

// reconnect opens a new tunnel to the main machine with its pinned host key.
func (c *Core) reconnect(ctx context.Context, wait time.Duration) error {
	c.mu.Lock()
	id, hostKey := c.st.ID, c.st.HostKey
	c.mu.Unlock()
	client, hostKey, err := c.dial(ctx, id, hostKey, wait)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.st.HostKey = hostKey
	c.saveState()
	c.mu.Unlock()
	return c.attach(client)
}

// ---------- Up ----------

// Up rents a machine (or reconnects to the rented one) and waits until the model answers.
func (c *Core) Up() {
	if hint := c.loadConfig(); hint != "" && c.vast.Key() == "" {
		c.set("error", hint)
		return
	}
	c.debug.Store(c.Config().Debug)
	c.checkCredit()
	if v := c.View(); v.Credit != nil && *v.Credit < 0.5 {
		c.set("error", fmt.Sprintf("на балансе Vast $%.2f — пополните на console.vast.ai, иначе Vast сразу остановит машину", *v.Credit))
		return
	}
	select {
	case c.busy <- struct{}{}:
	default:
		return // Up or Down is already running
	}
	defer func() { <-c.busy }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.mu.Lock()
	c.cancelUp = cancel
	c.mu.Unlock()
	if err := c.up(ctx); err != nil && ctx.Err() == nil { // cancelled by Down: Down reports
		c.set("error", err.Error())
	}
}

func (c *Core) up(ctx context.Context) error {
	start := time.Now()
	c.mu.Lock()
	id := c.st.ID
	c.mu.Unlock()
	if id != 0 && !c.tunnel().alive() {
		c.set("booting", fmt.Sprintf("подключаюсь к машине %d", id))
		if err := c.reconnect(ctx, 5*time.Minute); err != nil {
			return fmt.Errorf("машина %d не отвечает по SSH (%w). Она не удалена: Up — повторить, Down — удалить", id, err)
		}
	}
	for round := 1; id == 0 && round <= 3; round++ {
		var err error
		if id, err = c.rentAndRace(ctx, start); err != nil {
			return err
		}
	}
	if id == 0 {
		return errors.New("не удалось поднять машину за 3 попытки")
	}
	return c.waitReady(ctx, id, start)
}

// rentAndRace rents the cheapest machines (or takes the ones an interrupted Up left in state.json), keeps the first
// that answers by SSH and deletes the rest. It returns 0 when no machine came up and another round may help.
func (c *Core) rentAndRace(ctx context.Context, start time.Time) (int64, error) {
	cfg := c.Config()
	c.mu.Lock()
	cands := append([]Rented(nil), c.st.Pending...)
	bad := append([]int64(nil), c.st.BadMachines...)
	c.mu.Unlock()
	if len(cands) > 0 {
		c.logf("продолжаю прерванный запуск: машины %s", ids(cands))
	} else {
		all, err := c.vast.Offers(ctx, cfg.GPU, cfg.Datacenter)
		if err != nil {
			return 0, err
		}
		offers, dropped := usable(all, bad)
		for _, d := range dropped {
			c.debugf("предложение %s", d)
		}
		if len(offers) == 0 {
			return 0, fmt.Errorf("в Vast сейчас нет свободных %s с %d ГБ RAM — выберите другую видеокарту или повторите Up", cfg.GPU, minRAMMB/1000)
		}
		want := 1
		if cfg.Race {
			want = 2
			c.set("renting", fmt.Sprintf("арендую 2 самые дешёвые %s (аренда + трафик), оставлю первую поднявшуюся (дешёвой даю %d с форы)", cfg.GPU, cfg.Grace))
		} else {
			c.set("renting", "арендую самую дешёвую "+cfg.GPU)
		}
		for _, o := range offers[:min(want, len(offers))] {
			id, err := c.vast.Rent(ctx, o.ID, image(), base64.StdEncoding.EncodeToString([]byte(c.pub)))
			c.mu.Lock()
			if err != nil {
				c.st.BadMachines = append(c.st.BadMachines, o.Machine)
			} else {
				r := Rented{id, o.Machine, o.GPU, o.Geo, o.Dph, now()}
				c.st.Pending = append(c.st.Pending, r)
				cands = append(cands, r)
			}
			c.saveState()
			c.mu.Unlock()
			if err != nil {
				c.logf("аренда не удалась (%s): %v", o.Geo, err)
				continue
			}
			c.logf("арендована %d: %s %s %.1f ₽/ч, сеть %d Мбит/с, скачивание модели ≈ %.0f ₽", id, o.GPU, o.Geo, o.Dph*cfg.UsdRub, int(o.Inet),
				o.InetCost*modelGB*cfg.UsdRub)
		}
		if len(cands) == 0 {
			return 0, ctx.Err()
		}
	}
	c.set("booting", "машина качает образ и запускается (~5 мин)")

	race, stop := context.WithCancel(ctx)
	defer stop()
	results := make(chan dialed, len(cands))
	for _, r := range cands {
		c.Go("ssh", func() {
			client, hostKey, err := c.dial(race, r.ID, "", 10*time.Minute)
			results <- dialed{r, client, hostKey, err}
		})
	}
	win, lost := pickWinner(ctx, cands, results, time.Duration(cfg.Grace)*time.Second, func(f string, a ...any) { c.logf(f, a...) })
	stop()
	left := len(cands) - len(lost) - len(deref(win)) // racers stopped just now: collect them too
	for ; left > 0; left-- {
		lost = append(lost, <-results)
	}
	if ctx.Err() != nil { // Down: it deletes every machine in state.json itself
		for _, d := range append(lost, deref(win)...) {
			if d.client != nil {
				d.client.Close()
			}
		}
		return 0, ctx.Err()
	}
	for _, d := range lost {
		if d.client != nil {
			d.client.Close()
		}
		c.mu.Lock()
		if d.err != nil && !errors.Is(d.err, context.Canceled) {
			c.st.BadMachines = append(c.st.BadMachines, d.r.Machine)
			c.logLocked(fmt.Sprintf("машина %d не подошла: %v", d.r.ID, d.err))
		}
		c.mu.Unlock()
		c.Go("delete", func() { c.destroyPending(d.r.ID) })
	}
	if win == nil {
		return 0, nil
	}
	c.mu.Lock()
	c.st.Pending = without(c.st.Pending, win.r.ID)
	c.st.ID, c.st.Machine, c.st.GPU, c.st.Geo, c.st.Dph, c.st.Created = win.r.ID, win.r.Machine, win.r.GPU, win.r.Geo, win.r.Dph, win.r.Created
	c.st.HostKey = win.hostKey
	c.saveState()
	c.mu.Unlock()
	if err := c.attach(win.client); err != nil {
		return 0, err
	}
	c.logf("выбрана машина %d (%s), SSH через %s", win.r.ID, win.r.Geo, clock(time.Since(start)))
	return win.r.ID, nil
}

type dialed struct {
	r       Rented
	client  *ssh.Client
	hostKey string
	err     error
}

func deref(d *dialed) []dialed {
	if d == nil {
		return nil
	}
	return []dialed{*d}
}

// pickWinner reads SSH results until one machine is up. cands[0] is the cheapest (offers are rented cheapest first):
// another machine that comes up before it waits `grace` for it. It returns the winner (nil: none came up, or ctx
// ended) and every other result it has read.
func pickWinner(ctx context.Context, cands []Rented, results <-chan dialed, grace time.Duration, logf func(string, ...any)) (*dialed, []dialed) {
	cheapest := cands[0]
	var lost []dialed
	cheapestSeen := false
	for left := len(cands); left > 0; {
		var d dialed
		select {
		case d = <-results:
		case <-ctx.Done():
			return nil, lost
		}
		left--
		cheapestSeen = cheapestSeen || d.r.ID == cheapest.ID
		if d.err != nil {
			lost = append(lost, d)
			continue
		}
		if cheapestSeen {
			return &d, lost
		}
		logf("первой поднялась %d ($%.3f/ч), жду до %d с более дешёвую %d ($%.3f/ч)", d.r.ID, d.r.Dph, int(grace.Seconds()), cheapest.ID, cheapest.Dph)
		timer := time.NewTimer(grace)
		defer timer.Stop()
		for {
			select {
			case o := <-results:
				if o.r.ID == cheapest.ID && o.err == nil {
					logf("дешёвая %d успела — беру её, %d удаляю", o.r.ID, d.r.ID)
					return &o, append(lost, d)
				}
				lost = append(lost, o)
				if o.r.ID != cheapest.ID {
					continue
				}
				logf("дешёвая %d не поднялась — оставляю %d", cheapest.ID, d.r.ID)
			case <-timer.C:
				logf("дешёвая %d не успела за %d с — оставляю %d", cheapest.ID, int(grace.Seconds()), d.r.ID)
			case <-ctx.Done():
				return nil, append(lost, d)
			}
			return &d, lost
		}
	}
	return nil, lost
}

// waitReady follows /opt/llm/state, which llmd in the container keeps up to date.
func (c *Core) waitReady(ctx context.Context, id int64, start time.Time) error {
	for deadline := time.Now().Add(60 * time.Minute); time.Now().Before(deadline); {
		if !c.tunnel().alive() {
			c.Log("связь потеряна, переподключаюсь")
			if err := c.reconnect(ctx, 3*time.Minute); err != nil {
				return fmt.Errorf("связь с машиной %d потеряна (%w). Она не удалена: Up — повторить, Down — удалить", id, err)
			}
		}
		state, _ := c.tunnel().exec("cat /opt/llm/state 2>/dev/null")
		phase, detail, _ := strings.Cut(state, " ")
		switch phase {
		case "ready":
			c.set("ready", "готово за "+clock(time.Since(start)))
			return nil
		case "failed":
			c.mu.Lock()
			c.st.BadMachines = append(c.st.BadMachines, c.st.Machine)
			c.saveState()
			c.mu.Unlock()
			return fmt.Errorf("машина %d не подошла: %s. Нажмите Down, затем Up — возьму другую", id, detail)
		case "downloading":
			c.set("downloading", "качаю модель: "+detail)
		case "preparing":
			c.set("loading", "готовлю веса модели для движка (~2 мин)")
		case "loading":
			c.set("loading", "загружаю модель в память и GPU")
		default:
			c.set("booting", "контейнер запускается")
		}
		if err := c.sleep(ctx, c.poll); err != nil {
			return err
		}
	}
	return errors.New("модель не загрузилась за 60 минут")
}

// ---------- Down ----------

// destroy deletes a machine and waits until Vast no longer lists it.
func (c *Core) destroy(id int64) error {
	ctx := context.Background()
	if err := c.vast.Destroy(ctx, id); err != nil {
		c.logf("delete %d: %v", id, err)
	}
	for range 40 {
		if inst, err := c.vast.Instance(ctx, id); err == nil && inst == nil {
			c.logf("машина %d удалена (подтверждено API)", id)
			return nil
		}
		time.Sleep(c.poll)
	}
	return fmt.Errorf("удаление %d не подтверждено, проверьте console.vast.ai", id)
}

// destroyPending deletes an extra machine of an Up (race loser) and drops it from state.json once Vast confirms.
func (c *Core) destroyPending(id int64) {
	if err := c.destroy(id); err != nil {
		c.Log(err.Error())
		return
	}
	c.mu.Lock()
	c.st.Pending = without(c.st.Pending, id)
	c.saveState()
	c.mu.Unlock()
	c.changed()
}

// Down deletes every rented machine; nothing else ever does.
func (c *Core) Down(reason string) {
	c.mu.Lock()
	if c.cancelUp != nil {
		c.cancelUp()
	}
	c.mu.Unlock()
	c.busy <- struct{}{}
	defer func() { <-c.busy }()
	c.mu.Lock()
	var all []int64
	if c.st.ID != 0 {
		all = append(all, c.st.ID)
	}
	for _, r := range c.st.Pending {
		all = append(all, r.ID)
	}
	c.mu.Unlock()
	if len(all) == 0 {
		c.set("off", "машин нет")
		return
	}
	c.set("stopping", fmt.Sprintf("удаляю %v (%s)", all, reason))
	c.closeTunnel()
	for _, id := range all {
		if err := c.destroy(id); err != nil {
			c.set("error", err.Error())
			return
		}
	}
	c.mu.Lock()
	c.st = State{BadMachines: c.st.BadMachines}
	c.saveState()
	c.mu.Unlock()
	c.set("off", "машины удалены, оплата остановлена")
}

// ---------- upkeep ----------

func (c *Core) checkCredit() {
	credit, err := c.vast.Credit(context.Background())
	if err != nil {
		c.debugf("баланс Vast не получен: %v", err)
		return
	}
	c.mu.Lock()
	c.credit, c.lastCredit = &credit, time.Now()
	warn := credit < 1 && !c.lowCreditWarned
	c.lowCreditWarned = credit < 1
	c.mu.Unlock()
	c.debugf("баланс Vast $%.2f", credit)
	if warn {
		c.logf("⚠ баланс Vast почти кончился: $%.2f. На нуле Vast сам остановит машину — пополните на console.vast.ai", credit)
	}
	c.changed()
}

// Tick runs every 30 s: balance, and the tunnel to the main machine is reopened when it drops.
func (c *Core) Tick() {
	c.mu.Lock()
	id, creditAge, cleanAge := c.st.ID, time.Since(c.lastCredit), time.Since(c.lastClean)
	c.mu.Unlock()
	if creditAge > 10*time.Minute && c.vast.Key() != "" {
		c.checkCredit()
	}
	if cleanAge > 24*time.Hour {
		c.mu.Lock()
		c.lastClean = time.Now()
		c.mu.Unlock()
		c.cleanLogs()
	}
	if id == 0 {
		return
	}
	select {
	case c.busy <- struct{}{}: // a reconnect can take minutes: one at a time, and never beside Up or Down
	default:
		return
	}
	defer func() { <-c.busy }()
	if !c.tunnel().alive() {
		c.restoreTunnel(id)
	}
	c.mu.Lock()
	report := time.Since(c.lastStats) > 10*time.Minute
	if report {
		c.lastStats = time.Now()
	}
	c.mu.Unlock()
	if t := c.tunnel(); report && t != nil {
		c.debugf("туннель %v, машина %d, запросов через туннель за 10 мин: %d", t.alive(), id, t.requests.Swap(0))
	}
}

func (c *Core) restoreTunnel(id int64) {
	ctx := context.Background()
	c.Log("связь потеряна, переподключаюсь")
	inst, err := c.vast.Instance(ctx, id)
	switch {
	case err != nil:
		c.logf("переподключение не удалось: %v", err)
		return
	case inst == nil:
		c.closeTunnel()
		c.mu.Lock()
		c.st.ID = 0
		c.saveState()
		c.mu.Unlock()
		c.set("error", "машина пропала у Vast — нажмите Up")
		return
	case inst.Status == "exited" || inst.Status == "stopped" || inst.Intended == "stopped":
		c.checkCredit()
		credit := 0.0
		if v := c.View(); v.Credit != nil {
			credit = *v.Credit
		}
		c.set("error", fmt.Sprintf("Vast остановил машину %d (обычно — кончился баланс; сейчас $%.2f). Пополните баланс, затем Down и Up", id, credit))
		return
	}
	if err := c.reconnect(ctx, 3*time.Minute); err != nil {
		c.logf("переподключение не удалось: %v", err)
		return
	}
	c.Log("связь восстановлена")
	if state, _ := c.tunnel().exec("cat /opt/llm/state 2>/dev/null"); state == "ready" {
		c.set("ready", "связь восстановлена")
	}
}

// Startup reconciles state.json with what is really rented at Vast (after a restart or a PC reboot), then reconnects.
func (c *Core) Startup() {
	var err error
	if c.signer, c.pub, err = loadKey(c.path("ssh_key.pem")); err != nil {
		c.set("error", err.Error())
		return
	}
	c.loadState()
	hint := c.loadConfig()
	cfg := c.Config()
	c.debug.Store(cfg.Debug)
	if hint != "" {
		c.set("error", hint)
	}
	c.logf("настройки: GPU %s (самая дешёвая, фора %d с), порты %d/%d, гонка двух хостов %s, журнал %d дн, образ %s",
		cfg.GPU, cfg.Grace, cfg.LocalPort, cfg.GrafanaPort, yesNo(cfg.Race), cfg.LogDays, image())
	c.Go("rate", c.RefreshRate)
	if c.vast.Key() == "" {
		return
	}
	c.checkCredit()
	live, err := c.vast.Instances(context.Background())
	if err != nil {
		c.set("error", "не удалось проверить Vast: "+err.Error())
		return
	}
	c.mu.Lock()
	rented := map[int64]Instance{}
	for _, i := range live {
		if i.Label == Label {
			rented[i.ID] = i
		}
	}
	if _, ok := rented[c.st.ID]; c.st.ID != 0 && !ok {
		c.logLocked(fmt.Sprintf("машины %d у Vast больше нет", c.st.ID))
		c.st.ID = 0
	}
	known := map[int64]bool{c.st.ID: true}
	var pending []Rented
	for _, r := range c.st.Pending {
		if _, ok := rented[r.ID]; ok {
			pending = append(pending, r)
			known[r.ID] = true
		}
	}
	for id, i := range rented { // rented by this program but missing from state.json: adopt, never lose track
		if !known[id] {
			pending = append(pending, Rented{id, i.Machine, i.GPU, i.Geo, i.Dph, now()})
		}
	}
	c.st.Pending = pending
	c.saveState()
	main, extra := c.st.ID, append([]Rented(nil), pending...)
	c.mu.Unlock()
	c.logf("в Vast машин этой программы: %d", len(rented))
	switch {
	case main != 0 && len(extra) > 0:
		c.logf("лишние машины прерванного запуска %s — удаляю, основная %d", ids(extra), main)
		for _, r := range extra {
			c.Go("delete", func() { c.destroyPending(r.ID) })
		}
		c.Up()
	case main != 0 || len(extra) > 0:
		c.Up()
	default:
		c.set("off", "машин нет")
	}
}

func without(list []Rented, id int64) []Rented {
	var out []Rented
	for _, r := range list {
		if r.ID != id {
			out = append(out, r)
		}
	}
	return out
}

func ids(list []Rented) string {
	var s []string
	for _, r := range list {
		s = append(s, strconv.FormatInt(r.ID, 10))
	}
	return strings.Join(s, ", ")
}

func clock(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

func yesNo(b bool) string {
	if b {
		return "да"
	}
	return "нет"
}
