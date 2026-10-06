package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"github.com/KirillTsuran/vast-llm/internal/app"
)

var phaseNames = map[string]string{
	"off": "Выключено", "renting": "Аренда", "booting": "Запуск", "downloading": "Скачивание модели",
	"loading": "Загрузка модели", "ready": "Работает", "stopping": "Удаление", "error": "Ошибка",
}

type window struct {
	core                    *app.Core
	mw                      *walk.MainWindow
	phase, msg, info        *walk.Label
	url                     *walk.LineEdit
	up, down, grafana       *walk.PushButton
	gpu                     *walk.ComboBox
	log                     *walk.TextEdit
	tray                    *walk.NotifyIcon
	exiting, loadingGPU     bool
	lastPhase, lastInfoText string
}

func runWindow(core *app.Core, startInTray bool, showEvent windows.Handle) error {
	w := &window{core: core}
	bold := Font{Family: "Segoe UI", PointSize: 14, Bold: true}
	if err := (MainWindow{
		AssignTo: &w.mw, Title: "VastLLM — GPU для ZCode", Size: Size{Width: 700, Height: 520}, MinSize: Size{Width: 560, Height: 360},
		Layout: VBox{}, Visible: false,
		Children: []Widget{
			Label{AssignTo: &w.phase, Font: bold},
			Label{AssignTo: &w.msg, TextColor: walk.RGB(110, 110, 110)},
			Label{AssignTo: &w.info},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				Label{Text: "ZCode URL:"},
				LineEdit{AssignTo: &w.url, ReadOnly: true},
				PushButton{Text: "Копировать", OnClicked: func() { walk.Clipboard().SetText(strings.Fields(w.url.Text())[0]) }},
			}},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				ComboBox{AssignTo: &w.gpu, Model: app.GPUs, OnCurrentIndexChanged: w.gpuChosen},
				PushButton{AssignTo: &w.up, Text: "Up", OnClicked: func() { core.Go("Up", core.Up) }},
				PushButton{AssignTo: &w.down, Text: "Down", OnClicked: func() { core.Go("Down", func() { core.Down("вручную") }) }},
				PushButton{AssignTo: &w.grafana, Text: "Grafana", OnClicked: func() {
					open(fmt.Sprintf("http://127.0.0.1:%d/d/llm-metrics", core.Config().GrafanaPort))
				}},
				PushButton{Text: "Папка и config.json", OnClicked: func() { open(core.Dir) }},
				PushButton{Text: "Логи", OnClicked: func() { open(filepath.Join(core.Dir, "logs")) }},
				HSpacer{},
			}},
			TextEdit{AssignTo: &w.log, ReadOnly: true, VScroll: true, Font: Font{Family: "Consolas", PointSize: 9}},
		},
	}).Create(); err != nil {
		return err
	}
	if err := w.makeTray(); err != nil {
		return err
	}
	defer w.tray.Dispose()

	core.OnLog = func(line string) { w.mw.Synchronize(func() { w.log.AppendText(line + "\r\n") }) }
	core.OnChange = func() { w.mw.Synchronize(w.refresh) }
	w.mw.Closing().Attach(w.closing)
	// minimizing hides the window: the program lives in the tray
	w.mw.SizeChanged().Attach(func() {
		if win.IsIconic(w.mw.Handle()) {
			w.mw.Hide()
			w.tray.ShowInfo("VastLLM", "Работает в трее")
		}
	})
	go func() { // a second copy of the program was started: show this window
		for {
			if e, _ := windows.WaitForSingleObject(showEvent, windows.INFINITE); e != windows.WAIT_OBJECT_0 {
				return
			}
			w.mw.Synchronize(w.show)
		}
	}()
	core.Go("ticks", func() {
		for seconds := 1; ; seconds++ {
			time.Sleep(time.Second)
			w.mw.Synchronize(w.refresh) // minutes rented and rubles spent move on their own
			if seconds%30 == 0 {
				core.Go("tick", core.Tick)
			}
		}
	})
	core.Go("startup", func() {
		core.Startup()
		w.mw.Synchronize(func() { syncAutostart(core); w.refresh() })
	})

	w.refresh()
	if !startInTray {
		w.mw.Show()
	}
	w.mw.Run()
	return nil
}

func open(target string) { exec.Command("explorer.exe", target).Start() }

func (w *window) show() {
	w.mw.Show()
	win.ShowWindow(w.mw.Handle(), win.SW_RESTORE)
	win.SetForegroundWindow(w.mw.Handle())
}

func (w *window) makeTray() (err error) {
	if w.tray, err = walk.NewNotifyIcon(w.mw); err != nil {
		return err
	}
	w.tray.SetIcon(walk.IconApplication())
	w.tray.MouseDown().Attach(func(_, _ int, button walk.MouseButton) {
		if button == walk.LeftButton {
			w.show()
		}
	})
	for _, item := range []struct {
		text string
		run  func()
	}{
		{"Открыть", w.show},
		{"Up", func() { w.core.Go("Up", w.core.Up) }},
		{"Down", func() { w.core.Go("Down", func() { w.core.Down("вручную") }) }},
		{"Выход", func() { w.mw.Close() }},
	} {
		action := walk.NewAction()
		action.SetText(item.text)
		action.Triggered().Attach(item.run)
		w.tray.ContextMenu().Actions().Add(action)
	}
	return w.tray.SetVisible(true)
}

