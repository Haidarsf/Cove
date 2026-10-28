package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

// A tiny authoritative DNS server: every *.test name resolves to 127.0.0.1.
// Windows is pointed at it for the .test namespace only through an NRPT rule
// (`cove setup` adds it once, elevated), so no hosts-file edits per site.

var dnsConn *net.UDPConn

func startDNS() error {
	c := getConfig()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: c.Ports.DNS})
	if err != nil {
		return fmt.Errorf("DNS port %d: %w", c.Ports.DNS, err)
	}
	dnsConn = conn
	tld := "." + strings.ToLower(c.TLD) + "."
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if resp := dnsAnswer(buf[:n], tld); resp != nil {
				conn.WriteToUDP(resp, addr)
			}
		}
	}()
	return nil
}

func stopDNS() {
	if dnsConn != nil {
		dnsConn.Close()
		dnsConn = nil
	}
}

func dnsAnswer(q []byte, tld string) []byte {
	if len(q) < 12 || binary.BigEndian.Uint16(q[4:6]) != 1 {
		return nil
	}
	// Walk the question name.
	i, labels := 12, []string{}
	for i < len(q) && q[i] != 0 {
		l := int(q[i])
		if l&0xC0 != 0 || i+1+l > len(q) {
			return nil
		}
		labels = append(labels, string(q[i+1:i+1+l]))
		i += 1 + l
	}
	i++
	if i+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[i : i+2])
	qEnd := i + 4
	name := strings.ToLower(strings.Join(labels, ".")) + "."

	resp := make([]byte, 0, 128)
	resp = append(resp, q[0], q[1])
	flags := uint16(0x8400) | (binary.BigEndian.Uint16(q[2:4]) & 0x0100) // QR, AA, copy RD
	ours := strings.HasSuffix("."+name, tld)
	answer := ours && (qtype == 1 || qtype == 255) // A or ANY
	if !ours {
		flags |= 5 // REFUSED
	}
	resp = binary.BigEndian.AppendUint16(resp, flags)
	resp = binary.BigEndian.AppendUint16(resp, 1)
	an := uint16(0)
	if answer {
		an = 1
	}
	resp = binary.BigEndian.AppendUint16(resp, an)
	resp = append(resp, 0, 0, 0, 0)
	resp = append(resp, q[12:qEnd]...)
	if answer {
		resp = append(resp, 0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 127, 0, 0, 1)
	}
	return resp
}

// ---------- NRPT (one-time, admin) ----------

func nrptScript(tld string) string {
	return fmt.Sprintf(`Get-DnsClientNrptRule | Where-Object { $_.Namespace -contains '.%[1]s' } | Remove-DnsClientNrptRule -Force; `+
		`Add-DnsClientNrptRule -Namespace '.%[1]s' -NameServers '127.0.0.1' -Comment 'Cove'; Clear-DnsClientCache`, tld)
}

func nrptInstalled(tld string) bool {
	out, _ := hidden(exec.Command("powershell", "-NoProfile", "-Command",
		// Namespace is an array and a lone CIM result has no .Count, hence @() and -contains.
		fmt.Sprintf(`@(Get-DnsClientNrptRule | Where-Object { $_.Namespace -contains '.%s' }).Count`, tld))).Output()
	return strings.TrimSpace(string(out)) != "0" && strings.TrimSpace(string(out)) != ""
}

// runElevated re-launches cove.exe with admin rights (one UAC prompt) and waits.
func runElevated(args ...string) error {
	exe, _ := os.Executable()
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", "''") + "'"
	}
	ps := fmt.Sprintf(`$p = Start-Process -FilePath '%s' -ArgumentList %s -Verb RunAs -Wait -PassThru -WindowStyle Hidden; exit $p.ExitCode`,
		exe, strings.Join(quoted, ","))
	return exec.Command("powershell", "-NoProfile", "-Command", ps).Run()
}

// ---------- hosts-file fallback ----------

const hostsBegin, hostsEnd = "# >>> cove", "# <<< cove"

func hostsFile() string {
	return os.Getenv("SystemRoot") + `\System32\drivers\etc\hosts`
}

func writeHosts() error {
	b, err := os.ReadFile(hostsFile())
	if err != nil {
		return err
	}
	content := string(b)
	if s, e := strings.Index(content, hostsBegin), strings.Index(content, hostsEnd); s >= 0 && e > s {
		content = content[:s] + content[e+len(hostsEnd):]
	}
	content = strings.TrimRight(content, "\r\n") + "\r\n\r\n" + hostsBegin + "\r\n"
	content += "127.0.0.1 cove." + getConfig().TLD + "\r\n"
	for _, s := range listSites() {
		content += "127.0.0.1 " + s.Host + " www." + s.Host + "\r\n"
	}
	content += hostsEnd + "\r\n"
	return os.WriteFile(hostsFile(), []byte(content), 0o644)
}
