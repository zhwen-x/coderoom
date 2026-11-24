package winui

import (
	"os"
	"strings"
	"testing"
)

// The tray icon is loaded straight out of the embedded .ico, so this parser
// stands between a committed binary asset and a Win32 call that is handed a
// pointer and a length. Getting an offset wrong would read whatever follows in
// the binary, and the failure would only ever appear on a real desktop.
func TestBestIconImagePicksTheRightEntry(t *testing.T) {
	data, err := os.ReadFile("coderoom.ico")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 6 || data[2] != 1 {
		t.Fatalf("coderoom.ico 不是有效的图标文件")
	}
	count := int(data[4]) | int(data[5])<<8
	if count < 2 {
		t.Fatalf("图标只有 %d 个尺寸；托盘需要 16 与 32 两种", count)
	}

	for _, want := range []int{16, 32, 48} {
		offset, size, err := bestIconImage(data, want)
		if err != nil {
			t.Fatalf("bestIconImage(%d): %v", want, err)
		}
		if offset+size > len(data) {
			t.Fatalf("%d: 条目越界 offset=%d size=%d len=%d", want, offset, size, len(data))
		}
		// Each image starts with a BITMAPINFOHEADER whose width says which
		// entry was actually chosen - the real check that the directory was
		// read correctly rather than merely plausibly.
		img := data[offset : offset+size]
		if len(img) < 40 {
			t.Fatalf("%d: 图像数据过短 (%d)", want, len(img))
		}
		hdrSize := int(img[0]) | int(img[1])<<8 | int(img[2])<<16 | int(img[3])<<24
		if hdrSize != 40 {
			t.Errorf("%d: BITMAPINFOHEADER 大小为 %d，应为 40", want, hdrSize)
		}
		width := int(img[4]) | int(img[5])<<8 | int(img[6])<<16 | int(img[7])<<24
		if width != want {
			t.Errorf("要 %d 像素，取到的却是 %d 像素那张", want, width)
		}
	}

	// A size nothing matches exactly must still return the nearest rather than
	// failing: Windows asks for whatever the current DPI wants.
	offset, size, err := bestIconImage(data, 20)
	if err != nil {
		t.Fatalf("bestIconImage(20): %v", err)
	}
	img := data[offset : offset+size]
	width := int(img[4]) | int(img[5])<<8 | int(img[6])<<16 | int(img[7])<<24
	if width != 16 {
		t.Errorf("20 像素应取最接近的 16，取到 %d", width)
	}
}

func TestBestIconImageRejectsRubbish(t *testing.T) {
	for name, data := range map[string][]byte{
		"空":     {},
		"只有头":   {0, 0, 1, 0, 2, 0},
		"条目数为零": {0, 0, 1, 0, 0, 0},
		"偏移超出文件": append([]byte{0, 0, 1, 0, 1, 0},
			append(make([]byte, 8), []byte{0, 0, 0, 1, 0, 0, 0, 1}...)...),
	} {
		if _, _, err := bestIconImage(data, 32); err == nil {
			t.Errorf("%s：本应报错，却被接受", name)
		}
	}
}

// The window procedure is handed to the OS through windows.NewCallback, which
// requires every argument to be pointer-sized. A uint32 parameter there panics
// at registration - on a real desktop, where nothing in CI would see it. This
// checks the shape of the declaration rather than waiting to find out.
func TestWndProcSignatureIsCallbackSafe(t *testing.T) {
	src, err := os.ReadFile("winui_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	const want = "func (t *winTray) wndProc(hwnd, message, wparam, lparam uintptr) uintptr {"
	if !strings.Contains(string(src), want) {
		t.Errorf("wndProc 的签名必须全部是 uintptr，否则 NewCallback 会在注册时 panic；\n想要: %s", want)
	}
}