// gpuChosen: the chosen GPU is used by the next Up.
func (w *window) gpuChosen() {
	if i := w.gpu.CurrentIndex(); !w.loadingGPU && i >= 0 {
		w.core.SetGPU(app.GPUs[i])
	}
}

func (w *window) refresh() {
	v := w.core.View()
	name := phaseNames[v.Phase]
	if v.Phase != w.lastPhase {
		w.lastPhase = v.Phase
		color := walk.RGB(255, 140, 0) // in progress
		switch v.Phase {
		case "ready":
			color = walk.RGB(46, 139, 87)
		case "error":
			color = walk.RGB(178, 34, 34)
		case "off":
			color = walk.RGB(0, 0, 0)
		}
		w.phase.SetTextColor(color)
		w.phase.SetText(name)
		w.tray.SetToolTip("VastLLM: " + name)
	}
	w.msg.SetText(v.Message)
	if url := fmt.Sprintf("http://127.0.0.1:%d/v1   модель: %s", v.Cfg.LocalPort, app.Model); w.url.Text() != url {
		w.url.SetText(url)
	}
	if i := slices.Index(app.GPUs, v.Cfg.GPU); i != w.gpu.CurrentIndex() {
		w.loadingGPU = true
		w.gpu.SetCurrentIndex(i)
		w.loadingGPU = false
	}
	if text := infoText(v); text != w.lastInfoText {
		w.lastInfoText = text
		w.info.SetText(text)
	}
	busy := slices.Contains([]string{"renting", "booting", "downloading", "loading", "stopping"}, v.Phase)
	w.up.SetEnabled(!busy && v.Phase != "ready")
	w.gpu.SetEnabled(!busy)
	w.down.SetEnabled(len(v.Machines) > 0 && v.Phase != "stopping")
	w.grafana.SetEnabled(v.Tunnel)
}

// infoText: what is rented, what it costs and how long the balance lasts.
func infoText(v app.View) string {
	rub := v.Cfg.UsdRub
	total := 0.0
	var b strings.Builder
	if len(v.Machines) == 0 {
		fmt.Fprintf(&b, "Ничего не арендовано · Up возьмёт самую дешёвую %s · курс %.2f ₽/$", v.Cfg.GPU, rub)
	} else {
		var lines []string
		for _, m := range v.Machines {
			total += m.Dph
			hours := time.Since(m.Created.Time).Hours()
			mark, note := "●", ""
			if !m.Main {
				mark, note = "○", " (запуск)"
			}
			lines = append(lines, fmt.Sprintf("%s #%d · %s · %s · %.1f ₽/ч · %d мин · ≈ %.0f ₽%s", mark, m.ID, m.GPU, m.Geo, m.Dph*rub, int(hours*60), hours*m.Dph*rub, note))
		}
		fmt.Fprintf(&b, "Арендовано в Vast (%d), всего %.1f ₽/ч:\n%s", len(v.Machines), total*rub, strings.Join(lines, "\n"))
		if v.Phase == "ready" {
			tunnel := "нет"
			if v.Tunnel {
				tunnel = "есть"
			}
			fmt.Fprintf(&b, "\nТуннель %s · без запросов %d мин · удаляется только кнопкой Down", tunnel, v.IdleMin)
		}
		if v.Machines[0].Main && v.Machines[0].GPU != v.Cfg.GPU {
			fmt.Fprintf(&b, "\nВыбрана %s: будет после Down → Up", v.Cfg.GPU)
		}
	}
	if v.Credit != nil {
		fmt.Fprintf(&b, "\nБаланс Vast: $%.2f (≈ %.0f ₽)", *v.Credit, *v.Credit*rub)
		if total > 0 {
			fmt.Fprintf(&b, " · хватит примерно на %.0f ч", *v.Credit/total)
		}
		if *v.Credit < 1 {
			b.WriteString(" · ⚠ пополните")
		}
	}
	return b.String()
}

// closing asks what to do with rented machines. Windows shutdown does not come here: the machines stay and are
// picked up on the next start.
func (w *window) closing(canceled *bool, _ walk.CloseReason) {
	if w.exiting {
		return
	}
	v := w.core.View()
	if len(v.Machines) == 0 {
		w.core.Log("выход из программы, машин нет")
		return
	}
	*canceled = true
	total := 0.0
	for _, m := range v.Machines {
		total += m.Dph
	}
	switch walk.MsgBox(w.mw, "VastLLM", fmt.Sprintf("Арендовано машин: %d (≈%.0f ₽/ч).\n\nДа — удалить машины и выйти\n"+
		"Нет — выйти, машины останутся и будут оплачиваться; при следующем запуске программа их подхватит\nОтмена — не выходить",
		len(v.Machines), total*v.Cfg.UsdRub), walk.MsgBoxYesNoCancel|walk.MsgBoxIconWarning) {
	case walk.DlgCmdYes:
		w.show()
		w.core.Go("Down", func() {
			w.core.Down("выход из программы")
			w.mw.Synchronize(func() { w.exiting = true; w.mw.Close() })
		})
	case walk.DlgCmdNo:
		w.core.Log("выход по кнопке «Нет»: машины остаются и оплачиваются, туннель закрыт")
		w.mw.Synchronize(func() { w.exiting = true; w.mw.Close() })
	}
}
