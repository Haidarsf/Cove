package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Minute}

type PHPRelease struct {
	Branch  string `json:"branch"`
	Version string `json:"version"`
	Zip     string `json:"zip"`
	SHA256  string `json:"sha256"`
	Size    string `json:"size"`
}

// availablePHP reads the official windows.php.net release feed (NTS x64 builds,
// which is what php-cgi + nginx needs).
func availablePHP() ([]PHPRelease, error) {
	resp, err := httpClient.Get("https://windows.php.net/downloads/releases/releases.json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var feed map[string]map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, err
	}
	var out []PHPRelease
	for branch, entry := range feed {
		if versionLess(branch, "8.1") {
			continue
		}
		rel := PHPRelease{Branch: branch}
		json.Unmarshal(entry["version"], &rel.Version)
		for key, raw := range entry {
			if strings.HasPrefix(key, "nts-") && strings.HasSuffix(key, "-x64") {
				var b struct {
					Zip struct{ Path, Size, Sha256 string } `json:"zip"`
				}
				json.Unmarshal(raw, &b)
				rel.Zip, rel.SHA256, rel.Size = b.Zip.Path, b.Zip.Sha256, b.Zip.Size
			}
		}
		if rel.Zip != "" {
			out = append(out, rel)
		}
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[j].Branch, out[i].Branch) })
	return out, nil
}

func installPHP(branch string, w io.Writer) error {
	rels, err := availablePHP()
	if err != nil {
		return fmt.Errorf("reading PHP release feed: %w", err)
	}
	if branch == "" || branch == "latest" {
		branch = rels[0].Branch
	}
	var rel *PHPRelease
	for i := range rels {
		if rels[i].Branch == branch {
			rel = &rels[i]
		}
	}
	if rel == nil {
		return fmt.Errorf("PHP %s is not available on windows.php.net", branch)
	}
	fmt.Fprintf(w, "→ PHP %s (%s)\n", strings.TrimSuffix(rel.Version, "-1"), rel.Size)
	zipPath, err := download("https://windows.php.net/downloads/releases/"+rel.Zip, rel.SHA256, w)
	if err != nil {
		return err
	}
	defer os.Remove(zipPath)
	target := phpDir(branch)
	os.RemoveAll(target)
	if err := unzip(zipPath, target, false); err != nil {
		return err
	}
	os.WriteFile(filepath.Join(target, "VERSION"), []byte(rel.Version), 0o644)
	if err := writePHPIni(branch); err != nil {
		return err
	}
	updateConfig(func(c *Config) {
		if c.DefaultPHP == "" {
			c.DefaultPHP = branch
		}
	})
	writeShims()
	fmt.Fprintf(w, "✓ PHP %s installed\n", phpVersion(branch))
	return nil
}

func phpVersion(branch string) string {
	b, err := os.ReadFile(filepath.Join(phpDir(branch), "VERSION"))
	if err != nil {
		return branch
	}
	return strings.TrimSuffix(strings.TrimSpace(string(b)), "-1")
}

func latestNginx() (string, error) {
	body, err := getString("https://nginx.org/en/download.html")
	if err != nil {
		return "", err
	}
	m := regexp.MustCompile(`nginx-(\d+\.\d+\.\d+)\.zip`).FindAllStringSubmatch(body, -1)
	if len(m) == 0 {
		return "", fmt.Errorf("no nginx release found")
	}
	best := m[0][1]
	for _, x := range m {
		if versionLess(best, x[1]) {
			best = x[1]
		}
	}
	return best, nil
}

func installNginx(w io.Writer) error {
	v, err := latestNginx()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "→ nginx %s\n", v)
	zipPath, err := download(fmt.Sprintf("https://nginx.org/download/nginx-%s.zip", v), "", w)
	if err != nil {
		return err
	}
	defer os.Remove(zipPath)
	os.RemoveAll(nginxDir())
	if err := unzip(zipPath, nginxDir(), true); err != nil {
		return err
	}
	os.WriteFile(filepath.Join(nginxDir(), "VERSION"), []byte(v), 0o644)
	fmt.Fprintf(w, "✓ nginx %s installed\n", v)
	return nil
}

