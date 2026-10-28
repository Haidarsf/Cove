package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed ui
var uiFS embed.FS

func listenAPI() (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", getConfig().Ports.API))
}

func serveAPI(ln net.Listener) {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(uiFS, "ui")
	ui := http.FileServerFS(sub)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache") // always pick up a new build after upgrades
		ui.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /notfound", notFoundPage)

	mux.HandleFunc("GET /api/status", apiStatus)
	mux.HandleFunc("POST /api/services/{name}/{action}", apiService)
	mux.HandleFunc("POST /api/reload", func(w http.ResponseWriter, r *http.Request) { reply(w, nil, reconcile()) })
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		shutdown() // stop services before replying so `cove down` means everything is down
		reply(w, map[string]bool{"ok": true}, nil)
		go func() { time.Sleep(100 * time.Millisecond); quit() }()
	})

	mux.HandleFunc("GET /api/sites", func(w http.ResponseWriter, r *http.Request) { reply(w, listSites(), nil) })
	mux.HandleFunc("POST /api/sites/link", apiLink)
	mux.HandleFunc("POST /api/sites/unlink", siteAction(func(s Site, _ map[string]any) error {
		return changeConfig(func(c *Config) { delete(c.Links, s.Name) })
	}))
	mux.HandleFunc("POST /api/sites/secure", siteAction(func(s Site, body map[string]any) error {
		on, _ := body["secure"].(bool)
		if on {
			if err := issueSiteCert(s.Host); err != nil {
				return err
			}
		}
		return changeConfig(func(c *Config) {
			if on {
				c.Secured[s.Name] = true
			} else {
				delete(c.Secured, s.Name)
			}
		})
	}))
	mux.HandleFunc("POST /api/sites/isolate", siteAction(func(s Site, body map[string]any) error {
		v, _ := body["php"].(string)
		return changeConfig(func(c *Config) {
			if v == "" || v == c.DefaultPHP {
				delete(c.Isolated, s.Name)
			} else {
				c.Isolated[s.Name] = v
			}
		})
	}))
	mux.HandleFunc("POST /api/sites/open", siteAction(func(s Site, _ map[string]any) error { openURL(s.URL); return nil }))
	mux.HandleFunc("POST /api/sites/reveal", siteAction(func(s Site, _ map[string]any) error { revealPath(s.Path); return nil }))
	mux.HandleFunc("POST /api/paths", apiPaths)
	mux.HandleFunc("POST /api/open", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ URL, Path string }
		json.NewDecoder(r.Body).Decode(&body)
		if strings.HasPrefix(body.URL, "http://") || strings.HasPrefix(body.URL, "https://") {
			openURL(body.URL)
		} else if body.Path != "" {
			revealPath(body.Path)
		}
		reply(w, map[string]bool{"ok": true}, nil)
	})

	mux.HandleFunc("POST /api/php/default", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Branch string }
		json.NewDecoder(r.Body).Decode(&body)
		reply(w, nil, setDefaultPHP(body.Branch))
	})
	mux.HandleFunc("GET /api/php/available", func(w http.ResponseWriter, r *http.Request) {
		rels, err := availablePHP()
		reply(w, rels, err)
	})
	mux.HandleFunc("POST /api/install", apiInstall)

	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) { reply(w, logNames(), nil) })
	mux.HandleFunc("GET /api/logs/{name}", apiLogTail)
	mux.HandleFunc("GET /api/logs/{name}/stream", apiLogStream)
	mux.HandleFunc("DELETE /api/logs/{name}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := logPath(r.PathValue("name"))
		if ok {
			os.Truncate(p, 0)
		}
		reply(w, map[string]bool{"ok": ok}, nil)
	})

	mux.HandleFunc("GET /api/mail", func(w http.ResponseWriter, r *http.Request) { reply(w, listMail(), nil) })
	mux.HandleFunc("GET /api/mail/stream", apiMailStream)
	mux.HandleFunc("GET /api/mail/{id}", func(w http.ResponseWriter, r *http.Request) {
		m, err := readMail(r.PathValue("id"))
		reply(w, m, err)
	})
	mux.HandleFunc("DELETE /api/mail/{id}", func(w http.ResponseWriter, r *http.Request) {
		deleteMail(r.PathValue("id"))
		reply(w, map[string]bool{"ok": true}, nil)
	})
	mux.HandleFunc("DELETE /api/mail", func(w http.ResponseWriter, r *http.Request) {
		deleteMail("")
		reply(w, map[string]bool{"ok": true}, nil)
	})

	mux.HandleFunc("GET /api/db", func(w http.ResponseWriter, r *http.Request) {
		out, err := mysqlExec("SELECT s.schema_name, COALESCE(ROUND(SUM(t.data_length+t.index_length)/1024/1024,2),0), COUNT(t.table_name) FROM information_schema.schemata s LEFT JOIN information_schema.tables t ON t.table_schema=s.schema_name GROUP BY s.schema_name ORDER BY s.schema_name")
		type db struct {
			Name   string `json:"name"`
			SizeMB string `json:"size_mb"`
			Tables string `json:"tables"`
			System bool   `json:"system"`
		}
		var dbs []db
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Split(strings.TrimRight(line, "\r"), "\t")
			if len(f) == 3 {
				sys := f[0] == "mysql" || f[0] == "sys" || f[0] == "information_schema" || f[0] == "performance_schema"
				dbs = append(dbs, db{f[0], f[1], f[2], sys})
			}
		}
		reply(w, dbs, err)
	})
	mux.HandleFunc("POST /api/db", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name string }
		json.NewDecoder(r.Body).Decode(&body)
		if !regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`).MatchString(body.Name) {
			reply(w, nil, fmt.Errorf("database names may contain letters, digits and _"))
			return
		}
		_, err := mysqlExec("CREATE DATABASE IF NOT EXISTS `" + body.Name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
		reply(w, map[string]bool{"ok": err == nil}, err)
	})
	mux.HandleFunc("DELETE /api/db/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`).MatchString(name) || name == "mysql" || name == "sys" {
			reply(w, nil, fmt.Errorf("refusing to drop %q", name))
			return
		}
		_, err := mysqlExec("DROP DATABASE IF EXISTS `" + name + "`")
		reply(w, map[string]bool{"ok": err == nil}, err)
	})

	http.Serve(ln, guard(mux))
}

