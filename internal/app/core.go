package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
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

	qp       *QuickPod
	debug    atomic.Bool
	poll     time.Duration // pause between polls of QuickPod and of the machine
	waitPoll time.Duration // pause between looks for a fast machine while Up waits for one
	rates    string        // where the dollar rate comes from
	tasks    sync.WaitGroup
	busy     chan struct{}
	signer   ssh.Signer
	pub      string

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
	lastReconcile   time.Time
	lowCreditWarned bool
}

func New(dir string) *Core {
	c := &Core{Dir: dir, cfg: defaultConfig(), phase: "off", poll: 5 * time.Second, waitPoll: time.Minute, busy: make(chan struct{}, 1), lastStats: time.Now(),
		rates: "https://www.cbr.ru/scripts/XML_daily.asp"}
	c.qp = newQuickPod(func() string { return strings.TrimSpace(c.Config().Key) }, c.debugf)
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
	if c.st.ID != "" {
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
		resp, err := c.qp.HTTP.Do(req)
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
func (c *Core) dial(ctx context.Context, id, pinned string, boot time.Duration) (*ssh.Client, string, error) {
	deadline := time.Now().Add(boot)
	var running time.Time
	last := errors.New("машина не запустилась вовремя")
	lastAPI := ""
	for time.Now().Before(deadline) {
		inst, err := c.qp.Pod(ctx, id)
		switch {
		case ctx.Err() != nil:
			return nil, "", ctx.Err()
		case err != nil:
			if err.Error() != lastAPI {
				lastAPI = err.Error()
				c.Log("QuickPod API: " + lastAPI)
			}
		case inst == nil:
			return nil, "", errors.New("машина исчезла")
		case inst.running():
			if running.IsZero() {
				running = time.Now()
			}
			if port := inst.sshPort(); port > 0 {
				addr := net.JoinHostPort(inst.IP, strconv.Itoa(port))
				client, hostKey, err := dialSSH(ctx, addr, c.signer, pinned)
				if err == nil {
					c.debugf("ssh %s машины %s: подключено", addr, short(id))
					return client, hostKey, nil
				}
				last = err
				c.debugf("ssh %s машины %s: %v", addr, short(id), err)
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
	if hint := c.loadConfig(); hint != "" {
		c.set("error", hint)
		return
	}
	c.debug.Store(c.Config().Debug)
	c.checkCredit()
	if v := c.View(); v.Credit != nil && *v.Credit < 0.5 {
		c.set("error", fmt.Sprintf("на балансе QuickPod $%.2f — пополните на console.quickpod.io (Settings → Billing), иначе QuickPod сразу остановит машину", *v.Credit))
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
	if id != "" && !c.tunnel().alive() {
		c.set("booting", fmt.Sprintf("подключаюсь к машине %s", short(id)))
		if err := c.reconnect(ctx, 5*time.Minute); err != nil {
			return fmt.Errorf("машина %s не отвечает по SSH (%w). Она не удалена: Up — повторить, Down — удалить", short(id), err)
		}
	}
	for round := 1; round <= 3; round++ {
		if id == "" {
			var err error
			if id, err = c.rentAndRace(ctx, start); err != nil {
				return err
			}
			if id == "" {
				continue
			}
		}
		err := c.waitReady(ctx, id, start)
		var bad *machineFailed
		if !errors.As(err, &bad) {
			return err
		}
		// it cannot serve the model and is billed while it stays: delete it at once and take the next machine
		c.logf("машина %s не подошла: %s — удаляю её и беру следующую", short(id), bad.reason)
		c.closeTunnel()
		c.mu.Lock()
		c.blockLocked(bad.machine, bad.permanent, bad.reason)
		c.mu.Unlock()
		if derr := c.destroy(id); derr != nil {
			return fmt.Errorf("машина %s не подошла (%s), и удаление не подтвердилось: %w. Нажмите Down", short(id), bad.reason, derr)
		}
		c.mu.Lock()
		c.st.ID, c.st.HostKey = "", ""
		c.saveState()
		c.mu.Unlock()
		id = ""
	}
	return errors.New("не удалось поднять машину за 3 попытки")
}

// machineFailed: the machine itself cannot serve (llmd said so, or it never got ready); Up deletes it and goes on.
type machineFailed struct {
	machine   int64
	reason    string
	permanent bool // it can never run the engine (no AVX2, too little memory): never rent it again
}

func (e *machineFailed) Error() string { return e.reason }

// blockLocked keeps a machine out of the offers: for good when it can never run the engine, otherwise for 3 hours.
// Callers hold c.mu.
func (c *Core) blockLocked(machine int64, permanent bool, reason string) {
	until := time.Now().Add(3 * time.Hour)
	if permanent {
		until = time.Now().AddDate(10, 0, 0)
	}
	c.st.Blocked = append(c.st.Blocked, Block{machine, stamp{until.UTC()}, reason})
	c.saveState()
}

// blockedLocked drops the expired blocks and returns the machines still blocked. Callers hold c.mu.
func (c *Core) blockedLocked() []int64 {
	var keep []Block
	var machines []int64
	for _, b := range c.st.Blocked {
		if time.Now().Before(b.Until.Time) {
			keep = append(keep, b)
			machines = append(machines, b.Machine)
		}
	}
	c.st.Blocked = keep
	return machines
}

// pickOffers returns the machines to rent, best first. With fast_only only the fast configuration counts: while there
// is none it waits, polling every c.waitPoll, up to wait_fast_minutes (Down cancels the wait).
func (c *Core) pickOffers(ctx context.Context, cfg Config) ([]Offer, error) {
	deadline := time.Now().Add(time.Duration(cfg.WaitFastMin) * time.Minute)
	for {
		c.mu.Lock()
		bad := c.blockedLocked()
		c.mu.Unlock()
		all, err := c.qp.Offers(ctx)
		if err != nil {
			return nil, err
		}
		offers, dropped := usable(all, cfg.GPU, bad)
		for _, d := range dropped {
			c.debugf("предложение %s", d)
		}
		var fast []Offer
		for _, o := range offers {
			if o.fast() {
				fast = append(fast, o)
			}
		}
		switch {
		case len(fast) > 0:
			return fast, nil
		case !cfg.FastOnly && len(offers) > 0:
			return offers, nil
		case !cfg.FastOnly:
			return nil, fmt.Errorf("в QuickPod сейчас нет свободных %s, на которых работает модель (≥ %d ГБ RAM, ≥ %d потоков, AVX2, сеть ≥ %d Мбит/с) — повторите Up позже или выберите другую видеокарту",
				cfg.GPU, minRAMGB, minThreads, minInet)
		}
		other := "других подходящих тоже нет"
		if len(offers) > 0 {
			other = "свободна только " + describe(offers[0])
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("быстрой %s (≥ %d ГБ RAM, PCIe 4.0 x16, ≥ %d потоков) нет %d мин; %s. Чтобы брать и такие, поставьте \"fast_only\": false в config.json",
				cfg.GPU, fastRAMGB, fastThreads, cfg.WaitFastMin, other)
		}
		c.set("waiting", fmt.Sprintf("жду быструю %s (≥ %d ГБ RAM, PCIe 4.0 x16), проверяю раз в минуту до %s; %s. Машина не арендована, денег это не стоит; Down — отмена",
			cfg.GPU, fastRAMGB, deadline.Format("15:04"), other))
		if err := c.sleep(ctx, c.waitPoll); err != nil {
			return nil, err
		}
	}
}

// rentName is the altname of one rent: Label plus a nonce, so a rent whose answer was lost is found again by it.
func rentName() string {
	b := make([]byte, 4)
	rand.Read(b)
	return Label + "-" + hex.EncodeToString(b)
}

// rentAndRace rents the best machine (two with the race; or takes the ones an interrupted Up left in state.json),
// keeps the first that answers by SSH and deletes the rest. It returns "" when no machine came up and another round
// may help.
func (c *Core) rentAndRace(ctx context.Context, start time.Time) (string, error) {
	cfg := c.Config()
	c.mu.Lock()
	cands := append([]Rented(nil), c.st.Pending...)
	c.mu.Unlock()
	if len(cands) > 0 {
		c.logf("продолжаю прерванный запуск: машины %s", ids(cands))
	} else {
		offers, err := c.pickOffers(ctx, cfg)
		if err != nil {
			return "", err
		}
		want := 1
		if cfg.Race && len(offers) > 1 && offers[1].fast() == offers[0].fast() { // never race the best against a slower one
			want = 2
			c.set("renting", fmt.Sprintf("арендую 2 лучшие %s, оставлю первую поднявшуюся (лучшей даю %d с форы)", cfg.GPU, cfg.Grace))
		} else {
			c.set("renting", "арендую "+describe(offers[0]))
		}
		for _, o := range offers[:min(want, len(offers))] {
			id, err := c.qp.Rent(ctx, o.ID, cfg.Template, base64.StdEncoding.EncodeToString([]byte(c.pub)), rentName())
			c.mu.Lock()
			if err == nil {
				r := Rented{id, o.Machine, o.GPU, o.Geo, o.Dph, now()}
				c.st.Pending = append(c.st.Pending, r)
				cands = append(cands, r)
				c.saveState()
			}
			c.mu.Unlock()
			if err != nil { // a refusal (the offer was just taken) or a lost answer: no fault of the machine, nothing blocked
				c.logf("аренда не удалась (%s): %v", o.Geo, err)
				continue
			}
			c.logf("арендована %s: %s, %.1f ₽/ч, скачивание модели ≈ %.0f ₽", short(id), describe(o), o.Dph*cfg.UsdRub, modelGB*0.004*cfg.UsdRub)
		}
		if len(cands) == 0 {
			return "", ctx.Err()
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
		return "", ctx.Err()
	}
	for _, d := range lost {
		if d.client != nil {
			d.client.Close()
		}
		c.mu.Lock()
		if d.err != nil && !errors.Is(d.err, context.Canceled) {
			c.blockLocked(d.r.Machine, false, "SSH: "+d.err.Error())
			c.logLocked(fmt.Sprintf("машина %s не подошла: %v", short(d.r.ID), d.err))
		}
		c.mu.Unlock()
		c.Go("delete", func() { c.destroyPending(d.r.ID) })
	}
	if win == nil {
		return "", nil
	}
	c.mu.Lock()
	c.st.Pending = without(c.st.Pending, win.r.ID)
	c.st.ID, c.st.Machine, c.st.GPU, c.st.Geo, c.st.Dph, c.st.Created = win.r.ID, win.r.Machine, win.r.GPU, win.r.Geo, win.r.Dph, win.r.Created
	c.st.HostKey = win.hostKey
	c.saveState()
	c.mu.Unlock()
	if err := c.attach(win.client); err != nil {
		return "", err
	}
	c.logf("выбрана машина %s (%s), SSH через %s", short(win.r.ID), win.r.Geo, clock(time.Since(start)))
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

// pickWinner reads SSH results until one machine is up. cands[0] is the best (offers are rented best first):
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
		logf("первой поднялась %s ($%.3f/ч), жду до %d с лучшую %s ($%.3f/ч)", short(d.r.ID), d.r.Dph, int(grace.Seconds()), short(cheapest.ID), cheapest.Dph)
		timer := time.NewTimer(grace)
		defer timer.Stop()
		for {
			select {
			case o := <-results:
				if o.r.ID == cheapest.ID && o.err == nil {
					logf("лучшая %s успела — беру её, %s удаляю", short(o.r.ID), short(d.r.ID))
					return &o, append(lost, d)
				}
				lost = append(lost, o)
				if o.r.ID != cheapest.ID {
					continue
				}
				logf("лучшая %s не поднялась — оставляю %s", short(cheapest.ID), short(d.r.ID))
			case <-timer.C:
				logf("лучшая %s не успела за %d с — оставляю %s", short(cheapest.ID), int(grace.Seconds()), short(d.r.ID))
			case <-ctx.Done():
				return nil, append(lost, d)
			}
			return &d, lost
		}
	}
	return nil, lost
}

// waitReady follows /opt/llm/state, which llmd in the container keeps up to date.
func (c *Core) waitReady(ctx context.Context, id string, start time.Time) error {
	for deadline := time.Now().Add(60 * time.Minute); time.Now().Before(deadline); {
		if !c.tunnel().alive() {
			c.Log("связь потеряна, переподключаюсь")
			if err := c.reconnect(ctx, 3*time.Minute); err != nil {
				return fmt.Errorf("связь с машиной %s потеряна (%w). Она не удалена: Up — повторить, Down — удалить", short(id), err)
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
			machine := c.st.Machine
			c.mu.Unlock()
			// llmd's own checks of the hardware: such a machine can never run the engine
			never := strings.Contains(detail, "AVX2") || strings.Contains(detail, "мало памяти")
			return &machineFailed{machine, detail, never}
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
	c.mu.Lock()
	machine := c.st.Machine
	c.mu.Unlock()
	return &machineFailed{machine, "модель не загрузилась за 60 минут", false}
}

// ---------- Down ----------

// destroy deletes a machine and waits until QuickPod no longer lists it, sending the delete again every 6 polls.
func (c *Core) destroy(id string) error {
	ctx := context.Background()
	for try := range 40 {
		if try%6 == 0 {
			if err := c.qp.Destroy(ctx, id); err != nil {
				c.logf("delete %s: %v", short(id), err)
			}
		}
		time.Sleep(c.poll)
		if pod, err := c.qp.Pod(ctx, id); err == nil && pod == nil {
			c.logf("машина %s удалена (подтверждено API)", short(id))
			return nil
		}
	}
	return fmt.Errorf("удаление %s не подтверждено, проверьте console.quickpod.io", short(id))
}

// ourPods lists every machine of this program QuickPod still has, known to state.json or not.
func (c *Core) ourPods() ([]Pod, error) {
	pods, err := c.qp.Pods(context.Background())
	var ours []Pod
	for _, p := range pods {
		if p.ours() && p.UUID != "" {
			ours = append(ours, p)
		}
	}
	return ours, err
}

// destroyPending deletes an extra machine of an Up (race loser) and drops it from state.json once QuickPod confirms.
func (c *Core) destroyPending(id string) {
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
	var all []string
	if c.st.ID != "" {
		all = append(all, c.st.ID)
	}
	for _, r := range c.st.Pending {
		all = append(all, r.ID)
	}
	c.mu.Unlock()
	// and every machine of this program QuickPod still lists: a rent whose answer was lost, an adopted one
	ours, err := c.ourPods()
	if err != nil {
		c.logf("список машин QuickPod не получен (%v) — удаляю известные", err)
	}
	for _, p := range ours {
		if !slices.Contains(all, p.UUID) {
			c.logf("⚠ у QuickPod есть машина %s, которой нет в state.json — удаляю и её", short(p.UUID))
			all = append(all, p.UUID)
		}
	}
	if len(all) == 0 {
		c.set("off", "машин нет")
		return
	}
	c.set("stopping", fmt.Sprintf("удаляю %s (%s)", shorts(all), reason))
	c.closeTunnel()
	var left []string
	for _, id := range all { // every machine is tried even when one of them fails
		if err := c.destroy(id); err != nil {
			c.Log(err.Error())
			left = append(left, id)
		}
	}
	c.mu.Lock()
	c.st = State{Blocked: c.st.Blocked}
	for _, id := range left { // what was not confirmed stays known, so the next Down (or a restart) tries it again
		c.st.Pending = append(c.st.Pending, Rented{ID: id, Created: now()})
	}
	c.saveState()
	c.mu.Unlock()
	if len(left) > 0 {
		c.set("error", fmt.Sprintf("удаление %s не подтверждено — нажмите Down ещё раз или проверьте console.quickpod.io", shorts(left)))
		return
	}
	c.set("off", "машины удалены, оплата остановлена")
}

// ---------- upkeep ----------

func (c *Core) checkCredit() {
	credit, err := c.qp.Credit(context.Background())
	if err != nil {
		c.debugf("баланс QuickPod не получен: %v", err)
		return
	}
	c.mu.Lock()
	c.credit, c.lastCredit = &credit, time.Now()
	warn := credit < 1 && !c.lowCreditWarned
	c.lowCreditWarned = credit < 1
	c.mu.Unlock()
	c.debugf("баланс QuickPod $%.2f", credit)
	if warn {
		c.logf("⚠ баланс QuickPod почти кончился: $%.2f. На нуле QuickPod сам остановит машину (диск продолжит оплачиваться) — пополните на console.quickpod.io", credit)
	}
	c.changed()
}

// reconcile, every 10 minutes beside no Up or Down: a machine of this program that QuickPod lists and state.json does
// not (a rent whose answer was lost) is billed unseen - it is taken into state.json, so the window shows it and Down
// deletes it.
func (c *Core) reconcile() {
	c.mu.Lock()
	due := time.Since(c.lastReconcile) > 10*time.Minute && c.qp.Key() != ""
	if due {
		c.lastReconcile = time.Now()
	}
	c.mu.Unlock()
	if !due {
		return
	}
	ours, err := c.ourPods()
	if err != nil {
		c.debugf("сверка с QuickPod: %v", err)
		return
	}
	c.mu.Lock()
	var found []string
	for _, p := range ours {
		known := p.UUID == c.st.ID
		for _, r := range c.st.Pending {
			known = known || r.ID == p.UUID
		}
		if !known {
			c.st.Pending = append(c.st.Pending, Rented{p.UUID, p.Machine, p.GPU, p.Geo, p.Dph, now()})
			found = append(found, p.UUID)
		}
	}
	if len(found) > 0 {
		c.saveState()
	}
	c.mu.Unlock()
	if len(found) > 0 {
		c.logf("⚠ у QuickPod есть машины этой программы, о которых она не знала: %s — они оплачиваются, Down удалит их", shorts(found))
		c.changed()
	}
}

// Tick runs every 30 s: balance, the check for unseen machines, and the tunnel to the main machine is reopened when it
// drops.
func (c *Core) Tick() {
	c.mu.Lock()
	id, creditAge, cleanAge := c.st.ID, time.Since(c.lastCredit), time.Since(c.lastClean)
	c.mu.Unlock()
	if creditAge > 10*time.Minute && c.qp.Key() != "" {
		c.checkCredit()
	}
	if cleanAge > 24*time.Hour {
		c.mu.Lock()
		c.lastClean = time.Now()
		c.mu.Unlock()
		c.cleanLogs()
	}
	select {
	case c.busy <- struct{}{}: // a reconnect can take minutes: one at a time, and never beside Up or Down
	default:
		return
	}
	defer func() { <-c.busy }()
	c.reconcile()
	if id == "" {
		return
	}
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
		c.debugf("туннель %v, машина %s, запросов через туннель за 10 мин: %d", t.alive(), short(id), t.requests.Swap(0))
	}
}

func (c *Core) restoreTunnel(id string) {
	ctx := context.Background()
	c.Log("связь потеряна, переподключаюсь")
	inst, err := c.qp.Pod(ctx, id)
	switch {
	case err != nil:
		c.logf("переподключение не удалось: %v", err)
		return
	case inst == nil:
		c.closeTunnel()
		c.mu.Lock()
		c.st.ID = ""
		c.saveState()
		c.mu.Unlock()
		c.set("error", "машина пропала у QuickPod — нажмите Up")
		return
	case inst.stopped():
		c.checkCredit()
		credit := 0.0
		if v := c.View(); v.Credit != nil {
			credit = *v.Credit
		}
		c.set("error", fmt.Sprintf("QuickPod остановил машину %s (обычно — кончился баланс; сейчас $%.2f; диск оплачивается и дальше). Пополните баланс, затем Down и Up", short(id), credit))
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

// Startup reconciles state.json with what is really rented at QuickPod (after a restart or a PC reboot), then reconnects.
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
	c.logf("настройки: QuickPod, GPU %s, шаблон %s (образ в нём: %s), порты %d/%d, гонка двух хостов %s, журнал %d дн",
		cfg.GPU, short(cfg.Template), defaultImage, cfg.LocalPort, cfg.GrafanaPort, yesNo(cfg.Race), cfg.LogDays)
	c.Go("rate", c.RefreshRate)
	if c.qp.Key() == "" {
		return
	}
	c.checkCredit()
	live, err := c.qp.Pods(context.Background())
	if err != nil {
		c.set("error", "не удалось проверить QuickPod: "+err.Error())
		return
	}
	c.mu.Lock()
	rented := map[string]Pod{}
	for _, p := range live {
		if p.ours() {
			rented[p.UUID] = p
		}
	}
	if _, ok := rented[c.st.ID]; c.st.ID != "" && !ok {
		c.logLocked(fmt.Sprintf("машины %s у QuickPod больше нет", short(c.st.ID)))
		c.st.ID = ""
	}
	known := map[string]bool{c.st.ID: true}
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
	c.logf("в QuickPod машин этой программы: %d", len(rented))
	switch {
	case main != "" && len(extra) > 0:
		c.logf("лишние машины прерванного запуска %s — удаляю, основная %s", ids(extra), short(main))
		for _, r := range extra {
			c.Go("delete", func() { c.destroyPending(r.ID) })
		}
		c.Up()
	case main != "" || len(extra) > 0:
		c.Up()
	default:
		c.set("off", "машин нет")
	}
}

func without(list []Rented, id string) []Rented {
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
		s = append(s, r.ID)
	}
	return shorts(s)
}

// Short is how the window names a machine.
func (r Rented) Short() string { return short(r.ID) }

// short is the first block of a pod UUID: enough to tell machines apart in the window and the journal.
func short(uuid string) string {
	if head, _, ok := strings.Cut(uuid, "-"); ok {
		return head
	}
	return uuid
}

func shorts(list []string) string {
	s := make([]string, len(list))
	for i, id := range list {
		s[i] = short(id)
	}
	return strings.Join(s, ", ")
}

// describe is how an offer shows in the window and the journal.
func describe(o Offer) string {
	fast := ""
	if o.fast() {
		fast = ", быстрая конфигурация"
	}
	return fmt.Sprintf("%s · %s, %d потоков · %.0f ГБ RAM · PCIe %s x%s · %s · сеть %.0f Мбит/с%s", o.GPU, o.CPU, o.Threads, o.RAM, o.PCIe, o.Lanes, o.Geo, o.Inet, fast)
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
