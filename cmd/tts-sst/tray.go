package main

import (
	_ "embed"
	"os/exec"

	"github.com/TeeJS/tts-stt-windows/internal/config"
	"github.com/TeeJS/tts-stt-windows/internal/models"
	"github.com/getlantern/systray"
)

//go:embed icon.ico
var trayIcon []byte

// runTray hosts the service behind a system tray icon: live status line, model/voice pickers,
// start-with-Windows toggle. Blocks until Quit.
func runTray(svc *service) {
	systray.Run(func() {
		systray.SetIcon(trayIcon)
		systray.SetTitle("tts-sst")
		systray.SetTooltip("tts-sst — local speech services")

		status := systray.AddMenuItem(svc.Status(), "Current state")
		status.Disable()
		systray.AddSeparator()

		sttMenu := systray.AddMenuItem("Speech-to-text model", "Pick the STT model")
		ttsMenu := systray.AddMenuItem("Voice", "Pick the TTS voice")
		type pick struct {
			item *systray.MenuItem
			name string
			kind models.Kind
		}
		var picks []pick
		for _, m := range models.Registry {
			switch m.Kind {
			case models.STT:
				picks = append(picks, pick{sttMenu.AddSubMenuItemCheckbox(m.Description, m.Name, false), m.Name, models.STT})
			case models.TTS:
				picks = append(picks, pick{ttsMenu.AddSubMenuItemCheckbox(m.Description, m.Name, false), m.Name, models.TTS})
			}
		}
		// The SAPI fallback: not in models.Registry (nothing to download), but a real, selectable
		// voice — the instant/zero-download option, always available.
		picks = append(picks, pick{
			ttsMenu.AddSubMenuItemCheckbox("Windows built-in (instant, robotic, no download)", sapiVoiceName, false),
			sapiVoiceName, models.TTS,
		})
		systray.AddSeparator()
		autostart := systray.AddMenuItemCheckbox("Start with Windows", "Run tts-sst at login", autostartEnabled())
		openModels := systray.AddMenuItem("Open models folder", "")
		openLog := systray.AddMenuItem("Open log", "")
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Stop the speech services")

		refresh := func() {
			status.SetTitle(svc.Status())
			systray.SetTooltip("tts-sst — " + svc.Status())
			for _, p := range picks {
				active := svc.ActiveSTT()
				if p.kind == models.TTS {
					active = svc.ActiveTTS()
				}
				if p.name == active {
					p.item.Check()
				} else {
					p.item.Uncheck()
				}
			}
		}
		svc.onChange = refresh
		refresh()

		for _, p := range picks {
			p := p
			go func() {
				for range p.item.ClickedCh {
					if p.kind == models.STT {
						go svc.SwitchSTT(p.name)
					} else {
						go svc.SwitchTTS(p.name)
					}
				}
			}()
		}
		go func() {
			for {
				select {
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
	}, func() {
		_ = config.Save(svc.cfg)
	})
}
