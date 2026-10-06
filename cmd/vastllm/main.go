// VastLLM: a window with Up / Down that rents a GPU at Vast.ai and gives its OpenAI-compatible API on 127.0.0.1.
package main

// The manifest (common controls 6, DPI awareness) is linked from the .syso next to this file; after editing it:
//go:generate go run github.com/akavel/rsrc@v0.10.2 -arch amd64 -manifest vastllm.manifest -o rsrc_windows_amd64.syso

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/KirillTsuran/vast-llm/internal/app"
)

var version = "dev" // set by the release build: -ldflags "-X main.version=2.0.0"

func main() {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	tray := slices.Contains(os.Args[1:], "--tray")

	// one copy per PC: a second start only asks the first one to show its window
	_, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr("VastLLM-single-instance"))
	showName := windows.StringToUTF16Ptr("VastLLM-show")
	if err == windows.ERROR_ALREADY_EXISTS {
		if h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, showName); err == nil && !tray {
			windows.SetEvent(h)
		}
		return
	}
	show, _ := windows.CreateEvent(nil, 0, 0, showName)

	// every way the program can end leaves a line in the journal
	defer func() {
		if r := recover(); r != nil {
			app.Append(dir, fmt.Sprintf("АВАРИЙНОЕ ЗАВЕРШЕНИЕ: %v\n%s", r, debug.Stack()))
			os.Exit(2)
		}
		app.Append(dir, "процесс программы завершён")
	}()
	note := ""
	if tray {
		note = ", автозапуск в трей"
	}
	app.Append(dir, fmt.Sprintf("запуск VastLLM %s, pid %d%s", version, os.Getpid(), note))

	core := app.New(dir)
	if err := runWindow(core, tray, show); err != nil {
		app.Append(dir, "окно не создано: "+err.Error())
	}
}

// syncAutostart makes the HKCU Run entry follow config.json "autostart"; --tray starts the window hidden.
func syncAutostart(core *app.Core) {
	if version == "dev" { // a build from source run out of some folder must not take over the entry of the installed exe
		return
	}
	exe, _ := os.Executable()
	want := `"` + exe + `" --tray`
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		core.Log("автозапуск: " + err.Error())
		return
	}
	defer key.Close()
	have, _, err := key.GetStringValue("VastLLM")
	exists := err == nil
	switch {
	case core.Config().Autostart && have != want:
		if err = key.SetStringValue("VastLLM", want); err == nil {
			core.Log("автозапуск при входе в Windows включён")
		}
	case !core.Config().Autostart && exists:
		if err = key.DeleteValue("VastLLM"); err == nil {
			core.Log("автозапуск выключен")
		}
	default:
		return
	}
	if err != nil {
		core.Log("автозапуск: " + err.Error())
	}
}
