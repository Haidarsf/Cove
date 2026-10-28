package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A Windows job object with KILL_ON_JOB_CLOSE: if Cove dies, every nginx,
// php-cgi and mysqld it started dies with it. No orphaned processes holding ports.
var job windows.Handle

func initJob() {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(h)
		return
	}
	job = h
}

func addToJob(pid int) {
	if job == 0 {
		return
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	windows.AssignProcessToJobObject(job, ph)
	windows.CloseHandle(ph)
}

// hidden makes child processes run without flashing console windows.
func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 /* CREATE_NO_WINDOW */}
	return cmd
}

// Proc is one supervised external process that is restarted if it exits.
type Proc struct {
	Name  string
	Log   string
	Build func() *exec.Cmd
	Stop  func(p *Proc) // optional graceful stop; falls back to killing the tree

	mu       sync.Mutex
	cmd      *exec.Cmd
	want     bool
	state    string // running | stopped | error | starting
	lastErr  string
	started  time.Time
	restarts int
	done     chan struct{}
}

func (p *Proc) State() (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == "" {
		return "stopped", ""
	}
	return p.state, p.lastErr
}

func (p *Proc) Pid() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil && p.state == "running" {
		return p.cmd.Process.Pid
	}
	return 0
}

func (p *Proc) Start() {
	p.mu.Lock()
	if p.want {
		p.mu.Unlock()
		return
	}
	p.want, p.state, p.lastErr, p.restarts = true, "starting", "", 0
	p.done = make(chan struct{})
	p.mu.Unlock()
	go p.loop()
}

func (p *Proc) loop() {
	defer close(p.done)
	var quickFails int
	for {
		p.mu.Lock()
		if !p.want {
			p.state = "stopped"
			p.mu.Unlock()
			return
		}
		cmd := hidden(p.Build())
		logf, _ := os.OpenFile(p.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		cmd.Stdout, cmd.Stderr = logf, logf
		err := cmd.Start()
		if err != nil {
			p.state, p.lastErr, p.want = "error", err.Error(), false
			p.mu.Unlock()
			if logf != nil {
				logf.Close()
			}
			return
		}
		addToJob(cmd.Process.Pid)
		p.cmd, p.state, p.started = cmd, "running", time.Now()
		p.mu.Unlock()

		err = cmd.Wait()
		if logf != nil {
			logf.Close()
		}

		p.mu.Lock()
		ran := time.Since(p.started)
		if !p.want {
			p.state = "stopped"
			p.mu.Unlock()
			return
		}
		// php-cgi intentionally exits after PHP_FCGI_MAX_REQUESTS; only treat
		// rapid repeated exits as a crash loop.
		if ran < 3*time.Second {
			quickFails++
		} else {
			quickFails = 0
		}
		p.restarts++
		if quickFails >= 5 {
			msg := "exited repeatedly"
			if err != nil {
				msg = err.Error()
			}
			p.state, p.lastErr, p.want = "error", msg+" — see "+p.Log, false
			p.mu.Unlock()
			return
		}
		p.state = "starting"
		p.mu.Unlock()
		time.Sleep(time.Duration(quickFails) * 400 * time.Millisecond)
	}
}

func (p *Proc) StopAndWait() {
	p.mu.Lock()
	if !p.want && p.state != "running" {
		p.mu.Unlock()
		return
	}
	p.want = false
	cmd, done := p.cmd, p.done
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		if p.Stop != nil {
			p.Stop(p)
			select {
			case <-done:
				return
			case <-time.After(20 * time.Second):
			}
		}
		killTree(cmd.Process.Pid)
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}
}

func killTree(pid int) {
	hidden(exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))).Run()
}

// ---------- services ----------

type Service interface {
	Name() string
	Label() string
	Start() error
	Stop()
	Status() ServiceStatus
}

type ServiceStatus struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	State     string `json:"state"` // running | stopped | starting | error | missing
	Error     string `json:"error,omitempty"`
	Version   string `json:"version,omitempty"`
	Port      string `json:"port,omitempty"`
	Pid       int    `json:"pid,omitempty"`
	Uptime    int64  `json:"uptime,omitempty"`
	Restarts  int    `json:"restarts,omitempty"`
	Installed bool   `json:"installed"`
}

// ProcService groups one or more Procs (nginx: 1, mysql: 1).
type ProcService struct {
	name, label string
	version     func() string
	port        func() string
	installed   func() bool
	pre         func() error
	procs       func() []*Proc
}

func (s *ProcService) Name() string  { return s.name }
func (s *ProcService) Label() string { return s.label }

func (s *ProcService) Start() error {
	if !s.installed() {
		return fmt.Errorf("%s is not installed — run `cove install %s`", s.label, s.name)
	}
	if s.pre != nil {
		if err := s.pre(); err != nil {
			return err
		}
	}
	for _, p := range s.procs() {
		p.Start()
	}
	return nil
}

func (s *ProcService) Stop() {
	var wg sync.WaitGroup
	for _, p := range s.procs() {
		wg.Add(1)
		go func(p *Proc) { defer wg.Done(); p.StopAndWait() }(p)
	}
	wg.Wait()
}

func (s *ProcService) Status() ServiceStatus {
	st := ServiceStatus{Name: s.name, Label: s.label, Installed: s.installed(), State: "stopped"}
	if s.version != nil {
		st.Version = s.version()
	}
	if s.port != nil {
		st.Port = s.port()
	}
	if !st.Installed {
		st.State = "missing"
		return st
	}
	procs := s.procs()
	running := 0
	for _, p := range procs {
		state, err := p.State()
		switch state {
		case "running":
			running++
			if st.Pid == 0 {
				st.Pid = p.Pid()
				p.mu.Lock()
				st.Uptime = int64(time.Since(p.started).Seconds())
				p.mu.Unlock()
			}
		case "error":
			st.State, st.Error = "error", err
		case "starting":
			if st.State != "error" {
				st.State = "starting"
			}
		}
		p.mu.Lock()
		st.Restarts += p.restarts
		p.mu.Unlock()
	}
	if running > 0 && running == len(procs) {
		st.State = "running"
	}
	return st
}

// FuncService wraps an in-process server (DNS, mail).
type FuncService struct {
	name, label, port string
	start             func() error
	stop              func()
	mu                sync.Mutex
	running           bool
	err               string
	since             time.Time
}

func (s *FuncService) Name() string  { return s.name }
func (s *FuncService) Label() string { return s.label }
func (s *FuncService) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	if err := s.start(); err != nil {
		s.err = err.Error()
		return err
	}
	s.running, s.err, s.since = true, "", time.Now()
	return nil
}
func (s *FuncService) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.stop()
		s.running = false
	}
}
func (s *FuncService) Status() ServiceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := ServiceStatus{Name: s.name, Label: s.label, Installed: true, State: "stopped", Port: s.port, Version: "built-in"}
	if s.running {
		st.State, st.Pid, st.Uptime = "running", os.Getpid(), int64(time.Since(s.since).Seconds())
	} else if s.err != "" {
		st.State, st.Error = "error", s.err
	}
	return st
}
