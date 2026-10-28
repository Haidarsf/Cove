package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// runApp is CoveApp.exe: the dashboard in its own native window (WebView2, built
// into Windows 11) instead of a browser tab. It starts the background service
// first if it isn't running, and only ever keeps one window open.
func runApp() {
	user32 := windows.NewLazySystemDLL("user32.dll")
	if hwnd := findWindow(user32, "Cove"); hwnd != 0 {
		user32.NewProc("ShowWindow").Call(hwnd, 9 /* SW_RESTORE */)
		user32.NewProc("SetForegroundWindow").Call(hwnd)
		return
	}
	if !running() {
		startDaemonDetached()
		for i := 0; i < 50 && !running(); i++ {
			time.Sleep(150 * time.Millisecond)
		}
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  dir("webview"),
		WindowOptions: webview2.WindowOptions{
			Title: "Cove", Width: 1240, Height: 820, Center: true,
		},
	})
	if w == nil {
		// No WebView2 runtime: fall back to the browser so the user still gets in.
		openURL(apiBase())
		return
	}
	defer w.Destroy()
	setWindowIcon(user32, uintptr(w.Window()))
	themeTitleBar(uintptr(w.Window()))
	w.SetSize(900, 600, webview2.HintMin)
	// Site links and external pages open in the user's real browser, not in here.
	w.Bind("coveOpenExternal", func(url string) { openURL(url) })
	hash := "#home"
	if len(os.Args) > 1 && len(os.Args[1]) > 1 && os.Args[1][0] == '#' {
		hash = os.Args[1]
	}
	w.Navigate(apiBase() + "/" + hash)
	w.Run()
}

func findWindow(user32 *windows.LazyDLL, title string) uintptr {
	t, _ := windows.UTF16PtrFromString(title)
	cls, _ := windows.UTF16PtrFromString("webview")
	hwnd, _, _ := user32.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(t)))
	return hwnd
}

// setWindowIcon gives the window and its taskbar button the Cove badge, built
// from the same renderer as the tray icon (Windows accepts PNG icon data).
func setWindowIcon(user32 *windows.LazyDLL, hwnd uintptr) {
	create := user32.NewProc("CreateIconFromResourceEx")
	send := user32.NewProc("SendMessageW")
	for _, size := range []struct {
		px   int
		kind uintptr
	}{{32, 0 /* ICON_SMALL */}, {256, 1 /* ICON_BIG */}} {
		png := iconPNG(true, size.px)
		hicon, _, _ := create.Call(uintptr(unsafe.Pointer(&png[0])), uintptr(len(png)), 1, 0x00030000, uintptr(size.px), uintptr(size.px), 0)
		if hicon != 0 {
			send.Call(hwnd, 0x0080 /* WM_SETICON */, size.kind, hicon)
		}
	}
}

// themeTitleBar paints the native caption in the dashboard's own background and
// text colours (Windows 11 DWM attributes), so the title bar reads as part of
// the app instead of a white strip. It follows the Windows light/dark setting,
// like the page does via prefers-color-scheme.
func themeTitleBar(hwnd uintptr) {
	set := windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
	attr := func(id uintptr, v uint32) { set.Call(hwnd, id, uintptr(unsafe.Pointer(&v)), 4) }
	rgb := func(r, g, b uint32) uint32 { return r | g<<8 | b<<16 } // COLORREF is 0x00BBGGRR

	dark := true
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetIntegerValue("AppsUseLightTheme"); err == nil && v == 1 {
			dark = false
		}
		k.Close()
	}
	const useDarkMode, borderColor, captionColor, textColor = 20, 34, 35, 36
	if dark {
		attr(useDarkMode, 1)
		attr(captionColor, rgb(0x0a, 0x16, 0x15)) // --bg (dark)
		attr(borderColor, rgb(0x1f, 0x35, 0x33))  // --line
		attr(textColor, rgb(0xa9, 0xc1, 0xbe))    // --ink-2
	} else {
		attr(useDarkMode, 0)
		attr(captionColor, rgb(0xee, 0xf3, 0xf2))
		attr(borderColor, rgb(0xdb, 0xe6, 0xe4))
		attr(textColor, rgb(0x3f, 0x5a, 0x58))
	}
}

// startDaemonDetached launches CoveTray.exe (service + tray icon) next to this exe.
func startDaemonDetached() {
	exe, _ := os.Executable()
	tray := filepath.Join(filepath.Dir(exe), "CoveTray.exe")
	var cmd *exec.Cmd
	if fileExists(tray) {
		cmd = exec.Command(tray)
	} else {
		cmd = exec.Command(filepath.Join(filepath.Dir(exe), "cove.exe"), "__daemon", "tray")
	}
	cmd.SysProcAttr = hidden(cmd).SysProcAttr
	cmd.SysProcAttr.CreationFlags |= 0x00000008 // DETACHED_PROCESS
	if err := cmd.Start(); err == nil {
		cmd.Process.Release()
	}
}

// openApp shows the desktop window, falling back to the browser if CoveApp.exe is missing.
func openApp(hash string) {
	exe, _ := os.Executable()
	app := filepath.Join(filepath.Dir(exe), "CoveApp.exe")
	if !fileExists(app) {
		openURL(dashboardURL(hash))
		return
	}
	if err := exec.Command(app, "#"+hash).Start(); err != nil {
		openURL(dashboardURL(hash))
	}
}