// latestMySQL walks the official mysql-server tags newest-first and returns the
// first one that has a Windows zip on Oracle's CDN.
func latestMySQL() (string, string, error) {
	var tags []string
	for page := 1; page <= 3; page++ {
		var res []struct{ Name string }
		body, err := getString(fmt.Sprintf("https://api.github.com/repos/mysql/mysql-server/tags?per_page=100&page=%d", page))
		if err != nil {
			return "", "", err
		}
		json.Unmarshal([]byte(body), &res)
		for _, t := range res {
			if m := regexp.MustCompile(`^mysql-(\d+\.\d+\.\d+)$`).FindStringSubmatch(t.Name); m != nil {
				tags = append(tags, m[1])
			}
		}
	}
	sort.Slice(tags, func(i, j int) bool { return versionLess(tags[j], tags[i]) })
	for _, v := range tags {
		parts := strings.Split(v, ".")
		url := fmt.Sprintf("https://cdn.mysql.com/Downloads/MySQL-%s.%s/mysql-%s-winx64.zip", parts[0], parts[1], v)
		if resp, err := httpClient.Head(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return v, url, nil
			}
		}
	}
	return "", "", fmt.Errorf("could not find a MySQL Windows build")
}

func installMySQL(w io.Writer) error {
	v, url, err := latestMySQL()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "→ MySQL %s (≈380 MB, this takes a moment)\n", v)
	zipPath, err := download(url, "", w)
	if err != nil {
		return err
	}
	defer os.Remove(zipPath)
	os.RemoveAll(mysqlDir())
	if err := unzip(zipPath, mysqlDir(), true); err != nil {
		return err
	}
	os.WriteFile(filepath.Join(mysqlDir(), "VERSION"), []byte(v), 0o644)
	writeShims()
	fmt.Fprintf(w, "✓ MySQL %s installed (user root, no password, port %d)\n", v, getConfig().Ports.MySQL)
	return nil
}

func installComposer(w io.Writer) error {
	fmt.Fprintln(w, "→ Composer (latest stable)")
	p, err := download("https://getcomposer.org/download/latest-stable/composer.phar", "", w)
	if err != nil {
		return err
	}
	if err := moveFile(p, filepath.Join(binDir(), "composer.phar")); err != nil {
		return err
	}
	writeShims()
	fmt.Fprintln(w, "✓ Composer installed")
	return nil
}

// latestNode picks from the official nodejs.org feed (newest first): the newest
// LTS by default, or the newest release of a major line ("22", "v22").
func latestNode(major string) (string, error) {
	body, err := getString("https://nodejs.org/dist/index.json")
	if err != nil {
		return "", err
	}
	var rels []struct {
		Version string
		Files   []string
		LTS     any
	}
	if err := json.Unmarshal([]byte(body), &rels); err != nil {
		return "", err
	}
	major = strings.TrimPrefix(major, "v")
	for _, r := range rels {
		if !containsStr(r.Files, "win-x64-zip") {
			continue
		}
		if major == "" || major == "latest" {
			if lts, _ := r.LTS.(string); lts != "" {
				return r.Version, nil
			}
		} else if strings.HasPrefix(r.Version, "v"+major+".") {
			return r.Version, nil
		}
	}
	return "", fmt.Errorf("no Node.js %s release for Windows found", major)
}