// guard blocks DNS-rebinding and cross-site requests: only our own hostnames
// are served, and every mutating call must carry the X-Cove header (which a
// foreign web page cannot send without a CORS preflight we never approve).
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		ok := host == "127.0.0.1" || host == "localhost" || host == "cove."+getConfig().TLD || r.URL.Path == "/notfound"
		if !ok {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != "GET" && r.Header.Get("X-Cove") != "1" {
			http.Error(w, "missing X-Cove header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if v == nil {
		v = map[string]bool{"ok": true}
	}
	json.NewEncoder(w).Encode(v)
}

func changeConfig(fn func(c *Config)) error {
	if err := updateConfig(fn); err != nil {
		return err
	}
	return reconcile()
}

func siteAction(fn func(s Site, body map[string]any) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		json.NewDecoder(r.Body).Decode(&body)
		name, _ := body["name"].(string)
		s, ok := findSite(name)
		if !ok {
			if p, _ := body["path"].(string); p != "" {
				s, ok = siteForDir(p)
			}
		}
		if !ok {
			reply(w, nil, fmt.Errorf("no site named %q", name))
			return
		}
		err := fn(s, body)
		s, _ = findSite(s.Name)
		reply(w, s, err)
	}
}

func apiStatus(w http.ResponseWriter, r *http.Request) {
	c := getConfig()
	type phpInfo struct {
		Branch  string `json:"branch"`
		Version string `json:"version"`
		Default bool   `json:"default"`
	}
	var php []phpInfo
	for _, v := range installedPHP() {
		php = append(php, phpInfo{v, phpVersion(v), v == c.DefaultPHP})
	}
	st := make([]ServiceStatus, 0, len(services))
	for _, s := range services {
		st = append(st, s.Status())
	}
	unread := 0
	for _, m := range listMail() {
		if !m.Read {
			unread++
		}
	}
	reply(w, map[string]any{
		"version":     appVersion,
		"home":        Home(),
		"services":    st,
		"php":         php,
		"default_php": c.DefaultPHP,
		"tld":         c.TLD,
		"ports":       c.Ports,
		"paths":       c.Paths,
		"sites":       len(listSites()),
		"mail_unread": unread,
		"composer":    fileExists(filepath.Join(binDir(), "composer.phar")),
		"bin":         binDir(),
	}, nil)
}

