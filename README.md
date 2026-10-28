# ≋ Cove

A fast, native local PHP stack for Windows 11, built as a modern replacement for WAMP. It's one
8 MB Go binary with no Apache, no VC++ installer wizard and no services registered in Windows.

| Component | Source | Notes |
|---|---|---|
| PHP 8.1 – 8.5 | windows.php.net release feed (SHA-256 verified) | NTS x64, several versions side by side, OPcache + JIT |
| nginx | nginx.org (latest mainline) | HTTP/2, gzip, graceful reloads |
| MySQL | Oracle CDN (latest release) | root / no password, utf8mb4 |
| Redis | redis-windows builds of upstream Redis (SHA-256 verified) | 127.0.0.1:6379, persisted to `data
edis` |
| Composer | getcomposer.org (latest stable) | plus the global `laravel` installer |
| Node.js | nodejs.org (latest LTS, SHA-256 verified) | several versions side by side; `.nvmrc` / `.node-version` pick one per project |

## Laravel Herd–style features

- **Parked folders**: every folder in `%USERPROFILE%\Sites` is served at `http://<folder>.test`.
- **Linking**: run `cove link` inside any project to serve it as `<name>.test`.
- **Wildcard `.test` DNS**: a built-in DNS server and a Windows NRPT rule, so you never edit the hosts file.
- **One-command HTTPS**: `cove secure` issues a certificate signed by a local CA that Windows trusts.
- **Per-site PHP**: `cove isolate 8.3` pins one site, and every other site follows the default.
- **Mail catcher**: SMTP on `127.0.0.1:2525`. Mails appear in the dashboard and you get a notification for each one.
- **Live logs**: nginx, PHP, MySQL and Cove logs, in the dashboard or with `cove logs nginx -f`.
- **Databases**: list, create and drop MySQL databases from the CLI or the dashboard.
- **Tray app**: start and stop services, switch PHP, open sites, view logs and mail, and set start-at-login.
- **Dashboard**: `http://cove.test`, also at `http://127.0.0.1:7771`.
- Framework detection (Laravel, Symfony, WordPress, Craft, static) and `public/` roots are handled automatically.
- **Automatic databases**: when a site's `.env` points at Cove's MySQL, its `DB_DATABASE` is created for you. New folders in a parked directory are served within seconds.

## Build

```powershell
.\build.ps1          # needs Go 1.22+ → dist\cove.exe + dist\CoveTray.exe
```

## Install

```powershell
.\dist\cove.exe setup
```

`setup` copies Cove into `%USERPROFILE%\.cove\bin` and adds that folder to your PATH. It downloads
everything and asks you twice:

1. **"Install certificate?"**: click *Yes* so the local CA is trusted (current user, no admin needed).
2. **UAC prompt**: adds the `.test` DNS rule (once only).

> **Coming from WAMP?** Cove wants ports 80, 443 and 3306. Stop WAMP first, or change `ports` in
> `%USERPROFILE%\.cove\config.json`. Run `cove doctor` to see what's holding each port.

## CLI

```
cove up | down                      start/stop Cove + tray icon
cove status                         services, versions, ports
cove start|stop|restart [svc]       all | nginx | php | mysql | redis | mail | dns
cove php [8.4]                      list / switch default PHP (auto-installs)
cove node [22]                      list / switch default Node.js (an .nvmrc wins per project)
cove install php 8.3|nginx|mysql|redis|composer|node
cove isolate 8.3 [site] / unisolate
cove sites | park [dir] | link [name] | unlink
cove secure [site] | unsecure [site]
cove open [site] | dashboard
cove logs [name] [-f] [-n 100]      cove logs php -f, cove logs nginx -f
cove db [create|drop name]
cove new my-app                     composer create-project laravel/laravel → my-app.test
cove doctor | trust | dns | hosts | autostart on|off
cove uninstall                      also under Settings → Apps; asks before deleting data
```

## Customising

- PHP: put your overrides in `%USERPROFILE%\.cove\php\<ver>\php.local.ini`.
- MySQL: put your overrides in `%USERPROFILE%\.cove\config\my.local.ini`.
- nginx: add server blocks in `%USERPROFILE%\.cove\config\nginx.d\*.conf`.
- Everything else lives in `config.json`: TLD, ports, PHP worker count and autostart services.
- Set `COVE_HOME` to use a different data location.

## How it's fast

- nginx talks to a **pool of php-cgi workers per PHP version**, load-balanced with failover. Workers
  recycle every 2000 requests.
- OPcache with `revalidate_freq=0` and tracing JIT, and a 16 MB realpath cache.
- Config changes apply with a **graceful nginx reload** and no downtime.
- Every child process lives in a **Windows Job Object**, so if Cove exits nothing is left holding a port.
