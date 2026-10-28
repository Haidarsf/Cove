package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"sync"
	"time"
	"unsafe"

	"fyne.io/systray"
)

var (
	trayMode bool
	quitOnce sync.Once
)

func uintptrOf[T any](p *T) uintptr { return uintptr(unsafe.Pointer(p)) }

func quit() {
	quitOnce.Do(func() {
		if trayMode {
			systray.Quit()
		} else {
			close(quitCh)
		}
	})
}

func dashboardURL(hash string) string {
	return fmt.Sprintf("http://127.0.0.1:%d/#%s", getConfig().Ports.API, hash)
}

func runTray() {
	trayMode = true
	systray.Run(onTrayReady, func() {})
}

const maxSlots = 30

func onTrayReady() {
	iconOn, iconOff := trayIcon(true), trayIcon(false)
	systray.SetIcon(iconOff)
	systray.SetTooltip("Cove")

	header := systray.AddMenuItem("Cove "+appVersion, "")
	header.Disable()
	mDash := systray.AddMenuItem("Open Dashboard", "Open the Cove dashboard")
	systray.AddSeparator()

	// Services
	mSvc := systray.AddMenuItem("Services", "")
	svcItems := map[string]*systray.MenuItem{}
	for _, s := range services {
		item := mSvc.AddSubMenuItemCheckbox(s.Label(), "Click to start/stop", false)
		svcItems[s.Name()] = item
		go func(name string, item *systray.MenuItem) {
			for range item.ClickedCh {
				if serviceMap[name].Status().State == "running" {
					stopService(name)
				} else if err := startService(name); err != nil {
					notifyErr(err)
				}
			}
		}(s.Name(), item)
	}
	mSvc.AddSeparator()
	mStartAll := mSvc.AddSubMenuItem("Start all", "")
	mStopAll := mSvc.AddSubMenuItem("Stop all", "")
	mRestartAll := mSvc.AddSubMenuItem("Restart all", "")

	// PHP versions
	mPHP := systray.AddMenuItem("PHP", "")
	var phpSlots []*systray.MenuItem
	var phpSlotVer = make([]string, maxSlots)
	for i := 0; i < 10; i++ {
		item := mPHP.AddSubMenuItemCheckbox("", "Make this the default PHP", false)
		item.Hide()
		phpSlots = append(phpSlots, item)
		go func(i int, item *systray.MenuItem) {
			for range item.ClickedCh {
				if v := phpSlotVer[i]; v != "" {
					if err := setDefaultPHP(v); err != nil {
						notifyErr(err)
					}
				}
			}
		}(i, item)
	}
	mPHP.AddSeparator()
	mPHPInstall := mPHP.AddSubMenuItem("Install another version…", "")
	mPHPIni := mPHP.AddSubMenuItem("Open PHP folder", "Edit php.local.ini here")

	// Sites
	mSites := systray.AddMenuItem("Sites", "")
	var siteSlots []*systray.MenuItem
	siteURLs := make([]string, maxSlots)
	for i := 0; i < maxSlots; i++ {
		item := mSites.AddSubMenuItem("", "Open in browser")
		item.Hide()
		siteSlots = append(siteSlots, item)
		go func(i int, item *systray.MenuItem) {
			for range item.ClickedCh {
				if siteURLs[i] != "" {
					openURL(siteURLs[i])
				}
			}
		}(i, item)
	}
	mSites.AddSeparator()
	mParked := mSites.AddSubMenuItem("Open parked folder", "")
	mManage := mSites.AddSubMenuItem("Manage sites…", "")

	// Logs
	mLogs := systray.AddMenuItem("Logs", "")
	logLinks := map[*systray.MenuItem]string{
		mLogs.AddSubMenuItem("nginx errors", ""): "nginx-error",
		mLogs.AddSubMenuItem("nginx access", ""): "nginx-access",
		mLogs.AddSubMenuItem("PHP errors", ""):   "php",
		mLogs.AddSubMenuItem("MySQL", ""):        "mysql",
		mLogs.AddSubMenuItem("Cove", ""):         "cove",
	}
	for item, name := range logLinks {
		go func(item *systray.MenuItem, name string) {
			for range item.ClickedCh {
				if name == "php" {
					name = "php-" + getConfig().DefaultPHP + "-errors"
				}
				openApp(("logs/" + name))
			}
		}(item, name)
	}
	mLogs.AddSeparator()
	mLogFolder := mLogs.AddSubMenuItem("Open logs folder", "")

	mMail := systray.AddMenuItem("Mail", "Caught outgoing mail")
	systray.AddSeparator()
	mAuto := systray.AddMenuItemCheckbox("Start at login", "", autostartEnabled())
	mQuit := systray.AddMenuItem("Quit Cove", "Stop all services and exit")

	refresh := func() {
		running, total := 0, 0
		for _, s := range services {
			st := s.Status()
			item := svcItems[s.Name()]
			label := s.Label()
			if st.Version != "" && st.Version != "built-in" {
				label += "  " + st.Version
			}
			switch st.State {
			case "running":
				item.Check()
			case "missing":
				label += "  (not installed)"
				item.Uncheck()
			case "error":
				label += "  ⚠ error"
				item.Uncheck()
			default:
				item.Uncheck()
			}
			item.SetTitle(label)
			if st.Installed {
				total++
				if st.State == "running" {
					running++
				}
			}
		}
		header.SetTitle(fmt.Sprintf("Cove — %d/%d services running", running, total))
		if running > 0 {
			systray.SetIcon(iconOn)
		} else {
			systray.SetIcon(iconOff)
		}
		systray.SetTooltip(fmt.Sprintf("Cove · %d/%d running · PHP %s", running, total, getConfig().DefaultPHP))

		def := getConfig().DefaultPHP
		vers := installedPHP()
		for i, item := range phpSlots {
			if i < len(vers) {
				v := vers[len(vers)-1-i]
				phpSlotVer[i] = v
				item.SetTitle(fmt.Sprintf("PHP %s  (%s)", v, phpVersion(v)))
				if v == def {
					item.Check()
				} else {
					item.Uncheck()
				}
				item.Show()
			} else {
				phpSlotVer[i] = ""
				item.Hide()
			}
		}

		sites := listSites()
		for i, item := range siteSlots {
			if i < len(sites) {
				s := sites[i]
				title := s.Host
				if s.Secure {
					title = "🔒 " + title
				}
				siteURLs[i] = s.URL
				item.SetTitle(title)
				item.Show()
			} else {
				siteURLs[i] = ""
				item.Hide()
			}
		}

		unread := 0
		for _, m := range listMail() {
			if !m.Read {
				unread++
			}
		}
		if unread > 0 {
			mMail.SetTitle(fmt.Sprintf("Mail  (%d new)", unread))
		} else {
			mMail.SetTitle("Mail")
		}
	}
	refresh()
	go func() {
		for range time.Tick(2 * time.Second) {
			refresh()
		}
	}()

	go func() {
		for {
			select {
			case <-mDash.ClickedCh:
				openApp(("overview"))
			case <-mStartAll.ClickedCh:
				startService("all")
			case <-mStopAll.ClickedCh:
				stopService("all")
			case <-mRestartAll.ClickedCh:
				stopService("all")
				startService("all")
			case <-mPHPInstall.ClickedCh:
				openApp(("php"))
			case <-mPHPIni.ClickedCh:
				revealPath(phpDir(getConfig().DefaultPHP))
			case <-mParked.ClickedCh:
				if p := getConfig().Paths; len(p) > 0 {
					revealPath(p[0])
				}
			case <-mManage.ClickedCh:
				openApp(("sites"))
			case <-mLogFolder.ClickedCh:
				revealPath(logDir())
			case <-mMail.ClickedCh:
				openApp(("mail"))
			case <-mAuto.ClickedCh:
				on := !mAuto.Checked()
				if setAutostart(on) == nil {
					if on {
						mAuto.Check()
					} else {
						mAuto.Uncheck()
					}
				}
			case <-mQuit.ClickedCh:
				log.Printf("quit from the tray menu")
				systray.SetTooltip("Cove — stopping…")
				shutdown()
				quit()
				return
			}
		}
	}()
}

