package main

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A server is one `claude remote-control` process for one project, restarted
// when it exits. Three quick exits in a row mean it can't run; it then stays
// "failed" with its last output until it is switched on again.
type server struct {
	name  string
	stop  chan struct{} // closed to switch it off
	ended chan struct{} // closed when its process is gone for good

	mu      sync.Mutex
	cmd     *exec.Cmd
	since   time.Time
	failed  bool
	partial string
	lines   []string
	url     string
}

var servers = struct {
	sync.Mutex
	m map[string]*server
}{m: map[string]*server{}}

var claudeURL = regexp.MustCompile(`https://claude\.ai/[^\s"'<>]+`)

func serverFor(name string) *server {
	servers.Lock()
	defer servers.Unlock()
	return servers.m[name]
}

func switchOn(name string) {
	servers.Lock()
	defer servers.Unlock()
	if s := servers.m[name]; s != nil && !s.isFailed() {
		return
	}
	s := &server{name: name, stop: make(chan struct{}), ended: make(chan struct{})}
	servers.m[name] = s
	go s.run()
}

func switchOff(name string) *server {
	servers.Lock()
	s := servers.m[name]
	delete(servers.m, name)
	servers.Unlock()
	if s != nil {
		close(s.stop)
	}
	return s
}

func restartAll() {
	servers.Lock()
	var names []string
	for name := range servers.m {
		names = append(names, name)
	}
	servers.Unlock()
	for _, name := range names {
		if s := switchOff(name); s != nil { // nil: switched off by hand meanwhile
			<-s.ended
			switchOn(name)
		}
	}
}

func stopAll() {
	servers.Lock()
	var all []*server
	for name, s := range servers.m {
		all = append(all, s)
		delete(servers.m, name)
	}
	servers.Unlock()
	for _, s := range all {
		close(s.stop)
	}
	for _, s := range all {
		<-s.ended
	}
}

func (s *server) run() {
	defer close(s.ended)
	quick := 0
	for {
		cmd := exec.Command("claude", "remote-control", "--name", s.name, "--permission-mode", permMode)
		cmd.Dir = filepath.Join(projectsDir, s.name)
		cmd.Stdout, cmd.Stderr = s, s
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own group, so stopping it also stops what it started
		cmd.WaitDelay = 2 * time.Second
		started := time.Now()
		if err := cmd.Start(); err != nil {
			s.Write([]byte("cannot start claude: " + err.Error() + "\n"))
		} else {
			s.setProcess(cmd, started)
			exited := make(chan struct{})
			go func() { cmd.Wait(); close(exited) }()
			stayedUp := time.After(20 * time.Second)
			for running := true; running; {
				select {
				case <-stayedUp:
					remoteControlWorks()
				case <-exited:
					syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // whatever it left running
					running = false
				case <-s.stop:
					syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
					select {
					case <-exited:
					case <-time.After(10 * time.Second):
						syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
						<-exited
					}
					return
				}
			}
			s.flush()
			s.setProcess(nil, time.Time{})
		}
		if time.Since(started) < 20*time.Second {
			quick++
		} else {
			quick = 0
		}
		if quick >= 3 {
			s.mu.Lock()
			s.failed = true
			s.mu.Unlock()
			return
		}
		select {
		case <-time.After(time.Duration(quick) * 3 * time.Second):
		case <-s.stop:
			return
		}
	}
}

func (s *server) setProcess(cmd *exec.Cmd, since time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmd, s.since = cmd, since
}

func (s *server) isFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

func (s *server) status() (state, since, url, reason string, untrusted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.failed && s.cmd != nil && time.Since(s.since) > 3*time.Second:
		return "on", s.since.Format("2 Jan 15:04"), s.url, "", false
	case !s.failed:
		return "starting", "", "", "", false
	}
	// its own words for the known cases, otherwise the last thing it printed
	output := strings.Join(s.lines, "\n")
	switch {
	case strings.Contains(output, "Workspace not trusted"):
		return "failed", "", "", "folder isn't trusted", true
	case strings.Contains(output, "Enable Remote Control?"):
		return "failed", "", "", "Remote Control is off here", false
	case len(s.lines) > 0:
		reason = s.lines[len(s.lines)-1]
		if r := []rune(reason); len(r) > 100 {
			reason = string(r[:100]) + "…"
		}
	}
	return "failed", "", "", reason, false
}

// Write collects the process's output as lines, keeping the last few.
func (s *server) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partial += string(p)
	for {
		i := strings.IndexAny(s.partial, "\r\n")
		if i < 0 {
			break
		}
		s.addLine(s.partial[:i])
		s.partial = s.partial[i+1:]
	}
	if len(s.partial) > 4096 {
		s.addLine(s.partial)
		s.partial = ""
	}
	return len(p), nil
}

// flush keeps the last thing the process printed when it had no line break.
func (s *server) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addLine(s.partial)
	s.partial = ""
}

func (s *server) addLine(raw string) {
	line := strings.TrimSpace(plain([]byte(raw)))
	if line == "" {
		return
	}
	if u := claudeURL.FindString(line); u != "" {
		s.url = u
	}
	if r := []rune(line); len(r) > 300 {
		line = string(r[:300])
	}
	s.lines = append(s.lines, line)
	if len(s.lines) > 20 {
		s.lines = s.lines[len(s.lines)-20:]
	}
	if strings.Contains(line, "Enable Remote Control?") {
		remoteControlNeeded(s.name)
	}
}
