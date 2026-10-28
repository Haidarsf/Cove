package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func pgDir() string     { return filepath.Join(Home(), "postgres") }
func pgDataDir() string { return filepath.Join(Home(), "data", "postgres") }
func pgBin(tool string) string {
	return filepath.Join(pgDir(), "bin", tool+".exe")
}

// latestPostgres reads postgresql.org's release list for the current major.
func latestPostgres() (string, error) {
	body, err := getString("https://www.postgresql.org/versions.json")
	if err != nil {
		return "", err
	}
	var vs []struct {
		Major       string
		LatestMinor json.Number
		Current     bool
	}
	if err := json.Unmarshal([]byte(body), &vs); err != nil {
		return "", err
	}
	for _, v := range vs {
		if v.Current {
			return v.Major + "." + v.LatestMinor.String(), nil
		}
	}
	return "", fmt.Errorf("no current PostgreSQL release found")
}

// installPostgres fetches EnterpriseDB's official Windows binaries, leaving
// out pgAdmin, StackBuilder, docs and debug symbols (most of the download).
func installPostgres(w io.Writer) error {
	v, err := latestPostgres()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "→ PostgreSQL %s (≈330 MB, this takes a moment)\n", v)
	zipPath, err := download(fmt.Sprintf("https://get.enterprisedb.com/postgresql/postgresql-%s-1-windows-x64-binaries.zip", v), "", w)
	if err != nil {
		return err
	}
	defer os.Remove(zipPath)
	os.RemoveAll(pgDir())
	skip := func(name string) bool {
		for _, p := range []string{"pgAdmin 4/", "StackBuilder/", "symbols/", "doc/"} {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	if err := unzipSkip(zipPath, pgDir(), true, skip); err != nil {
		return err
	}
	os.WriteFile(filepath.Join(pgDir(), "VERSION"), []byte(v), 0o644)
	// A PostgreSQL already installed as a Windows service usually holds 5432;
	// leave it alone and take the next free port.
	if p := getConfig().Ports.Postgres; portFree(p) != nil {
		for q := p + 1; q < p+20; q++ {
			if portFree(q) == nil {
				fmt.Fprintf(w, "  port %d is taken (another PostgreSQL?) — using %d\n", p, q)
				updateConfig(func(c *Config) { c.Ports.Postgres = q })
				break
			}
		}
	}
	addAutostart("postgres")
	writeShims()
	fmt.Fprintf(w, "✓ PostgreSQL %s installed (user postgres, no password, port %d)\n", v, getConfig().Ports.Postgres)
	return nil
}

// preparePostgres creates the cluster on first start: UTF-8, C locale, and
// trust auth for loopback only (it only listens on 127.0.0.1).
func preparePostgres() error {
	c := getConfig()
	if err := portFree(c.Ports.Postgres); err != nil {
		return err
	}
	if fileExists(filepath.Join(pgDataDir(), "PG_VERSION")) {
		return nil
	}
	log.Printf("initializing PostgreSQL data directory (first run)")
	os.MkdirAll(filepath.Dir(pgDataDir()), 0o755)
	out, err := hidden(exec.Command(pgBin("initdb"), "-D", pgDataDir(), "-U", "postgres", "-A", "trust",
		"-E", "UTF8", "--no-locale")).CombinedOutput()
	os.WriteFile(filepath.Join(logDir(), "postgres-init.log"), out, 0o644)
	if err != nil {
		os.RemoveAll(pgDataDir())
		return fmt.Errorf("postgres init failed — see logs/postgres-init.log")
	}
	return nil
}

func postgresCmd() *exec.Cmd {
	return exec.Command(pgBin("postgres"), "-D", pgDataDir(), "-p", fmt.Sprint(getConfig().Ports.Postgres),
		"-c", "listen_addresses=127.0.0.1")
}

func stopPostgres(p *Proc) {
	hidden(exec.Command(pgBin("pg_ctl"), "stop", "-D", pgDataDir(), "-m", "fast", "-w")).Run()
}

func pgExec(sql string) (string, error) {
	out, err := hidden(exec.Command(pgBin("psql"), "-h", "127.0.0.1", "-p", fmt.Sprint(getConfig().Ports.Postgres),
		"-U", "postgres", "-d", "postgres", "-tA", "-F", "\t", "-c", sql)).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

type pgDB struct {
	Name   string `json:"name"`
	SizeMB string `json:"size_mb"`
}

func listPostgresDBs() ([]pgDB, error) {
	out, err := pgExec("SELECT datname, round(pg_database_size(datname)/1024.0/1024.0, 2) FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres' ORDER BY datname")
	var dbs []pgDB
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Split(strings.TrimRight(line, "\r"), "\t"); len(f) == 2 {
			dbs = append(dbs, pgDB{f[0], f[1]})
		}
	}
	return dbs, err
}

// createPostgresDB is a no-op when the database exists. Names are checked by
// the callers (dbNameRe), and quoted.
func createPostgresDB(name string) (bool, error) {
	out, err := pgExec("SELECT 1 FROM pg_database WHERE datname = '" + name + "'")
	if err != nil || strings.TrimSpace(out) == "1" {
		return false, err
	}
	_, err = pgExec(`CREATE DATABASE "` + name + `" ENCODING 'UTF8'`)
	return err == nil, err
}
