//go:build windows

package config

import (
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SystemLocale returns the user's Windows display language split into its primary language and
// region ("de","DE" for de-DE; "en","GB" for en-GB), or empty strings if it can't be determined.
// It seeds the first-run question so most people can accept the defaults rather than hunting
// through fifty languages — and so an en-GB user is offered a British voice rather than whichever
// English accent happens to sort first.
func SystemLocale() (lang, region string) {
	const localeNameMaxLength = 85 // LOCALE_NAME_MAX_LENGTH from winnls.h
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetUserDefaultLocaleName")
	buf := make([]uint16, localeNameMaxLength)
	n, _, _ := proc.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return "", ""
	}
	// e.g. "en-GB", "pt-BR", "zh-Hans-CN" — the region is the last part when it's two letters.
	parts := strings.Split(syscall.UTF16ToString(buf[:n]), "-")
	lang = strings.ToLower(parts[0])
	if last := parts[len(parts)-1]; len(parts) > 1 && len(last) == 2 {
		region = strings.ToUpper(last)
	}
	return lang, region
}

// SystemLanguage is SystemLocale's language half.
func SystemLanguage() string {
	lang, _ := SystemLocale()
	return lang
}
