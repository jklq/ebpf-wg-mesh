package continuity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gofrs/flock"
)

type child struct {
	launch      Launch
	boot        Launch
	configPath  string
	cmd         *exec.Cmd
	backends    []string
	ready       []string
	stop        []string
	connections int
	exited      atomic.Bool
	done        chan struct{}
}

type Status struct {
	Protocol    int    `json:"protocol"`
	Active      string `json:"active"`
	Draining    string `json:"draining,omitempty"`
	Connections int    `json:"connections"`
	PID         int    `json:"pid"`
}

type Server struct {
	dir         string
	transition  sync.Mutex
	mu          sync.Mutex
	active, old *child
	listeners   []net.Listener
	ctx         context.Context
}

func New(dir string) *Server { return &Server{dir: dir} }

func (s *Server) Run(ctx context.Context) error {
	s.ctx = ctx
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(s.dir, "lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("continuity runtime already owned")
	}
	defer lock.Close()
	var saved Launch
	b, err := os.ReadFile(filepath.Join(s.dir, "active.json"))
	if err == nil {
		if err = json.Unmarshal(b, &saved); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	path := filepath.Join(s.dir, "control.sock")
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(s.Status()) })
	mux.HandleFunc("POST /activate", func(w http.ResponseWriter, r *http.Request) {
		var l Launch
		d := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
		d.DisallowUnknownFields()
		if err := d.Decode(&l); err != nil {
			http.Error(w, "invalid launch", 400)
			return
		}
		// The runtime owns the transition even if the requesting controller disappears.
		activation, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := s.Activate(activation, l); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		json.NewEncoder(w).Encode(s.Status())
	})
	mux.HandleFunc("POST /admission", func(w http.ResponseWriter, r *http.Request) {
		var l Launch
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&l); err != nil {
			http.Error(w, "invalid launch", 400)
			return
		}
		admission, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := s.Admission(admission, l); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		json.NewEncoder(w).Encode(s.Status())
	})
	mux.HandleFunc("POST /finish", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Finish(); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		json.NewEncoder(w).Encode(s.Status())
	})
	h := http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- h.Serve(ln) }()
	if saved.ID != "" {
		go func() {
			for {
				if s.Status().Active != "" {
					return
				}
				startup, cancel := context.WithTimeout(ctx, 30*time.Second)
				err := s.Activate(startup, saved)
				cancel()
				if err == nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		}()
	}
	select {
	case <-ctx.Done():
		h.Close()
		s.close()
		return ctx.Err()
	case err := <-done:
		s.close()
		return err
	}
}

func (s *Server) Seed(l Launch) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(s.dir, "lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("cannot seed a running listener owner")
	}
	defer lock.Close()
	inputs, err := snapshotConfig(s.dir, l)
	if err != nil {
		return err
	}
	return save(filepath.Join(s.dir, "active.json"), bootLaunch(l, inputs))
}

func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := Status{Protocol: Protocol}
	if s.active != nil && !s.active.exited.Load() {
		v.Active = s.active.launch.ID
		v.PID = s.active.cmd.Process.Pid
	}
	if s.old != nil {
		v.Draining = s.old.launch.ID
		v.Connections = s.old.connections
	}
	return v
}

func (s *Server) Activate(ctx context.Context, l Launch) error {
	s.transition.Lock()
	defer s.transition.Unlock()
	return s.activate(ctx, l)
}

