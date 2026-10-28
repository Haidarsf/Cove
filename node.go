package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// installedNode returns the installed Node.js major lines, ascending ("22", "24").
func installedNode() []string {
	entries, _ := os.ReadDir(nodeDir())
	var out []string
	for _, e := range entries {
		if e.IsDir() && fileExists(filepath.Join(nodeDir(), e.Name(), "node.exe")) {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[i], out[j]) })
	return out
}

// migrateNodeLayout moves a single-version install (node\node.exe, from
// before versions lived side by side) into node\<major>.
func migrateNodeLayout() {
	if !fileExists(filepath.Join(nodeDir(), "node.exe")) {
		return
	}
	maj := strings.SplitN(versionOf(nodeDir()), ".", 2)[0]
	if maj == "" {
		return
	}
	tmp := nodeDir() + "-" + maj
	if os.Rename(nodeDir(), tmp) != nil {
		return
	}
	os.MkdirAll(nodeDir(), 0o755)
	if os.Rename(tmp, nodeVersionDir(maj)) == nil && getConfig().DefaultNode == "" {
		updateConfig(func(c *Config) { c.DefaultNode = maj })
	}
}

// ltsMajor maps nvm's lts/<codename> aliases to their major line.
var ltsMajor = map[string]string{
	"argon": "4", "boron": "6", "carbon": "8", "dubnium": "10", "erbium": "12", "fermium": "14",
	"gallium": "16", "hydrogen": "18", "iron": "20", "jod": "22", "krypton": "24", "lithium": "26",
}

var nodeVerRe = regexp.MustCompile(`^v?(\d+)`)

// resolveNode picks the major line for dir: the nearest .nvmrc or
// .node-version wins, else the default. It returns where the choice came from.
func resolveNode(dir string) (major, source string) {
	for d := dir; ; d = filepath.Dir(d) {
		for _, f := range []string{".nvmrc", ".node-version"} {
			b, err := os.ReadFile(filepath.Join(d, f))
			if err != nil {
				continue
			}
			want := strings.ToLower(strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]))
			if m := nodeVerRe.FindStringSubmatch(want); m != nil {
				return m[1], filepath.Join(d, f)
			}
			if maj, ok := ltsMajor[strings.TrimPrefix(want, "lts/")]; ok {
				return maj, filepath.Join(d, f)
			}
			// "node", "lts/*" or anything else: the default is the latest we have.
			return getConfig().DefaultNode, filepath.Join(d, f)
		}
		if filepath.Dir(d) == d {
			return getConfig().DefaultNode, "default"
		}
	}
}

// runNodeTool is what the node/npm/npx shims call: it runs the tool from the
// resolved version with that version first on PATH (so npm scripts that spawn
// node get the same one) and passes the exit code through.
func runNodeTool(tool string, args []string) int {
	cwd, _ := os.Getwd()
	major, source := resolveNode(cwd)
	dir := nodeVersionDir(major)
	if major == "" || !fileExists(filepath.Join(dir, "node.exe")) {
		fmt.Fprintf(os.Stderr, "cove: Node.js %s (from %s) is not installed — run `cove install node %s`\n", major, source, major)
		return 1
	}
	argv := args
	switch tool {
	case "npm", "npx":
		argv = append([]string{filepath.Join(dir, "node_modules", "npm", "bin", tool+"-cli.js")}, args...)
	}
	cmd := exec.Command(filepath.Join(dir, "node.exe"), argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "PATH="+dir+";"+os.Getenv("PATH"))
	// Ctrl+C reaches node directly through the shared console; we just wait.
	signal.Ignore(os.Interrupt)
	if err := cmd.Run(); err != nil {
		if cmd.ProcessState != nil {
			return cmd.ProcessState.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "cove:", err)
		return 1
	}
	return 0
}

// cmdNode lists installed versions, or switches the default (installing it first if needed).
func cmdNode(args []string) error {
	if !running() {
		migrateNodeLayout()
	}
	if len(args) == 0 {
		c := getConfig()
		installed := installedNode()
		if len(installed) == 0 {
			fmt.Println("No Node.js installed — run `cove install node`")
		}
		cwd, _ := os.Getwd()
		here, source := resolveNode(cwd)
		for _, v := range installed {
			mark := "  "
			if v == c.DefaultNode {
				mark = cGreen + "● " + cReset
			}
			note := ""
			if v == here && source != "default" {
				note = cDim + "  ← " + source + cReset
			}
			fmt.Printf("  %s%-4s %s%s%s%s\n", mark, v, cDim, versionOf(nodeVersionDir(v)), cReset, note)
		}
		return nil
	}
	major := strings.TrimPrefix(args[0], "v")
	if !fileExists(filepath.Join(nodeVersionDir(major), "node.exe")) {
		if err := cmdInstall([]string{"node", major}); err != nil {
			return err
		}
	}
	// The daemon owns config.json while it runs, so ask it to make the change.
	var err error
	if running() {
		_, err = call("POST", "/api/node/default", map[string]string{"major": major})
	} else {
		err = setDefaultNode(major)
	}
	if err != nil {
		return err
	}
	ok("Default Node.js is now " + versionOf(nodeVersionDir(major)) + "  (projects with an .nvmrc keep their own)")
	return nil
}

func setDefaultNode(major string) error {
	if !fileExists(filepath.Join(nodeVersionDir(major), "node.exe")) {
		return fmt.Errorf("Node.js %s is not installed", major)
	}
	if err := updateConfig(func(c *Config) { c.DefaultNode = major }); err != nil {
		return err
	}
	writeShims()
	return nil
}

type nodeInfo struct {
	Major   string `json:"major"`
	Version string `json:"version"`
	Default bool   `json:"default"`
}

func nodeVersions(def string) []nodeInfo {
	var out []nodeInfo
	for _, v := range installedNode() {
		out = append(out, nodeInfo{v, versionOf(nodeVersionDir(v)), v == def})
	}
	return out
}
