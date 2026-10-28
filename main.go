package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const appVersion = "1.0.0"

// guiBuild is set via -ldflags "-X main.guiBuild=1" for CoveTray.exe.
var guiBuild = ""

const (
	cReset  = "\x1b[0m"
	cDim    = "\x1b[2m"
	cBold   = "\x1b[1m"
	cAccent = "\x1b[38;5;43m"
	cGreen  = "\x1b[32m"
	cRed    = "\x1b[31m"
	cYellow = "\x1b[33m"
	cCyan   = "\x1b[36m"
)

func main() {
	args := os.Args[1:]
	if guiBuild == "app" {
		runApp()
		return
	}
	if guiBuild == "1" && len(args) == 0 {
		runDaemon(true)
		return
	}
	enableVT()
	if len(args) == 0 {
		usage()
		return
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "__daemon":
		runDaemon(len(rest) > 0 && rest[0] == "tray")
	case "__nrpt":
		err = exec.Command("powershell", "-NoProfile", "-Command", nrptScript(getConfig().TLD)).Run()
	case "__icon": // used by build.ps1 to embed the app icon
		err = os.WriteFile(rest[0], iconPNG(true, 256), 0o644)
	case "__hosts":
		err = writeHosts()
	case "serve":
		fmt.Println(cAccent + "Cove running in foreground — Ctrl+C to stop" + cReset)
		runDaemon(false)
	case "setup":
		err = cmdSetup()
	case "up", "tray":
		err = cmdUp()
	case "down", "quit":
		_, err = call("POST", "/api/quit", nil)
		if err == nil {
			ok("Cove stopped")
		}
	case "status", "st":
		err = cmdStatus()
	case "start", "stop", "restart":
		name := "all"
		if len(rest) > 0 {
			name = rest[0]
		}
		if cmd == "start" && !running() {
			err = cmdUp() // autostart services come up with the daemon
			if name == "all" || err != nil {
				break
			}
		}
		_, err = call("POST", "/api/services/"+name+"/"+cmd, nil)
		if err == nil {
			ok(fmt.Sprintf("%s %s", name, map[string]string{"start": "started", "stop": "stopped", "restart": "restarted"}[cmd]))
			cmdStatus()
		}
	case "install":
		err = cmdInstall(rest)
	case "php", "use":
		err = cmdPHP(rest)
	case "isolate":
		if len(rest) == 0 {
			err = fmt.Errorf("usage: cove isolate <php-version> [site]")
			break
		}
		err = siteCall("/api/sites/isolate", rest[1:], map[string]any{"php": rest[0]}, "now uses PHP "+rest[0])
	case "unisolate":
		err = siteCall("/api/sites/isolate", rest, map[string]any{"php": ""}, "now follows the default PHP")
	case "sites", "ls", "links", "parked":
		err = cmdSites()
	case "link":
		err = cmdLink(rest)
	case "unlink":
		err = siteCall("/api/sites/unlink", rest, nil, "unlinked")
	case "park":
		err = cmdPark(rest, false)
	case "unpark", "forget":
		err = cmdPark(rest, true)
	case "paths":
		err = cmdPaths()
	case "secure":
		err = siteCall("/api/sites/secure", rest, map[string]any{"secure": true}, "is now served over HTTPS 🔒")
	case "unsecure":
		err = siteCall("/api/sites/secure", rest, map[string]any{"secure": false}, "is now served over HTTP")
	case "open":
		err = cmdOpen(rest)
	case "dashboard", "ui":
		openURL(fmt.Sprintf("http://127.0.0.1:%d", getConfig().Ports.API))
	case "logs", "log", "tail":
		err = cmdLogs(rest)
	case "db":
		err = cmdDB(rest)
	case "new":
		err = cmdNew(rest)
	case "trust":
		err = trustCA()
		if err == nil {
			ok("Cove CA trusted — HTTPS sites show a padlock")
		}
	case "dns":
		err = runElevated("__nrpt")
		if err == nil {
			ok("Windows now resolves *." + getConfig().TLD + " through Cove")
		}
	case "hosts":
		err = runElevated("__hosts")
		if err == nil {
			ok("hosts file updated")
		}
	case "shortcuts":
		err = createShortcuts()
		if err == nil {
			ok("Cove added to the Start menu and Desktop")
		}
	case "app":
		openApp("home")
	case "autostart":
		on := len(rest) == 0 || rest[0] != "off"
		err = setAutostart(on)
		if err == nil {
			ok(map[bool]string{true: "Cove starts at login", false: "autostart disabled"}[on])
		}
	case "doctor":
		cmdDoctor()
	case "home":
		fmt.Println(Home())
	case "version", "-v", "--version":
		fmt.Println("Cove " + appVersion)
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q — run `cove help`", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, cRed+"✗ "+cReset+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Printf(`%s≋ Cove %s%s  — fast local PHP dev environment for Windows

%sGETTING STARTED%s
  cove setup                    Download latest PHP, nginx, MySQL, Composer and configure everything
  cove up | down                Start / stop Cove (tray icon + all services)
  cove dashboard                Open the web dashboard (also at http://cove.test)

%sSERVICES%s
  cove status                   Show services, versions and ports
  cove start|stop|restart [svc] svc = all | nginx | php | mysql | mail | dns
  cove logs [name] [-f] [-n 50] Show or follow a log (nginx-error, nginx-access, php-8.4-errors, mysql, cove…)

%sPHP%s
  cove php                      List installed PHP versions
  cove php 8.4                  Switch default PHP (installs it if needed)
  cove install php 8.3          Install another PHP version side-by-side
  cove isolate 8.3 [site]       Pin a site to a PHP version  (unisolate to undo)

%sSITES%s
  cove sites                    List sites
  cove park [dir]               Serve every folder in dir as <folder>.test  (unpark to undo)
  cove link [name]              Serve the current folder as name.test       (unlink to undo)
  cove secure [site]            Serve over HTTPS with a trusted cert         (unsecure to undo)
  cove open [site]              Open a site in your browser
  cove new <name>               Create a new Laravel app in your parked folder

%sMORE%s
  cove db [create|drop <name>]  List or manage MySQL databases
  cove install nginx|mysql|composer   Upgrade a component to the latest release
  cove autostart on|off         Launch Cove at Windows login
  cove trust | dns | hosts      Re-run CA trust / .test DNS rule / hosts-file fallback
  cove doctor                   Diagnose ports, DNS and certificates
`, cAccent+cBold, appVersion, cReset, cBold, cReset, cBold, cReset, cBold, cReset, cBold, cReset, cBold, cReset)
}

func ok(msg string)   { fmt.Println(cGreen + "✓ " + cReset + msg) }
func info(msg string) { fmt.Println(cAccent + "→ " + cReset + msg) }

func enableVT() {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
	windows.SetConsoleOutputCP(65001)
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	select {
	case <-ch:
	case <-quitCh:
	}
}

var quitCh = make(chan struct{})

// ---------- API client ----------

func apiBase() string { return fmt.Sprintf("http://127.0.0.1:%d", getConfig().Ports.API) }

func call(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, apiBase()+path, rdr)
	req.Header.Set("X-Cove", "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("Cove is not running — start it with `cove up`")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	return b, nil
}

func running() bool {
	c := http.Client{Timeout: 700 * time.Millisecond}
	resp, err := c.Get(apiBase() + "/api/status")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// ---------- commands ----------

func cmdUp() error {
	if running() {
		ok("Cove is already running")
		return nil
	}
	exe, _ := os.Executable()
	tray := filepath.Join(filepath.Dir(exe), "CoveTray.exe")
	var cmd *exec.Cmd
	if fileExists(tray) {
		cmd = exec.Command(tray)
	} else {
		cmd = exec.Command(exe, "__daemon", "tray")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200 /* DETACHED | NEW_PROCESS_GROUP */, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	cmd.Process.Release()
	for i := 0; i < 40 && !running(); i++ {
		time.Sleep(150 * time.Millisecond)
	}
	if !running() {
		return fmt.Errorf("Cove did not start — see %s", filepath.Join(logDir(), "cove.log"))
	}
	ok("Cove is up — tray icon added")
	time.Sleep(1500 * time.Millisecond)
	return cmdStatus()
}

func cmdStatus() error {
	b, err := call("GET", "/api/status", nil)
	if err != nil {
		return err
	}
	var st struct {
		Services   []ServiceStatus
		DefaultPHP string `json:"default_php"`
		Sites      int
		TLD        string
		Ports      Ports
		MailUnread int `json:"mail_unread"`
	}
	json.Unmarshal(b, &st)
	fmt.Println()
	for _, s := range st.Services {
		dot := map[string]string{"running": cGreen + "●", "starting": cYellow + "◐", "error": cRed + "●", "missing": cDim + "○"}[s.State]
		if dot == "" {
			dot = cDim + "●"
		}
		extra := ""
		if s.Error != "" {
			extra = cRed + "  " + s.Error + cReset
		}
		fmt.Printf("  %s%s  %-12s %s%-10s%s  %-18s %s%s\n", dot, cReset, s.Label, cDim, s.State, cReset, s.Version, cDim+s.Port+cReset, extra)
	}
	fmt.Printf("\n  %sPHP %s · %d sites · dashboard http://cove.%s · mail unread %d%s\n\n", cDim, st.DefaultPHP, st.Sites, st.TLD, st.MailUnread, cReset)
	return nil
}

func cmdInstall(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cove install php [version] | nginx | mysql | composer")
	}
	branch := ""
	if len(args) > 1 {
		branch = args[1]
	}
	if running() {
		// Let the daemon do it so it can pause/resume the affected service.
		body, _ := json.Marshal(map[string]string{"what": args[0], "branch": branch})
		req, _ := http.NewRequest("POST", apiBase()+"/api/install", bytes.NewReader(body))
		req.Header.Set("X-Cove", "1")
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(os.Stdout, resp.Body)
		return err
	}
	return runInstall(args[0], branch, os.Stdout)
}

func cmdPHP(args []string) error {
	if len(args) == 0 {
		c := getConfig()
		installed := installedPHP()
		if len(installed) == 0 {
			fmt.Println("No PHP installed — run `cove install php`")
		}
		for _, v := range installed {
			mark := "  "
			if v == c.DefaultPHP {
				mark = cGreen + "● " + cReset
			}
			fmt.Printf("  %s%-5s %s%s%s\n", mark, v, cDim, phpVersion(v), cReset)
		}
		if rels, err := availablePHP(); err == nil {
			var more []string
			for _, r := range rels {
				if !containsStr(installed, r.Branch) {
					more = append(more, r.Branch)
				}
			}
			if len(more) > 0 {
				fmt.Printf("\n  %savailable: %s  (cove install php <version>)%s\n", cDim, strings.Join(more, ", "), cReset)
			}
		}
		return nil
	}
	v := strings.TrimPrefix(args[0], "php@")
	if !fileExists(filepath.Join(phpDir(v), "php-cgi.exe")) {
		info("PHP " + v + " is not installed yet — installing")
		if err := cmdInstall([]string{"php", v}); err != nil {
			return err
		}
	}
	if running() {
		if _, err := call("POST", "/api/php/default", map[string]string{"branch": v}); err != nil {
			return err
		}
	} else {
		updateConfig(func(c *Config) { c.DefaultPHP = v })
		writeShims()
	}
	ok("Default PHP is now " + phpVersion(v) + "  (php, composer and all non-isolated sites)")
	return nil
}

// siteCall sends a site action for the named site, or the site in the current folder.
func siteCall(path string, args []string, extra map[string]any, msg string) error {
	body := map[string]any{}
	for k, v := range extra {
		body[k] = v
	}
	if len(args) > 0 {
		body["name"] = args[0]
	} else {
		wd, _ := os.Getwd()
		body["path"] = wd
	}
	b, err := call("POST", path, body)
	if err != nil {
		return err
	}
	var s Site
	json.Unmarshal(b, &s)
	ok(fmt.Sprintf("%s %s", cBold+s.Host+cReset, msg))
	return nil
}

func cmdSites() error {
	b, err := call("GET", "/api/sites", nil)
	var sites []Site
	if err == nil {
		json.Unmarshal(b, &sites)
	} else {
		sites = listSites()
	}
	if len(sites) == 0 {
		fmt.Println("No sites yet. Create a folder in", strings.Join(getConfig().Paths, ", "), "or run `cove link` in a project.")
		return nil
	}
	fmt.Println()
	for _, s := range sites {
		lock := "  "
		if s.Secure {
			lock = "🔒"
		}
		php := s.PHP
		if s.Isolated {
			php += "*"
		}
		fmt.Printf("  %s %-38s %s%-6s %-10s %s%s\n", lock, cAccent+s.URL+cReset, cDim, php, s.Framework, s.Path, cReset)
	}
	fmt.Printf("\n  %s* isolated PHP version%s\n\n", cDim, cReset)
	return nil
}

func cmdLink(args []string) error {
	wd, _ := os.Getwd()
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	b, err := call("POST", "/api/sites/link", map[string]string{"name": name, "path": wd})
	if err != nil {
		return err
	}
	var s Site
	json.Unmarshal(b, &s)
	ok("Linked " + wd + " → " + cAccent + s.URL + cReset)
	return nil
}

func cmdPark(args []string, remove bool) error {
	p, _ := os.Getwd()
	if len(args) > 0 {
		p = args[0]
	}
	if _, err := call("POST", "/api/paths", map[string]any{"path": p, "remove": remove}); err != nil {
		return err
	}
	if remove {
		ok("No longer serving folders in " + p)
	} else {
		ok("Every folder in " + p + " is now a ." + getConfig().TLD + " site")
	}
	return nil
}

func cmdPaths() error {
	for _, p := range getConfig().Paths {
		fmt.Println("  " + p)
	}
	return nil
}

func cmdOpen(args []string) error {
	var s Site
	var found bool
	if len(args) > 0 {
		s, found = findSite(args[0])
	} else {
		wd, _ := os.Getwd()
		s, found = siteForDir(wd)
	}
	if !found {
		return fmt.Errorf("no matching site — see `cove sites`")
	}
	openURL(s.URL)
	ok("Opening " + s.URL)
	return nil
}

func cmdLogs(args []string) error {
	follow, n, name := false, 40, ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-f", "--follow":
			follow = true
		case "-n":
			if i+1 < len(args) {
				n, _ = strconv.Atoi(args[i+1])
				i++
			}
		default:
			name = args[i]
		}
	}
	if name == "" {
		fmt.Println("Available logs:")
		for _, l := range logNames() {
			fmt.Println("  " + l)
		}
		fmt.Printf("\n%susage: cove logs <name> [-f] [-n 100]   (`cove logs php` follows PHP %s errors)%s\n", cDim, getConfig().DefaultPHP, cReset)
		return nil
	}
	if name == "php" {
		name = "php-" + getConfig().DefaultPHP + "-errors"
	}
	if name == "nginx" {
		name = "nginx-error"
	}
	p, exists := logPath(name)
	if !exists {
		return fmt.Errorf("no log named %q — run `cove logs` to list them", name)
	}
	text, off := tailFile(p, n)
	fmt.Print(colorLog(text))
	if !follow {
		fmt.Println()
		return nil
	}
	fmt.Printf("%s— following %s (Ctrl+C to stop) —%s\n", cDim, filepath.Base(p), cReset)
	for {
		time.Sleep(300 * time.Millisecond)
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.Size() < off {
			off = 0
		}
		if fi.Size() > off {
			f, _ := os.Open(p)
			f.Seek(off, io.SeekStart)
			b, _ := io.ReadAll(f)
			f.Close()
			off += int64(len(b))
			fmt.Print(colorLog(string(b)))
		}
	}
}

func colorLog(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		ll := strings.ToLower(l)
		switch {
		case strings.Contains(ll, "fatal") || strings.Contains(ll, "[error]") || strings.Contains(ll, "[emerg]") || strings.Contains(ll, " 500 "):
			lines[i] = cRed + l + cReset
		case strings.Contains(ll, "warn") || strings.Contains(ll, "deprecated") || strings.Contains(ll, " 404 "):
			lines[i] = cYellow + l + cReset
		}
	}
	return strings.Join(lines, "\n")
}

func cmdDB(args []string) error {
	if len(args) >= 2 && (args[0] == "create" || args[0] == "drop") {
		var err error
		if args[0] == "create" {
			_, err = call("POST", "/api/db", map[string]string{"name": args[1]})
		} else {
			_, err = call("DELETE", "/api/db/"+args[1], nil)
		}
		if err == nil {
			ok(fmt.Sprintf("Database %s %sd", args[1], args[0]))
		}
		return err
	}
	b, err := call("GET", "/api/db", nil)
	if err != nil {
		return err
	}
	var dbs []struct {
		Name, Tables string
		SizeMB       string `json:"size_mb"`
		System       bool
	}
	json.Unmarshal(b, &dbs)
	for _, d := range dbs {
		if !d.System {
			fmt.Printf("  %-30s %s%s tables · %s MB%s\n", d.Name, cDim, d.Tables, d.SizeMB, cReset)
		}
	}
	fmt.Printf("\n  %smysql -u root -h 127.0.0.1 -P %d  (no password)%s\n", cDim, getConfig().Ports.MySQL, cReset)
	return nil
}

func cmdNew(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cove new <name>")
	}
	name := slug(args[0])
	c := getConfig()
	if len(c.Paths) == 0 {
		return fmt.Errorf("no parked folder — run `cove park <dir>` first")
	}
	target := filepath.Join(c.Paths[0], name)
	info("Creating Laravel app in " + target)
	cmd := exec.Command(filepath.Join(binDir(), "composer.cmd"), "create-project", "laravel/laravel", target, "--prefer-dist")
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		return err
	}
	call("POST", "/api/reload", nil)
	ok("Ready at " + cAccent + "http://" + name + "." + c.TLD + cReset)
	return nil
}

// ---------- setup ----------

func cmdSetup() error {
	fmt.Println(cAccent + cBold + "\n≋ Setting up Cove\n" + cReset)
	if running() {
		call("POST", "/api/quit", nil)
		time.Sleep(time.Second)
	}
	loadConfig()
	saveConfig()

	// 1. Put cove.exe + CoveTray.exe in a stable place on PATH.
	exe, _ := os.Executable()
	bd := binDir()
	if !strings.EqualFold(filepath.Dir(exe), bd) {
		for _, f := range []string{"cove.exe", "CoveTray.exe", "CoveApp.exe"} {
			src := filepath.Join(filepath.Dir(exe), f)
			if fileExists(src) {
				b, _ := os.ReadFile(src)
				os.WriteFile(filepath.Join(bd, f), b, 0o755)
			}
		}
	}
	if err := addToUserPath(bd); err != nil {
		fmt.Println(cYellow + "! could not update PATH: " + err.Error() + cReset)
	} else {
		ok("Added " + bd + " to your PATH (open a new terminal to use php, composer, mysql, cove)")
	}

	// 2. Binaries.
	steps := []struct {
		have bool
		run  func() error
	}{
		{len(installedPHP()) > 0, func() error { return installPHP("latest", os.Stdout) }},
		{fileExists(filepath.Join(nginxDir(), "nginx.exe")), func() error { return installNginx(os.Stdout) }},
		{fileExists(filepath.Join(mysqlDir(), "bin", "mysqld.exe")), func() error { return installMySQL(os.Stdout) }},
		{fileExists(filepath.Join(binDir(), "composer.phar")), func() error { return installComposer(os.Stdout) }},
	}
	for _, s := range steps {
		if !s.have {
			if err := s.run(); err != nil {
				return err
			}
		}
	}
	if err := installCABundle(os.Stdout); err != nil {
		fmt.Println(cYellow + "! CA bundle: " + err.Error() + cReset)
	}
	ensureCA()
	rebuildCABundle()
	for _, v := range installedPHP() {
		writePHPIni(v)
	}
	writeShims()

	// 3. Trust the local CA (one Windows dialog, no admin).
	info("Trusting the Cove certificate authority — click Yes in the Windows dialog")
	if err := trustCA(); err != nil {
		fmt.Println(cYellow + "! CA not trusted (run `cove trust` later): " + err.Error() + cReset)
	} else {
		ok("Local HTTPS certificates trusted")
	}

	// 4. *.test DNS via NRPT (one UAC prompt).
	tld := getConfig().TLD
	if nrptInstalled(tld) {
		ok("*." + tld + " DNS already configured")
	} else {
		info("Routing *." + tld + " to Cove's DNS — approve the admin prompt")
		if err := runElevated("__nrpt"); err != nil || !nrptInstalled(tld) {
			fmt.Println(cYellow + "! DNS rule not added. Use `cove dns` to retry, or `cove hosts` for the hosts-file fallback." + cReset)
		} else {
			ok("*." + tld + " now resolves to 127.0.0.1")
		}
	}

	setAutostart(true)
	ok("Cove will start at login (cove autostart off to disable)")
	if err := createShortcuts(); err == nil {
		ok("Cove added to the Start menu and Desktop")
	}

	// 5. Laravel installer.
	if fileExists(filepath.Join(binDir(), "composer.phar")) {
		info("Installing the Laravel installer globally")
		cmd := exec.Command(filepath.Join(binDir(), "composer.cmd"), "global", "require", "laravel/installer", "--quiet")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Run()
	}

	cmdDoctor()
	if err := cmdUp(); err != nil {
		return err
	}
	fmt.Printf("%sParked folder:%s %s — every folder inside is served at <folder>.%s\n", cBold, cReset, getConfig().Paths[0], tld)
	fmt.Printf("%sDashboard:%s     http://cove.%s\n\n", cBold, cReset, tld)
	return nil
}

func addToUserPath(d string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	cur, _, _ := k.GetStringValue("Path")
	// Put Cove first and drop entries that would shadow it with a dead or
	// foreign PHP: a removed WAMP install, or an older Cove location.
	keep := []string{d}
	for _, p := range strings.Split(cur, ";") {
		lp := strings.ToLower(strings.TrimRight(p, `\`))
		if p == "" || strings.EqualFold(lp, strings.TrimRight(d, `\`)) ||
			(strings.Contains(lp, `\wamp64\`) && !fileExists(p)) || strings.HasSuffix(lp, `\appdata\local\cove\bin`) {
			continue
		}
		keep = append(keep, p)
	}
	newPath := strings.Join(keep, ";")
	if newPath == cur {
		return nil
	}
	if err := k.SetExpandStringValue("Path", newPath); err != nil {
		return err
	}
	broadcastEnvChange()
	return nil
}

func broadcastEnvChange() {
	user32 := windows.NewLazySystemDLL("user32.dll")
	send := user32.NewProc("SendMessageTimeoutW")
	env, _ := windows.UTF16PtrFromString("Environment")
	var res uintptr
	send.Call(0xFFFF /* HWND_BROADCAST */, 0x001A /* WM_SETTINGCHANGE */, 0, uintptrOf(env), 0x0002, 3000, uintptrOf(&res))
}

func setAutostart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		k.DeleteValue("Cove")
		return nil
	}
	tray := filepath.Join(binDir(), "CoveTray.exe")
	if !fileExists(tray) {
		exe, _ := os.Executable()
		return k.SetStringValue("Cove", fmt.Sprintf(`"%s" up`, exe))
	}
	return k.SetStringValue("Cove", `"`+tray+`"`)
}

// createShortcuts puts "Cove" (the desktop app window) in the Start menu and on
// the Desktop, with the Cove badge as its icon.
func createShortcuts() error {
	app := filepath.Join(binDir(), "CoveApp.exe")
	if !fileExists(app) {
		return fmt.Errorf("CoveApp.exe not found in %s", binDir())
	}
	ico := filepath.Join(Home(), "cove.ico")
	png := iconPNG(true, 256)
	var b bytes.Buffer
	b.Write([]byte{0, 0, 1, 0, 1, 0, 0, 0, 0, 0, 1, 0, 32, 0}) // ICONDIR + entry (0 = 256px)
	b.Write([]byte{byte(len(png)), byte(len(png) >> 8), byte(len(png) >> 16), byte(len(png) >> 24), 22, 0, 0, 0})
	b.Write(png)
	if err := os.WriteFile(ico, b.Bytes(), 0o644); err != nil {
		return err
	}
	ps := fmt.Sprintf(`$s = New-Object -ComObject WScript.Shell
foreach ($p in @([Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('Desktop'))) {
  $l = $s.CreateShortcut((Join-Path $p 'Cove.lnk'))
  $l.TargetPath = '%[1]s'; $l.WorkingDirectory = '%[2]s'; $l.IconLocation = '%[3]s,0'
  $l.Description = 'Cove — local PHP development'; $l.Save()
}`, app, binDir(), ico)
	out, err := hidden(exec.Command("powershell", "-NoProfile", "-Command", ps)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("creating shortcuts: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue("Cove")
	return err == nil
}

func cmdDoctor() {
	c := getConfig()
	fmt.Println(cBold + "\nDoctor" + cReset)
	check := func(good bool, label, fix string) {
		if good {
			fmt.Println("  " + cGreen + "✓" + cReset + " " + label)
		} else {
			fmt.Println("  " + cRed + "✗" + cReset + " " + label + cDim + "  → " + fix + cReset)
		}
	}
	check(len(installedPHP()) > 0, "PHP installed ("+strings.Join(installedPHP(), ", ")+")", "cove install php")
	check(fileExists(filepath.Join(nginxDir(), "nginx.exe")), "nginx installed "+versionOf(nginxDir()), "cove install nginx")
	check(fileExists(filepath.Join(mysqlDir(), "bin", "mysqld.exe")), "MySQL installed "+versionOf(mysqlDir()), "cove install mysql")
	check(nrptInstalled(c.TLD), "*."+c.TLD+" DNS rule present", "cove dns")
	daemon := running()
	for _, p := range []struct {
		name string
		port int
		svc  string
	}{{"HTTP", c.Ports.HTTP, "nginx"}, {"HTTPS", c.Ports.HTTPS, "nginx"}, {"MySQL", c.Ports.MySQL, "mysql"}, {"SMTP", c.Ports.SMTP, "mail"}} {
		owner := portOwner(p.port)
		free := owner == "" || portFree(p.port) == nil || (daemon &&(strings.EqualFold(owner, "nginx.exe") || strings.EqualFold(owner, "mysqld.exe") || strings.HasPrefix(strings.ToLower(owner), "cove")))
		check(free, fmt.Sprintf("port %d (%s) available", p.port, p.name),
			fmt.Sprintf("in use by %s — stop it (e.g. WAMP) or change ports.%s in %s", owner, strings.ToLower(p.name), configPath()))
	}
	fmt.Println()
}

// portOwner returns the process image listening on a TCP port, or "".
func portOwner(port int) string {
	out, _ := hidden(exec.Command("powershell", "-NoProfile", "-Command",
		fmt.Sprintf(`$c = Get-NetTCPConnection -State Listen -LocalPort %d -ErrorAction SilentlyContinue | Select-Object -First 1; if ($c) { (Get-Process -Id $c.OwningProcess).ProcessName + '.exe' }`, port))).Output()
	return strings.TrimSpace(string(out))
}