func apiService(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	var err error
	switch action {
	case "start":
		err = startService(name)
	case "stop":
		err = stopService(name)
	case "restart":
		err = restartService(name)
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	time.Sleep(300 * time.Millisecond)
	reply(w, nil, err)
}

func apiLink(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name, Path string }
	json.NewDecoder(r.Body).Decode(&body)
	fi, err := os.Stat(body.Path)
	if err != nil || !fi.IsDir() || !filepath.IsAbs(body.Path) {
		reply(w, nil, fmt.Errorf("folder not found: %s", body.Path))
		return
	}
	abs, _ := filepath.Abs(body.Path)
	name := slug(body.Name)
	if name == "" {
		name = slug(filepath.Base(abs))
	}
	if err := changeConfig(func(c *Config) { c.Links[name] = abs }); err != nil {
		reply(w, nil, err)
		return
	}
	s, _ := findSite(name)
	reply(w, s, nil)
}

func apiPaths(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path   string
		Remove bool
	}
	json.NewDecoder(r.Body).Decode(&body)
	// filepath.Abs("") is the daemon's working directory — never what the user meant.
	if strings.TrimSpace(body.Path) == "" || !filepath.IsAbs(body.Path) {
		reply(w, nil, fmt.Errorf("enter a full folder path, like C:\\Users\\you\\Sites"))
		return
	}
	abs, _ := filepath.Abs(body.Path)
	if !body.Remove {
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			reply(w, nil, fmt.Errorf("folder not found: %s", body.Path))
			return
		}
	}
	reply(w, nil, changeConfig(func(c *Config) {
		var out []string
		for _, p := range c.Paths {
			if !strings.EqualFold(p, abs) {
				out = append(out, p)
			}
		}
		if !body.Remove {
			out = append(out, abs)
		}
		c.Paths = out
	}))
}

func setDefaultPHP(branch string) error {
	if !fileExists(filepath.Join(phpDir(branch), "php-cgi.exe")) {
		return fmt.Errorf("PHP %s is not installed", branch)
	}
	return changeConfig(func(c *Config) { c.DefaultPHP = branch })
}

var installMu sync.Mutex

// apiInstall streams installer output so the dashboard can show live progress.
func apiInstall(w http.ResponseWriter, r *http.Request) {
	var body struct{ What, Branch string }
	json.NewDecoder(r.Body).Decode(&body)
	if !installMu.TryLock() {
		http.Error(w, "another install is running", 409)
		return
	}
	defer installMu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fw := &flushWriter{w: w}
	if err := runInstall(body.What, body.Branch, fw); err != nil {
		fmt.Fprintf(fw, "✗ %v\n", err)
		return
	}
	if err := reconcile(); err != nil {
		fmt.Fprintf(fw, "! %v\n", err)
	}
	fmt.Fprintln(fw, "done")
}