func installNode(major string, w io.Writer) error {
	v, err := latestNode(major)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("node-%s-win-x64.zip", v)
	sums, err := getString(fmt.Sprintf("https://nodejs.org/dist/%s/SHASUMS256.txt", v))
	if err != nil {
		return err
	}
	sha := ""
	for _, line := range strings.Split(sums, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == name {
			sha = f[0]
		}
	}
	if sha == "" {
		return fmt.Errorf("no checksum published for %s", name)
	}
	fmt.Fprintf(w, "→ Node.js %s\n", v)
	zipPath, err := download(fmt.Sprintf("https://nodejs.org/dist/%s/%s", v, name), sha, w)
	if err != nil {
		return err
	}
	defer os.Remove(zipPath)
	os.RemoveAll(nodeDir())
	if err := unzip(zipPath, nodeDir(), true); err != nil {
		return err
	}
	os.WriteFile(filepath.Join(nodeDir(), "VERSION"), []byte(strings.TrimPrefix(v, "v")), 0o644)
	writeShims()
	// npm's default global prefix, so `npm i -g` tools (pnpm, vite…) run.
	appendUserPath(filepath.Join(os.Getenv("APPDATA"), "npm"))
	fmt.Fprintf(w, "✓ Node.js %s installed (node, npm, npx)\n", v)
	return nil
}

// githubAsset finds the newest release asset of repo whose name matches, with
// the SHA-256 GitHub publishes for it.
func githubAsset(repo string, match func(name string) bool) (tag, url, sha string, err error) {
	body, err := getString("https://api.github.com/repos/" + repo + "/releases/latest")
	if err != nil {
		return "", "", "", err
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name   string
			URL    string `json:"browser_download_url"`
			Digest string
		}
	}
	if err := json.Unmarshal([]byte(body), &rel); err != nil {
		return "", "", "", err
	}
	for _, a := range rel.Assets {
		if match(a.Name) {
			return rel.TagName, a.URL, strings.TrimPrefix(a.Digest, "sha256:"), nil
		}
	}
	return "", "", "", fmt.Errorf("no matching download in %s %s", repo, rel.TagName)
}

