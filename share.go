package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Sharing runs a Cloudflare quick tunnel (no account) per site. nginx learns
// the public hostname, so the app sees its real address and builds links that
// work outside this machine.

type share struct {
	proc *Proc
	url  string
}

var (
	shareMu sync.Mutex
	shares  = map[string]*share{}
	tunnel  = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)
)

func cloudflaredPath() string { return filepath.Join(binDir(), "cloudflared.exe") }

func installCloudflared(w io.Writer) error {
	v, url, sha, err := githubAsset("cloudflare/cloudflared", func(n string) bool { return n == "cloudflared-windows-amd64.exe" })
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "→ cloudflared %s (for sharing)\n", v)
	p, err := download(url, sha, w)
	if err != nil {
		return err
	}
	if err := moveFile(p, cloudflaredPath()); err != nil {
		return err
	}
	fmt.Fprintf(w, "✓ cloudflared %s installed\n", v)
	return nil
}

// shareURL returns the public URL of a shared site, or "".
func shareURL(name string) string {
	shareMu.Lock()
	defer shareMu.Unlock()
	if s := shares[name]; s != nil {
		return s.url
	}
	return ""
}

func shareHost(name string) string {
	return strings.TrimPrefix(shareURL(name), "https://")
}

func startShare(s Site) error {
	shareMu.Lock()
	if shares[s.Name] != nil {
		shareMu.Unlock()
		return nil
	}
	shareMu.Unlock()
	if !fileExists(cloudflaredPath()) {
		return fmt.Errorf("cloudflared isn't installed yet — run `cove install cloudflared`")
	}
	c := getConfig()
	// HTTPS sites go to 443 (SNI picks the site's cert); plain ones to 80, where
	// an HTTPS site's block would only redirect.
	args := []string{"tunnel", "--no-autoupdate", "--url", fmt.Sprintf("http://127.0.0.1:%d", c.Ports.HTTP)}
	if s.Secure {
		args = []string{"tunnel", "--no-autoupdate", "--url", fmt.Sprintf("https://127.0.0.1:%d", c.Ports.HTTPS),
			"--no-tls-verify", "--origin-server-name", s.Host}
	}
	logPath := filepath.Join(logDir(), "share-"+s.Name+".log")
	os.WriteFile(logPath, nil, 0o644)
	sh := &share{proc: &Proc{
		Name:  "share-" + s.Name,
		Log:   logPath,
		Build: func() *exec.Cmd { return exec.Command(cloudflaredPath(), args...) },
	}}
	shareMu.Lock()
	shares[s.Name] = sh
	shareMu.Unlock()
	sh.proc.Start()
	// cloudflared prints the hostname to its log once the tunnel is up.
	for i := 0; i < 60; i++ {
		time.Sleep(500 * time.Millisecond)
		b, _ := os.ReadFile(logPath)
		if u := tunnel.Find(b); u != nil {
			shareMu.Lock()
			sh.url = string(u)
			shareMu.Unlock()
			log.Printf("sharing %s at %s", s.Host, u)
			return reconcile()
		}
		if st, _ := sh.proc.State(); st == "error" {
			break
		}
	}
	stopShare(s.Name)
	return fmt.Errorf("the tunnel didn't come up — see logs/share-%s.log", s.Name)
}

func stopShare(name string) error {
	shareMu.Lock()
	sh := shares[name]
	delete(shares, name)
	shareMu.Unlock()
	if sh == nil {
		return nil
	}
	sh.proc.StopAndWait()
	log.Printf("stopped sharing %s", name)
	return reconcile()
}

func stopAllShares() {
	shareMu.Lock()
	var names []string
	for n := range shares {
		names = append(names, n)
	}
	shareMu.Unlock()
	for _, n := range names {
		stopShare(n)
	}
}

// cmdShare shares a site (by name, or the one for the current folder) and
// prints its public URL; `cove unshare` stops it.
func cmdShare(args []string, on bool) error {
	body := map[string]any{"share": on}
	if len(args) > 0 {
		body["name"] = args[0]
	} else {
		cwd, _ := os.Getwd()
		body["path"] = cwd
	}
	if on {
		if !fileExists(cloudflaredPath()) {
			if err := cmdInstall([]string{"cloudflared"}); err != nil {
				return err
			}
		}
		info("Opening a tunnel…")
	}
	b, err := call("POST", "/api/sites/share", body)
	if err != nil {
		return err
	}
	if !on {
		ok("Sharing stopped")
		return nil
	}
	if u := tunnel.Find(b); u != nil {
		ok("Shared at " + cBold + string(u) + cReset + "  — anyone with the link can open it")
		fmt.Println(cDim + "  Stop with `cove unshare`. Laravel: add trustProxies(at: '*') so it builds https links." + cReset)
	}
	return nil
}
