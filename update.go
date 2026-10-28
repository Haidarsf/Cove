package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// updateRepo is the GitHub repository releases come from; set at build time
// with -X main.updateRepo=owner/name. It is private, so requests carry the
// token of the signed-in GitHub CLI (`gh auth login`).
var updateRepo = "Haidarsf/Cove"

const updateAsset = "cove-windows-x64.zip"

type Release struct {
	Version string `json:"version"`
	Notes   string `json:"notes"`
	URL     string `json:"url"`
	Newer   bool   `json:"newer"`
	asset   string // API URL of the zip
}

func ghToken() (string, error) {
	out, err := hidden(exec.Command("gh", "auth", "token")).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return "", fmt.Errorf("sign in to GitHub first: gh auth login (Cove's releases are private)")
	}
	return strings.TrimSpace(string(out)), nil
}

func ghRequest(url, accept string) (*http.Response, error) {
	tok, err := ghToken()
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "Cove/"+appVersion)
	// The asset redirect goes to a signed storage URL; Go drops the
	// Authorization header on that cross-host hop, as it must.
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		if resp.StatusCode == 404 {
			return nil, fmt.Errorf("no release found in %s (or this GitHub account can't see it)", updateRepo)
		}
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return resp, nil
}

func checkUpdate() (Release, error) {
	var r Release
	resp, err := ghRequest("https://api.github.com/repos/"+updateRepo+"/releases/latest", "application/vnd.github+json")
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	var rel struct {
		TagName string `json:"tag_name"`
		Body    string
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name string
			URL  string
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return r, err
	}
	r.Version, r.Notes, r.URL = strings.TrimPrefix(rel.TagName, "v"), rel.Body, rel.HTMLURL
	r.Newer = versionLess(appVersion, r.Version)
	for _, a := range rel.Assets {
		if a.Name == updateAsset {
			r.asset = a.URL
		}
	}
	if r.Newer && r.asset == "" {
		return r, fmt.Errorf("release %s has no %s", rel.TagName, updateAsset)
	}
	return r, nil
}

// applyUpdate downloads the release and swaps the exes in binDir. Running
// exes can't be overwritten but can be renamed, so the old ones become *.old
// and are deleted on the next start.
func applyUpdate(r Release, w io.Writer) error {
	fmt.Fprintf(w, "→ Downloading Cove %s\n", r.Version)
	resp, err := ghRequest(r.asset, "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(dir("tmp"), "update-*.zip")
	if err != nil {
		return err
	}
	_, err = io.Copy(io.MultiWriter(f, &progress{w: w, total: resp.ContentLength}), resp.Body)
	f.Close()
	defer os.Remove(f.Name())
	if err != nil {
		return err
	}
	stage := filepath.Join(dir("tmp"), "update")
	os.RemoveAll(stage)
	defer os.RemoveAll(stage)
	if err := unzip(f.Name(), stage, false); err != nil {
		return err
	}
	names := []string{"cove.exe", "CoveTray.exe", "CoveApp.exe"}
	for _, n := range names {
		if !fileExists(filepath.Join(stage, n)) {
			return fmt.Errorf("the update is missing %s", n)
		}
	}
	bd := binDir()
	for _, n := range names {
		dst := filepath.Join(bd, n)
		os.Remove(dst + ".old")
		if fileExists(dst) {
			if err := os.Rename(dst, dst+".old"); err != nil {
				return fmt.Errorf("replacing %s: %w", n, err)
			}
		}
		if err := moveFile(filepath.Join(stage, n), dst); err != nil {
			os.Rename(dst+".old", dst) // put the old one back
			return err
		}
	}
	fmt.Fprintf(w, "✓ Cove %s installed\n", r.Version)
	return nil
}

// cleanOldExes removes the exes an update renamed aside.
func cleanOldExes() {
	old, _ := filepath.Glob(filepath.Join(binDir(), "*.exe.old"))
	for _, f := range old {
		os.Remove(f)
	}
}

// restartAfter launches the (new) cove.exe to start Cove again once process
// pid has exited, so the new daemon can take the API port.
func restartAfter(pid int) error {
	cmd := exec.Command(filepath.Join(binDir(), "cove.exe"), "__up-after", fmt.Sprint(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008 | 0x00000200 /* DETACHED | NEW_PROCESS_GROUP */}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func upAfter(pid int) error {
	if p, err := os.FindProcess(pid); err == nil {
		done := make(chan struct{})
		go func() { p.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
		}
	}
	for i := 0; i < 20 && running(); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	return cmdUp()
}

func cmdUpdate(args []string) error {
	r, err := checkUpdate()
	if err != nil {
		return err
	}
	if !r.Newer {
		ok("Cove " + appVersion + " is up to date")
		return nil
	}
	info(fmt.Sprintf("Cove %s is available (you have %s)", r.Version, appVersion))
	if len(args) > 0 && args[0] == "--check" {
		return nil
	}
	wasRunning := running()
	if err := applyUpdate(r, os.Stdout); err != nil {
		return err
	}
	if wasRunning {
		call("POST", "/api/quit", nil)
		for i := 0; i < 40 && running(); i++ {
			time.Sleep(150 * time.Millisecond)
		}
		exe := filepath.Join(binDir(), "cove.exe")
		cmd := exec.Command(exe, "up")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	return nil
}

// apiUpdate streams the download, then hands over to the new version: a
// helper starts it once this daemon has exited.
func apiUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fw := &flushWriter{w: w}
	rel, err := checkUpdate()
	if err == nil && !rel.Newer {
		err = fmt.Errorf("Cove %s is already the latest", appVersion)
	}
	if err == nil {
		err = applyUpdate(rel, fw)
	}
	if err == nil {
		err = restartAfter(os.Getpid())
	}
	if err != nil {
		fmt.Fprintf(fw, "✗ %v\n", err)
		return
	}
	fmt.Fprintln(fw, "Restarting Cove…")
	go func() { time.Sleep(300 * time.Millisecond); shutdown(); quit() }()
}