func notifyErr(err error) {
	systray.SetTooltip("Cove — " + err.Error())
}

// trayIcon draws Cove's Bay logo at runtime (ICO wrapping a PNG): a C-shaped
// shoreline with water flowing in, on a deep teal tile, greyed out when
// every service is stopped.
func trayIcon(active bool) []byte {
	const n = 32
	pngBytes := iconPNG(active, n)
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, []uint16{0, 1, 1})
	ico.Write([]byte{n, n, 0, 0})
	binary.Write(&ico, binary.LittleEndian, []uint16{1, 32})
	binary.Write(&ico, binary.LittleEndian, []uint32{uint32(len(pngBytes)), 22})
	ico.Write(pngBytes)
	return ico.Bytes()
}

// iconPNG renders the Bay logo at any size. Shapes live on a 64-unit grid
// (matching ui/index.html's SVG); at tray sizes the shoreline gets thicker and
// the two waves merge into one bold wave so it stays readable at 16px.
func iconPNG(active bool, n int) []byte {
	scale := 64 / float64(n)
	tile := color.NRGBA{0x0B, 0x3B, 0x3C, 255}
	light, mid, pale := color.NRGBA{0x9F, 0xF0, 0xE3, 255}, color.NRGBA{0x14, 0xB8, 0xA6, 255}, color.NRGBA{0xE9, 0xFB, 0xF8, 255}
	if !active {
		tile = color.NRGBA{0x44, 0x44, 0x41, 255}
		light, mid, pale = color.NRGBA{0xB4, 0xB2, 0xA9, 255}, color.NRGBA{0x88, 0x87, 0x80, 255}, color.NRGBA{0xD3, 0xD1, 0xC7, 255}
	}
	roundRect := func(x, y, x0, y0, w, h, r float64) bool {
		cx := math.Max(x0+r, math.Min(x, x0+w-r))
		cy := math.Max(y0+r, math.Min(y, y0+h-r))
		return x >= x0 && x <= x0+w && y >= y0 && y <= y0+h && math.Hypot(x-cx, y-cy) <= r
	}
	// bay is a round-capped ring around (32,32), open ±45° to the right.
	bay := func(x, y, r, w float64) bool {
		if math.Abs(math.Atan2(y-32, x-32)) > math.Pi/4 {
			return math.Abs(math.Hypot(x-32, y-32)-r) <= w/2
		}
		e := r * math.Sqrt2 / 2
		return math.Hypot(x-32-e, y-32+e) <= w/2 || math.Hypot(x-32-e, y-32-e) <= w/2
	}
	// wave is a round-capped stroke of segs parabolic humps (the SVG's q/t
	// path): each hump is l wide with control point h off the baseline y0.
	wave := func(x, y, x0, y0 float64, segs int, l, h, w float64) bool {
		x1 := x0 + float64(segs)*l
		if x < x0-w || x > x1+w || math.Abs(y-y0) > h/2+w {
			return false
		}
		// Nearest point by scanning the curve within a stroke width of x.
		for cx := math.Max(x0, x-w/2); cx <= math.Min(x1, x+w/2); cx += 0.05 {
			i := math.Min(math.Floor((cx-x0)/l), float64(segs-1))
			u, s := (cx-x0)/l-i, 1-2*math.Mod(i, 2)
			if math.Hypot(x-cx, y-(y0-s*2*h*u*(1-u))) <= w/2 {
				return true
			}
		}
		return math.Hypot(x-x1, y-y0) <= w/2
	}
	small := n <= 24
	// paint returns the colour at one sample point, topmost layer first.
	paint := func(x, y float64) (color.NRGBA, bool) {
		if small {
			if wave(x, y, 26, 32, 3, 9, 6, 7) {
				return pale, true
			}
			if bay(x, y, 17, 10) {
				return mid, true
			}
		} else {
			if wave(x, y, 25, 28.5, 4, 6.5, 3.5, 4.5) {
				return pale, true
			}
			if wave(x, y, 27, 37.5, 2, 6.5, 3.5, 4.5) {
				return light, true
			}
			if bay(x, y, 17, 8) {
				return mid, true
			}
		}
		if roundRect(x, y, 0, 0, 64, 64, 14) {
			return tile, true
		}
		return color.NRGBA{}, false
	}
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			// 4x supersampling for smooth edges.
			var r, g, b, hits int
			for sy := 0; sy < 4; sy++ {
				for sx := 0; sx < 4; sx++ {
					fx, fy := (float64(x)+(float64(sx)+0.5)/4)*scale, (float64(y)+(float64(sy)+0.5)/4)*scale
					if c, ok := paint(fx, fy); ok {
						r, g, b, hits = r+int(c.R), g+int(c.G), b+int(c.B), hits+1
					}
				}
			}
			if hits > 0 {
				img.SetNRGBA(x, y, color.NRGBA{uint8(r / hits), uint8(g / hits), uint8(b / hits), uint8(255 * hits / 16)})
			}
		}
	}
	var pngBuf bytes.Buffer
	png.Encode(&pngBuf, img)
	return pngBuf.Bytes()
}