// installRedis uses the redis-windows builds of upstream Redis (msys2 runtime).
func installRedis(w io.Writer) error {
	v, url, sha, err := githubAsset("redis-windows/redis-windows", func(n string) bool {
		return strings.HasSuffix(n, "-Windows-x64-msys2.zip")
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "→ Redis %s\n", v)
	zipPath, err := download(url, sha, w)
	if err != nil {
		return err
	}
	defer os.Remove(zipPath)
	os.RemoveAll(redisDir())
	if err := unzip(zipPath, redisDir(), zipHasTopDir(zipPath)); err != nil {
		return err
	}
	os.WriteFile(filepath.Join(redisDir(), "VERSION"), []byte(v), 0o644)
	addAutostart("redis")
	writeShims()
	fmt.Fprintf(w, "✓ Redis %s installed (127.0.0.1:%d)\n", v, getConfig().Ports.Redis)
	return nil
}

// zipHasTopDir reports whether every entry sits under one top-level folder.
func zipHasTopDir(src string) bool {
	r, err := zip.OpenReader(src)
	if err != nil {
		return false
	}
	defer r.Close()
	top := ""
	for _, f := range r.File {
		i := strings.IndexAny(f.Name, "/\\")
		if i < 0 {
			return false
		}
		if top == "" {
			top = f.Name[:i]
		} else if f.Name[:i] != top {
			return false
		}
	}
	return top != ""
}

// addAutostart makes a newly installed service start with Cove.
func addAutostart(name string) {
	updateConfig(func(c *Config) {
		if !containsStr(c.Autostart, name) {
			c.Autostart = append(c.Autostart, name)
		}
	})
}

// installCABundle fetches Mozilla's CA bundle (via curl.se) and appends the Cove
// root so PHP's curl/openssl trust both the internet and local *.test sites.
func installCABundle(w io.Writer) error {
	p, err := download("https://curl.se/ca/cacert.pem", "", io.Discard)
	if err != nil {
		return err
	}
	return moveFile(p, filepath.Join(certDir(), "cacert-mozilla.pem"))
}

func rebuildCABundle() {
	mozilla, _ := os.ReadFile(filepath.Join(certDir(), "cacert-mozilla.pem"))
	ca, _ := os.ReadFile(filepath.Join(certDir(), "CoveCA.crt"))
	os.WriteFile(filepath.Join(certDir(), "cacert.pem"), append(append(mozilla, '\n'), ca...), 0o644)
}

func versionOf(path string) string {
	b, err := os.ReadFile(filepath.Join(path, "VERSION"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ---------- helpers ----------

func getString(url string) (string, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Cove/"+appVersion)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

type progress struct {
	w           io.Writer
	total, done int64
	last        time.Time
}

func (p *progress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if time.Since(p.last) > 700*time.Millisecond {
		p.last = time.Now()
		if p.total > 0 {
			fmt.Fprintf(p.w, "  %3d%%  %.1f / %.1f MB\n", p.done*100/p.total, mb(p.done), mb(p.total))
		} else {
			fmt.Fprintf(p.w, "  %.1f MB\n", mb(p.done))
		}
	}
	return len(b), nil
}

func mb(n int64) float64 { return float64(n) / 1024 / 1024 }

func download(url, sha string, w io.Writer) (string, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Cove/"+appVersion)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.CreateTemp(dir("tmp"), "dl-*")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h, &progress{w: w, total: resp.ContentLength}), resp.Body)
	f.Close()
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if sha != "" && !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), sha) {
		os.Remove(f.Name())
		return "", fmt.Errorf("checksum mismatch for %s", url)
	}
	return f.Name(), nil
}

// unzip extracts src into dst. stripTop drops the archive's single top folder
// (nginx-1.x/, mysql-x-winx64/).
func unzip(src, dst string, stripTop bool) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	root, _ := filepath.Abs(dst)
	for _, f := range r.File {
		name := f.Name
		if stripTop {
			if i := strings.IndexAny(name, "/\\"); i >= 0 {
				name = name[i+1:]
			} else {
				continue
			}
		}
		if name == "" {
			continue
		}
		p := filepath.Join(root, name)
		if !strings.HasPrefix(p, root+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path in archive: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(p, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(p)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func moveFile(src, dst string) error {
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.Remove(dst)
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	os.Remove(src)
	return os.WriteFile(dst, b, 0o644)
}

// writeShims puts php/composer/mysql/laravel/node wrappers in %LOCALAPPDATA%\Cove\bin,
// which `cove setup` adds to the user PATH. php points at the default version.
func writeShims() {
	c := getConfig()
	bd := binDir()
	shim := func(name, body string) {
		os.WriteFile(filepath.Join(bd, name+".cmd"), []byte("@echo off\r\n"+body+"\r\n"), 0o644)
	}
	if c.DefaultPHP != "" {
		php := filepath.Join(phpDir(c.DefaultPHP), "php.exe")
		shim("php", fmt.Sprintf(`"%s" -c "%s" %%*`, php, filepath.Join(phpDir(c.DefaultPHP), "php.ini")))
		shim("composer", fmt.Sprintf(`"%s" -c "%s" "%s" %%*`, php, filepath.Join(phpDir(c.DefaultPHP), "php.ini"), filepath.Join(bd, "composer.phar")))
		shim("laravel", fmt.Sprintf(`call "%s" global exec laravel -- %%*`, filepath.Join(bd, "composer.cmd")))
	}
	for _, tool := range []string{"mysql", "mysqldump", "mysqladmin"} {
		shim(tool, fmt.Sprintf(`"%s" -P %d -h 127.0.0.1 %%*`, filepath.Join(mysqlDir(), "bin", tool+".exe"), c.Ports.MySQL))
	}
	if fileExists(filepath.Join(redisDir(), "redis-cli.exe")) {
		shim("redis-cli", fmt.Sprintf(`"%s" -p %d %%*`, filepath.Join(redisDir(), "redis-cli.exe"), c.Ports.Redis))
	}
	// npm and npx are .cmd files themselves, so they need call to return here.
	if fileExists(filepath.Join(nodeDir(), "node.exe")) {
		shim("node", fmt.Sprintf(`"%s" %%*`, filepath.Join(nodeDir(), "node.exe")))
		shim("npm", fmt.Sprintf(`call "%s" %%*`, filepath.Join(nodeDir(), "npm.cmd")))
		shim("npx", fmt.Sprintf(`call "%s" %%*`, filepath.Join(nodeDir(), "npx.cmd")))
	}
}
