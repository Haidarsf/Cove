package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// watchSites polls the parked folders: a new or removed project folder is
// served (or dropped) without a manual reload, and databases a site's .env
// asks for are created as soon as the database server is up.
func watchSites() {
	var lastSites, lastEnv string
	for range time.Tick(3 * time.Second) {
		var sites, env strings.Builder
		for _, s := range listSites() {
			fmt.Fprintf(&sites, "%s=%s;", s.Name, s.Path)
			if fi, err := os.Stat(filepath.Join(s.Path, ".env")); err == nil {
				fmt.Fprintf(&env, "%s:%d;", s.Name, fi.ModTime().UnixNano())
			}
		}
		for _, name := range []string{"mysql"} {
			fmt.Fprintf(&env, "%s:%s;", name, serviceMap[name].Status().State)
		}
		if sites.String() != lastSites {
			if lastSites != "" {
				if err := reconcile(); err != nil {
					log.Printf("sites changed: %v", err)
				}
			}
			lastSites = sites.String()
		}
		if env.String() != lastEnv {
			lastEnv = env.String()
			ensureSiteDatabases()
		}
	}
}

var dbNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`) // used inside backticks, so - is safe

// ensureSiteDatabases creates the DB_DATABASE of every site whose .env points
// at Cove's own database server.
func ensureSiteDatabases() {
	c := getConfig()
	if serviceMap["mysql"].Status().State != "running" {
		return
	}
	for _, s := range listSites() {
		env := readEnv(filepath.Join(s.Path, ".env"))
		conn, name := env["DB_CONNECTION"], env["DB_DATABASE"]
		host, port := env["DB_HOST"], env["DB_PORT"]
		if (conn != "mysql" && conn != "mariadb") || !dbNameRe.MatchString(name) {
			continue
		}
		if (host != "" && host != "127.0.0.1" && host != "localhost") || (port != "" && port != fmt.Sprint(c.Ports.MySQL)) {
			continue
		}
		out, err := mysqlExec("SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='" + name + "'")
		if err != nil || strings.TrimSpace(out) != "0" {
			continue
		}
		if _, err := mysqlExec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			log.Printf("creating database %s for %s: %v", name, s.Host, err)
		} else {
			log.Printf("created database %s for %s", name, s.Host)
		}
	}
}

// readEnv parses the KEY=value lines of a .env file (quotes and trailing
// comments stripped; ${VAR} references are not expanded).
func readEnv(path string) map[string]string {
	env := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return env
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		k, v = strings.TrimSpace(strings.TrimPrefix(k, "export ")), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		env[k] = v
	}
	return env
}
