//go:build windows

package config

import (
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SystemLanguage returns the primary language code of the user's Windows display language
// ("de" for de-DE, "pt" for pt-BR), or "" if it can't be determined. It seeds the first-run
// language question so most people can accept the default rather than hunting for their own
// language in a list of fifty.
func SystemLanguage() string {
	const localeNameMaxLength = 85 // LOCALE_NAME_MAX_LENGTH from winnls.h
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetUserDefaultLocaleName")
	buf := make([]uint16, localeNameMaxLength)
	n, _, _ := proc.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	name := syscall.UTF16ToString(buf[:n]) // e.g. "en-GB", "pt-BR", "zh-Hans-CN"
	if i := strings.IndexAny(name, "-_"); i > 0 {
		return strings.ToLower(name[:i])
	}
	return strings.ToLower(name)
}
