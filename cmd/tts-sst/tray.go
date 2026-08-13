package main

import (
	_ "embed"
	"os/exec"

	"github.com/TeeJS/tts-stt-windows/internal/config"
	"github.com/getlantern/systray"
)

//go:embed icon.ico
var trayIcon []byte

// runTray hosts the service behind a system tray icon. Model choice lives in the settings page
// (200+ voices across 50+ languages don't fit in a tray menu); the tray keeps the always-visible
// status line and the handful of one-click actions. Blocks until Quit.
func runTray(svc *service, ui *uiServer) {
	systray.Run(func() {
		systray.SetIcon(trayIcon)
		systray.SetTitle("tts-sst")
		systray.SetTooltip("tts-sst — local speech services")

		status := systray.AddMenuItem(svc.Status(), "Current state")
		status.Disable()
		systray.AddSeparator()

		settings := systray.AddMenuItem("Settings & models…", "Choose voices and speech models")
		autostart := systray.AddMenuItemCheckbox("Start with Windows", "Run tts-sst at login", autostartEnabled())
		openModels := systray.AddMenuItem("Open models folder", "")
		openLog := systray.AddMenuItem("Open log", "")
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Stop the speech services")

		refresh := func() {
			line := svc.Busy()
			if line == "" {
				line = svc.Status()
			}
			status.SetTitle(line)
			systray.SetTooltip("tts-sst — " + line)
		}
		svc.onChange = refresh
		refresh()

		go func() {
			for {
				select {
				case <-settings.ClickedCh:
					ui.Open()
				case <-autostart.ClickedCh:
					if autostart.Checked() {
						setAutostart(false)
						autostart.Uncheck()
					} else {
						setAutostart(true)
						autostart.Check()
					}
				case <-openModels.ClickedCh:
					exec.Command("explorer", svc.modelsDir).Start()
				case <-openLog.ClickedCh:
					exec.Command("notepad", logPath()).Start()
				case <-quit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()

		svc.Start()
		// First run: open settings so the language question is answered before models download.
		if !svc.cfg.Setup {
			ui.Open()
		}
	}, func() {
		_ = config.Save(svc.cfg)
	})
}