func (s *Server) activate(ctx context.Context, l Launch) error {
	if err := l.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.active != nil && s.active.launch.ID == l.ID && !s.active.exited.Load() {
		s.mu.Unlock()
		return nil
	}
	if s.old != nil {
		if s.old.launch.ID == l.ID && !s.old.exited.Load() {
			if err := save(filepath.Join(s.dir, "active.json"), s.old.boot); err != nil {
				s.mu.Unlock()
				return err
			}
			s.active, s.old = s.old, s.active
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		return errors.New("previous generation still draining; finish it before another transition")
	}
	previous := s.active
	if previous != nil && Digest(previous.launch.Listeners) != Digest(l.Listeners) {
		s.mu.Unlock()
		return errors.New("stable listeners cannot change during a handover; restart the owner")
	}
	s.mu.Unlock()
	if err := availableMemory(l.MemoryMiB); err != nil {
		return err
	}
	c, err := start(ctx, s.dir, l)
	if err != nil {
		return err
	}
	if err = s.ready(ctx, c); err != nil {
		terminate(c)
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.exited.Load() {
		return errors.New("candidate exited before traffic handover")
	}
	// Publish durably before allowing connections to select this generation.
	if err = save(filepath.Join(s.dir, "active.json"), c.boot); err != nil {
		terminate(c)
		return err
	}
	if previous == nil && len(s.listeners) == 0 {
		for index, p := range l.Listeners {
			ln, err := net.Listen("tcp", p.Address)
			if err != nil {
				for _, ln := range s.listeners {
					ln.Close()
				}
				s.listeners = nil
				terminate(c)
				return err
			}
			s.listeners = append(s.listeners, ln)
			go s.serve(ln, index)
		}
	}
	s.old, s.active = previous, c
	go s.watch(c)
	return nil
}

// Admission terminates sessions for maintenance/recovery; upgrades must not call it.
func (s *Server) Admission(ctx context.Context, l Launch) error {
	if err := l.Validate(); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(l.ConfigDir, "authority.json"))
	if err != nil {
		return err
	}
	var authority struct {
		Installation string `json:"installationId"`
		Generation   string `json:"generation"`
		Paused       *bool  `json:"paused"`
	}
	if err = json.Unmarshal(b, &authority); err != nil || authority.Installation == "" || authority.Generation == "" || authority.Paused == nil {
		return errors.New("admission requires an explicit provisioned runtime authority")
	}
	s.transition.Lock()
	defer s.transition.Unlock()
	drop := func() {
		s.mu.Lock()
		old := s.old
		s.old = nil
		s.mu.Unlock()
		if old != nil {
			terminate(old)
		}
	}
	drop()
	if err = s.activate(ctx, l); err != nil {
		return err
	}
	drop()
	return nil
}

func (s *Server) ready(ctx context.Context, c *child) error {
	for {
		if c.exited.Load() {
			return errors.New("candidate exited before readiness")
		}
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(probe, c.ready[0], c.ready[1:]...)
		cmd.Env = c.cmd.Env
		err := cmd.Run()
		cancel()
		if err == nil && ownsListeners(c) {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("candidate readiness timed out; serving generation retained")
		case <-c.done:
			return errors.New("candidate exited before readiness")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *Server) serve(ln net.Listener, index int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		c := s.active
		if c == nil || c.exited.Load() {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		c.connections++
		s.mu.Unlock()
		go func() {
			defer func() { conn.Close(); s.mu.Lock(); c.connections--; s.mu.Unlock() }()
			backend, err := net.DialTimeout("tcp", c.backends[index], 3*time.Second)
			if err != nil {
				return
			}
			defer backend.Close()
			done := make(chan struct{})
			go func() {
				io.Copy(backend, conn)
				if tcp, ok := backend.(*net.TCPConn); ok {
					tcp.CloseWrite()
				}
				close(done)
			}()
			io.Copy(conn, backend)
			if tcp, ok := conn.(*net.TCPConn); ok {
				tcp.CloseWrite()
			}
			<-done
		}()
	}
}

func (s *Server) Finish() error {
	s.transition.Lock()
	defer s.transition.Unlock()
	s.mu.Lock()
	c := s.old
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	if len(c.stop) > 0 && !c.exited.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), stopPreparation)
		cmd := exec.CommandContext(ctx, c.stop[0], c.stop[1:]...)
		// Omitting MAINPID keeps stop preparation from marking the active console as draining.
		cmd.Env = c.cmd.Env
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		_ = cmd.Run()
		cancel()
	}
	terminate(c)
	s.mu.Lock()
	if s.old == c {
		s.old = nil
	}
	s.mu.Unlock()
	return nil
}

func (s *Server) watch(c *child) {
	<-c.done
	s.mu.Lock()
	if s.active != c {
		s.mu.Unlock()
		return
	}
	if s.old != nil && !s.old.exited.Load() {
		if err := save(filepath.Join(s.dir, "active.json"), s.old.boot); err == nil {
			s.active, s.old = s.old, nil
			s.mu.Unlock()
			return
		}
	}
	ctx := s.ctx
	s.mu.Unlock()
	if ctx == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
		s.mu.Lock()
		if s.active != c {
			s.mu.Unlock()
			return
		}
		s.active = nil
		s.mu.Unlock()
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.Activate(attempt, c.boot)
		cancel()
		if err == nil {
			return
		}
		s.mu.Lock()
		if s.active == nil {
			s.active = c
		}
		s.mu.Unlock()
	}
}

func (s *Server) close() {
	s.transition.Lock()
	defer s.transition.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ln := range s.listeners {
		ln.Close()
	}
	if s.active != nil {
		terminate(s.active)
	}
	if s.old != nil {
		terminate(s.old)
	}
}

const (
	stopPreparation = 100 * time.Second
	stopGrace       = 110 * time.Second
)

func terminate(c *child) {
	defer os.RemoveAll(c.configPath)
	if c.exited.Load() {
		return
	}
	c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.done:
	case <-time.After(stopGrace):
		c.cmd.Process.Kill()
		<-c.done
	}
}

func availableMemory(wanted int64) error {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			f := strings.Fields(line)
			n, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return err
			}
			if n/1024 < wanted+32 {
				return errors.New("insufficient available memory for safe process overlap")
			}
			return nil
		}
	}
	return errors.New("available host memory could not be observed")
}