// runInstall stops the affected service while its files are replaced (Windows
// locks running executables) and brings it back afterwards.
func runInstall(what, branch string, w io.Writer) error {
	var resume []string
	pause := func(name string) {
		if s := serviceMap[name]; s != nil && s.Status().State == "running" {
			fmt.Fprintf(w, "  stopping %s during upgrade\n", name)
			stopService(name)
			resume = append(resume, name)
		}
	}
	defer func() {
		for _, name := range resume {
			startService(name)
		}
	}()
	switch what {
	case "php":
		if containsStr(branchesInUse(), branch) {
			pause("php")
		}
		return installPHP(branch, w)
	case "nginx":
		pause("nginx")
		return installNginx(w)
	case "mysql":
		pause("mysql")
		return installMySQL(w)
	case "composer":
		return installComposer(w)
	}
	return fmt.Errorf("unknown component %q", what)
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// ---------- logs ----------

func logNames() []string {
	files, _ := filepath.Glob(filepath.Join(logDir(), "*.log"))
	var out []string
	for _, f := range files {
		out = append(out, strings.TrimSuffix(filepath.Base(f), ".log"))
	}
	sort.Strings(out)
	return out
}

func logPath(name string) (string, bool) {
	if !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(name) || strings.Contains(name, "..") {
		return "", false
	}
	p := filepath.Join(logDir(), name+".log")
	return p, fileExists(p)
}

// tailFile returns roughly the last n lines without reading huge files fully.
func tailFile(p string, n int) (string, int64) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	fi, _ := f.Stat()
	size := fi.Size()
	start := size - 512*1024
	if start < 0 {
		start = 0
	}
	f.Seek(start, io.SeekStart)
	b, _ := io.ReadAll(f)
	lines := strings.Split(string(b), "\n")
	if len(lines) > n+1 {
		lines = lines[len(lines)-n-1:]
	}
	return strings.Join(lines, "\n"), size
}

func apiLogTail(w http.ResponseWriter, r *http.Request) {
	p, ok := logPath(r.PathValue("name"))
	if !ok {
		reply(w, map[string]any{"text": "", "size": 0}, nil)
		return
	}
	text, size := tailFile(p, 400)
	reply(w, map[string]any{"text": text, "size": size}, nil)
}

func apiLogStream(w http.ResponseWriter, r *http.Request) {
	p, ok := logPath(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	var off int64
	if fi, err := os.Stat(p); err == nil {
		off = fi.Size()
	}
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			if fi.Size() < off {
				off = 0
			}
			if fi.Size() == off {
				continue
			}
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			f.Seek(off, io.SeekStart)
			b, _ := io.ReadAll(io.LimitReader(f, 256*1024))
			f.Close()
			off += int64(len(b))
			j, _ := json.Marshal(string(b))
			fmt.Fprintf(w, "data: %s\n\n", j)
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

func apiMailStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	ch := make(chan MailMeta, 8)
	mailMu.Lock()
	mailSubs[ch] = true
	mailMu.Unlock()
	defer func() { mailMu.Lock(); delete(mailSubs, ch); mailMu.Unlock() }()
	fmt.Fprint(w, ": hello\n\n")
	if fl != nil {
		fl.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-ch:
			j, _ := json.Marshal(m)
			fmt.Fprintf(w, "data: %s\n\n", j)
			if fl != nil {
				fl.Flush()
			}
		case <-time.After(25 * time.Second):
			fmt.Fprint(w, ": ping\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

func notFoundPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(404)
	host := strings.Split(r.Host, ":")[0]
	fmt.Fprintf(w, `<!doctype html><meta charset=utf-8><title>No site</title><style>body{font:16px/1.6 "Segoe UI Variable Text","Segoe UI",system-ui;background:#eef3f2;color:#0f2b2a;display:grid;place-items:center;height:100vh;margin:0}div{max-width:520px;padding:36px 40px;background:#fff;border-radius:20px;box-shadow:0 20px 50px -25px rgba(15,60,58,.35)}h1{font-size:22px;margin:0 0 8px}b{color:#0d9488}a{color:#0d9488;font-weight:600}code{background:#e6f2f0;padding:2px 8px;border-radius:6px}</style><div><h1>Nothing is moored at <b>%s</b></h1><p>Add a folder to a parked directory, or run <code>cove link</code> inside your project.</p><p><a href="http://cove.%s">Open Cove →</a></p></div>`,
		htmlEscape(host), getConfig().TLD)
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
