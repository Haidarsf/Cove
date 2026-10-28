package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// A local SMTP catcher: anything sent to 127.0.0.1:2525 is stored and shown in
// the dashboard instead of being delivered. Laravel: MAIL_HOST=127.0.0.1 MAIL_PORT=2525.

type MailMeta struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	To      []string  `json:"to"`
	Subject string    `json:"subject"`
	Date    time.Time `json:"date"`
	Size    int       `json:"size"`
	Read    bool      `json:"read"`
}

type MailFull struct {
	MailMeta
	Text        string            `json:"text"`
	HTML        string            `json:"html"`
	Headers     map[string]string `json:"headers"`
	Attachments []string          `json:"attachments"`
}

var (
	smtpLn   net.Listener
	mailMu   sync.Mutex
	mailSubs = map[chan MailMeta]bool{}
)

func startSMTP() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", getConfig().Ports.SMTP))
	if err != nil {
		return err
	}
	smtpLn = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSMTP(c)
		}
	}()
	return nil
}

func stopSMTP() {
	if smtpLn != nil {
		smtpLn.Close()
		smtpLn = nil
	}
}

func handleSMTP(c net.Conn) {
	defer c.Close()
	r, w := bufio.NewReader(c), bufio.NewWriter(c)
	say := func(s string) { w.WriteString(s + "\r\n"); w.Flush() }
	say("220 cove ESMTP ready")
	var from string
	var to []string
	for {
		c.SetDeadline(time.Now().Add(5 * time.Minute))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			say("250-cove\r\n250-SIZE 52428800\r\n250-8BITMIME\r\n250-AUTH PLAIN LOGIN\r\n250 SMTPUTF8")
		case strings.HasPrefix(cmd, "HELO"):
			say("250 cove")
		case strings.HasPrefix(cmd, "AUTH LOGIN"):
			// Accept any credentials.
			if len(strings.Fields(line)) < 3 {
				say("334 VXNlcm5hbWU6")
				r.ReadString('\n')
			}
			say("334 UGFzc3dvcmQ6")
			r.ReadString('\n')
			say("235 2.7.0 Authentication successful")
		case strings.HasPrefix(cmd, "AUTH"):
			if len(strings.Fields(line)) < 3 {
				say("334 ")
				r.ReadString('\n')
			}
			say("235 2.7.0 Authentication successful")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			from, to = addrArg(line[10:]), nil
			say("250 OK")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			to = append(to, addrArg(line[8:]))
			say("250 OK")
		case cmd == "DATA":
			say("354 End data with <CR><LF>.<CR><LF>")
			var buf bytes.Buffer
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" || l == ".\n" {
					break
				}
				if strings.HasPrefix(l, "..") {
					l = l[1:]
				}
				buf.WriteString(l)
			}
			id := storeMail(from, to, buf.Bytes())
			say("250 OK queued as " + id)
		case cmd == "RSET":
			from, to = "", nil
			say("250 OK")
		case cmd == "NOOP":
			say("250 OK")
		case cmd == "QUIT":
			say("221 Bye")
			return
		default:
			say("502 Command not implemented")
		}
	}
}

func addrArg(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " "); i > 0 {
		s = s[:i]
	}
	return strings.Trim(s, "<>")
}

func mailDir() string { return dataDir("mail") }

func storeMail(from string, to []string, raw []byte) string {
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	meta := MailMeta{ID: id, From: from, To: to, Date: time.Now(), Size: len(raw), Subject: "(no subject)"}
	if m, err := mail.ReadMessage(bytes.NewReader(raw)); err == nil {
		dec := new(mime.WordDecoder)
		if s, err := dec.DecodeHeader(m.Header.Get("Subject")); err == nil && s != "" {
			meta.Subject = s
		}
		if f, err := dec.DecodeHeader(m.Header.Get("From")); err == nil && f != "" {
			meta.From = f
		}
	}
	os.WriteFile(filepath.Join(mailDir(), id+".eml"), raw, 0o644)
	b, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(mailDir(), id+".json"), b, 0o644)
	mailMu.Lock()
	for ch := range mailSubs {
		select {
		case ch <- meta:
		default:
		}
	}
	mailMu.Unlock()
	return id
}

func listMail() []MailMeta {
	files, _ := filepath.Glob(filepath.Join(mailDir(), "*.json"))
	out := []MailMeta{}
	for _, f := range files {
		var m MailMeta
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &m) == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out
}

func safeID(id string) bool {
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return id != ""
}

func readMail(id string) (*MailFull, error) {
	if !safeID(id) {
		return nil, fmt.Errorf("bad id")
	}
	metaPath := filepath.Join(mailDir(), id+".json")
	var full MailFull
	b, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, err
	}
	json.Unmarshal(b, &full.MailMeta)
	if !full.Read {
		full.Read = true
		nb, _ := json.Marshal(full.MailMeta)
		os.WriteFile(metaPath, nb, 0o644)
	}
	raw, err := os.ReadFile(filepath.Join(mailDir(), id+".eml"))
	if err != nil {
		return nil, err
	}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		full.Text = string(raw)
		return &full, nil
	}
	full.Headers = map[string]string{}
	for k, v := range m.Header {
		full.Headers[k] = strings.Join(v, ", ")
	}
	walkPart(m.Header.Get("Content-Type"), m.Header.Get("Content-Transfer-Encoding"), "", m.Body, &full)
	return &full, nil
}

func walkPart(ctype, enc, disp string, body io.Reader, out *MailFull) {
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		mt = "text/plain"
	}
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextRawPart()
			if err != nil {
				return
			}
			walkPart(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p.Header.Get("Content-Disposition"), p, out)
		}
	}
	switch strings.ToLower(enc) {
	case "base64":
		body = base64.NewDecoder(base64.StdEncoding, body) // skips CR/LF itself
	case "quoted-printable":
		body = quotedprintable.NewReader(body)
	}
	data, _ := io.ReadAll(body)
	if strings.HasPrefix(disp, "attachment") || (!strings.HasPrefix(mt, "text/")) {
		_, dp, _ := mime.ParseMediaType(disp)
		name := dp["filename"]
		if name == "" {
			name = params["name"]
		}
		if name == "" {
			name = mt
		}
		out.Attachments = append(out.Attachments, fmt.Sprintf("%s (%d KB)", name, len(data)/1024))
		return
	}
	if mt == "text/html" && out.HTML == "" {
		out.HTML = string(data)
	} else if mt == "text/plain" && out.Text == "" {
		out.Text = string(data)
	}
}

func deleteMail(id string) {
	if id == "" {
		files, _ := filepath.Glob(filepath.Join(mailDir(), "*"))
		for _, f := range files {
			os.Remove(f)
		}
		return
	}
	if safeID(id) {
		os.Remove(filepath.Join(mailDir(), id+".json"))
		os.Remove(filepath.Join(mailDir(), id+".eml"))
	}
}
