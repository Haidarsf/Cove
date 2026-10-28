package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Ports used by every Cove service. Change them in config.json when another
// stack (WAMP, XAMPP, IIS) is already holding the defaults.
type Ports struct {
	HTTP  int `json:"http"`
	HTTPS int `json:"https"`
	MySQL int `json:"mysql"`
	SMTP  int `json:"smtp"`
	DNS   int `json:"dns"`
	API   int `json:"api"`
}

type Config struct {
	TLD        string            `json:"tld"`
	DefaultPHP string            `json:"default_php"`
	Paths      []string          `json:"paths"`    // parked directories: every sub-folder becomes a site
	Links      map[string]string `json:"links"`    // site name -> folder
	Secured    map[string]bool   `json:"secured"`  // site name -> served over HTTPS
	Isolated   map[string]string `json:"isolated"` // site name -> PHP branch
	Ports      Ports             `json:"ports"`
	PHPWorkers int               `json:"php_workers"`
	Autostart  []string          `json:"autostart"`
}

var (
	cfgMu sync.RWMutex
	cfg   *Config
)

// Home is where Cove keeps binaries, data and generated config.
func Home() string {
	if h := os.Getenv("COVE_HOME"); h != "" {
		return h
	}
	// %USERPROFILE%\.cove rather than AppData: packaged (MSIX) apps get AppData
	// redirected into a private container, which would hide Cove from everything else.
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cove")
}

func dir(parts ...string) string {
	p := filepath.Join(append([]string{Home()}, parts...)...)
	os.MkdirAll(p, 0o755)
	return p
}

func binDir() string              { return dir("bin") }
func phpDir(branch string) string { return filepath.Join(Home(), "php", branch) }
func nginxDir() string            { return filepath.Join(Home(), "nginx") }
func mysqlDir() string            { return filepath.Join(Home(), "mysql") }
func nodeDir() string             { return filepath.Join(Home(), "node") }
func logDir() string              { return dir("logs") }
func confDir() string             { return dir("config") }
func certDir() string             { return dir("certs") }
func dataDir(sub string) string   { return dir("data", sub) }

func configPath() string { return filepath.Join(Home(), "config.json") }

func defaultConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		TLD:        "test",
		DefaultPHP: "",
		Paths:      []string{filepath.Join(home, "Sites")},
		Links:      map[string]string{},
		Secured:    map[string]bool{},
		Isolated:   map[string]string{},
		Ports:      Ports{HTTP: 80, HTTPS: 443, MySQL: 3306, SMTP: 2525, DNS: 53, API: 7771},
		PHPWorkers: 4,
		Autostart:  []string{"dns", "php", "nginx", "mysql", "mail"},
	}
}

func loadConfig() *Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	c := defaultConfig()
	if b, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(b, c)
	}
	if c.Links == nil {
		c.Links = map[string]string{}
	}
	if c.Secured == nil {
		c.Secured = map[string]bool{}
	}
	if c.Isolated == nil {
		c.Isolated = map[string]string{}
	}
	if c.PHPWorkers < 1 {
		c.PHPWorkers = 4
	}
	for _, p := range c.Paths {
		os.MkdirAll(p, 0o755)
	}
	if c.DefaultPHP == "" {
		if v := installedPHP(); len(v) > 0 {
			c.DefaultPHP = v[len(v)-1]
		}
	}
	cfg = c
	return c
}

func getConfig() Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	if cfg == nil {
		cfgMu.RUnlock()
		loadConfig()
		cfgMu.RLock()
	}
	return *cfg
}

// updateConfig mutates and persists the config atomically.
func updateConfig(fn func(c *Config)) error {
	getConfig()
	cfgMu.Lock()
	defer cfgMu.Unlock()
	fn(cfg)
	b, _ := json.MarshalIndent(cfg, "", "  ")
	os.MkdirAll(Home(), 0o755)
	tmp := configPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, configPath())
}

func saveConfig() error { return updateConfig(func(*Config) {}) }

// installedPHP returns installed PHP branches sorted ascending ("8.3", "8.4", ...).
func installedPHP() []string {
	entries, _ := os.ReadDir(filepath.Join(Home(), "php"))
	var out []string
	for _, e := range entries {
		if e.IsDir() && fileExists(filepath.Join(Home(), "php", e.Name(), "php-cgi.exe")) {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[i], out[j]) })
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// versionLess compares dotted numeric versions ("8.10" > "8.9").
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = atoi(pa[i])
		}
		if i < len(pb) {
			y = atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// fwd converts a Windows path to the forward-slash form nginx and php.ini prefer.
func fwd(p string) string { return strings.ReplaceAll(p, `\`, "/") }
