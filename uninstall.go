package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\Cove`

// registerUninstall lists Cove under Settings → Apps → Installed apps (per
// user, no admin), with Uninstall running `cove uninstall`.
func registerUninstall() error {
	bd := binDir()
	k, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	cove := filepath.Join(bd, "cove.exe")
	for name, v := range map[string]string{
		"DisplayName":          "Cove",
		"DisplayVersion":       appVersion,
		"Publisher":            "Cove",
		"DisplayIcon":          filepath.Join(bd, "CoveApp.exe") + ",0",
		"InstallLocation":      Home(),
		"UninstallString":      `"` + cove + `" uninstall`,
		"QuietUninstallString": `"` + cove + `" uninstall --yes`,
		"URLInfoAbout":         "http://cove." + getConfig().TLD,
	} {
		k.SetStringValue(name, v)
	}
	k.SetDWordValue("NoModify", 1)
	k.SetDWordValue("NoRepair", 1)
	var size int64
	filepath.WalkDir(Home(), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				size += fi.Size()
			}
		}
		return nil
	})
	return k.SetDWordValue("EstimatedSize", uint32(size/1024))
}

// cmdUninstall reverses `cove setup`. Data (MySQL databases, certificates,
// config) is only deleted when the user says so.
func cmdUninstall(args []string) error {
	yes := len(args) > 0 && (args[0] == "--yes" || args[0] == "-y")
	in := bufio.NewReader(os.Stdin)
	ask := func(q string) bool {
		if yes {
			return false // quiet uninstall never deletes data
		}
		fmt.Print(q + " [y/N] ")
		a, _ := in.ReadString('\n')
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(a)), "y")
	}
	if !yes && !ask("Uninstall Cove? Sites in your folders are not touched.") {
		return nil
	}
	wipe := ask("Also delete " + Home() + " — PHP, MySQL databases, certificates and config?")

	fmt.Println(cBold + "\nUninstalling Cove" + cReset)
	if running() {
		call("POST", "/api/quit", nil)
		ok("Cove stopped")
	}
	exec.Command("taskkill", "/F", "/IM", "CoveApp.exe").Run()
	setAutostart(false)
	ok("Autostart removed")
	for _, f := range []*windows.KNOWNFOLDERID{windows.FOLDERID_Programs, windows.FOLDERID_Desktop} {
		if d, err := windows.KnownFolderPath(f, 0); err == nil {
			os.Remove(filepath.Join(d, "Cove.lnk"))
		}
	}
	ok("Shortcuts removed")
	if err := removeFromUserPath(binDir()); err == nil {
		ok("Removed " + binDir() + " from PATH")
	}
	if err := untrustCA(); err == nil {
		ok("Cove certificate authority untrusted")
	}
	info("Removing the *." + getConfig().TLD + " DNS rule — approve the admin prompt")
	if err := runElevated("__unsystem"); err != nil {
		fmt.Println(cYellow + "! DNS rule / hosts entries not removed: " + err.Error() + cReset)
	} else {
		ok("DNS rule and hosts entries removed")
	}
	registry.DeleteKey(registry.CURRENT_USER, uninstallKey)

	// This exe lives in the folder being removed, so a detached cmd deletes it
	// once we have exited.
	target := binDir()
	if wipe {
		target = Home()
	}
	cmd := exec.Command("cmd", "/c", fmt.Sprintf(`timeout /t 2 /nobreak >nul & rmdir /s /q "%s"`, target))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008 /* DETACHED_PROCESS */}
	cmd.Start()
	if wipe {
		ok("Deleting " + Home())
	} else {
		ok("Removed the Cove programs — your data is kept in " + Home())
	}
	fmt.Println("\nCove is uninstalled.")
	return nil
}

// unsystem is the elevated half of uninstall: the NRPT rule and hosts block.
func unsystem() error {
	tld := getConfig().TLD
	exec.Command("powershell", "-NoProfile", "-Command", fmt.Sprintf(
		`Get-DnsClientNrptRule | Where-Object { $_.Namespace -contains '.%s' } | Remove-DnsClientNrptRule -Force; Clear-DnsClientCache`, tld)).Run()
	b, err := os.ReadFile(hostsFile())
	if err != nil {
		return err
	}
	content := string(b)
	if s, e := strings.Index(content, hostsBegin), strings.Index(content, hostsEnd); s >= 0 && e > s {
		content = strings.TrimRight(content[:s], "\r\n") + "\r\n" + strings.TrimLeft(content[e+len(hostsEnd):], "\r\n")
		return os.WriteFile(hostsFile(), []byte(content), 0o644)
	}
	return nil
}

func removeFromUserPath(d string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	cur, _, _ := k.GetStringValue("Path")
	var keep []string
	for _, p := range strings.Split(cur, ";") {
		if p != "" && !strings.EqualFold(strings.TrimRight(p, `\`), strings.TrimRight(d, `\`)) {
			keep = append(keep, p)
		}
	}
	if err := k.SetExpandStringValue("Path", strings.Join(keep, ";")); err != nil {
		return err
	}
	broadcastEnvChange()
	return nil
}
