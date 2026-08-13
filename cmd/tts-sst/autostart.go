package main

import (
	"log"
	"os"

	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "tts-sst"

// Start-with-Windows via the per-user Run key — no admin rights, no scheduled task, and the
// user can also see/remove it in Task Manager's Startup tab like any normal app.

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValue)
	return err == nil
}

func setAutostart(enable bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		log.Printf("autostart: open Run key: %v", err)
		return
	}
	defer k.Close()
	if !enable {
		if err := k.DeleteValue(runValue); err != nil {
			log.Printf("autostart: remove: %v", err)
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("autostart: %v", err)
		return
	}
	if err := k.SetStringValue(runValue, `"`+exe+`"`); err != nil {
		log.Printf("autostart: set: %v", err)
	}
}
