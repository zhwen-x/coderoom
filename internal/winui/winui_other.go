//go:build !windows

package winui

import "errors"

// No desktop integration off Windows. The development build keeps its terminal
// and never grows a tray icon, so every caller can invoke these unconditionally
// and get the behaviour it already had.

// HideConsole does nothing and reports that there was no console window to
// hide, which is what callers use to decide whether a tray icon is the only
// remaining way to reach the program.
func HideConsole() bool { return false }

func ShowConsole()         {}
func ConsoleVisible() bool { return true }

var errNoTray = errors.New("托盘图标仅在 Windows 上可用")

func Run(string, []MenuItem, func(Tray)) error { return errNoTray }
