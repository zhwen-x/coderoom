// Package winui is the small amount of Windows desktop integration CodeRoom
// needs: getting the console window out of the way, and putting a tray icon
// there instead.
//
// Everything is build-tagged, so the rest of the project calls these functions
// unconditionally and they do nothing on the development machine.
package winui

import "fmt"

// MenuItem is one entry in the tray icon's context menu. A Separator is drawn
// as a divider and ignores OnClick.
type MenuItem struct {
	Label     string
	Separator bool
	// Default marks the item run on a double-click of the icon itself, which
	// is what most people try first.
	Default bool
	OnClick func()
}

// Tray is what a caller holds while a tray icon is on screen.
type Tray interface {
	// SetTooltip replaces the hover text, which is where a server puts its
	// address so the teacher can read it without opening anything.
	SetTooltip(string)
	// Notify shows a balloon notification.
	Notify(title, body string)
	// Stop removes the icon and makes Run return.
	Stop()
}

// bestIconImage picks the entry closest to the wanted size from an ICO file.
func bestIconImage(data []byte, want int) (offset, size int, err error) {
	if len(data) < 6 {
		return 0, 0, fmt.Errorf("图标数据过短")
	}
	count := int(data[4]) | int(data[5])<<8
	if count == 0 || len(data) < 6+16*count {
		return 0, 0, fmt.Errorf("图标目录无效")
	}
	best := -1
	bestDelta := 1 << 30
	for i := 0; i < count; i++ {
		e := data[6+16*i:]
		w := int(e[0])
		if w == 0 {
			w = 256
		}
		delta := w - want
		if delta < 0 {
			delta = -delta
		}
		if delta < bestDelta {
			bestDelta = delta
			best = i
		}
	}
	e := data[6+16*best:]
	size = int(e[8]) | int(e[9])<<8 | int(e[10])<<16 | int(e[11])<<24
	offset = int(e[12]) | int(e[13])<<8 | int(e[14])<<16 | int(e[15])<<24
	if offset+size > len(data) {
		return 0, 0, fmt.Errorf("图标条目越界")
	}
	return offset, size, nil
}
