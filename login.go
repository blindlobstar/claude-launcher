package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// A terminal runs a claude command that insists on one and answers it for the
// person at the page: `claude auth login` (link out, code in) and the one-time
// "Enable Remote Control?" question. It works by reading what the command
// prints, so a change in Claude Code's wording can break it.
type terminal struct {
	cmd    *exec.Cmd
	tty    *os.File
	exited chan struct{}
	err    error // valid once exited is closed

	mu     sync.Mutex
	screen []byte
}

func startTerminal(dir string, args ...string) (*terminal, error) {
	cmd := exec.Command("claude", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	// very wide, so a long link stays on one line
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 1000})
	if err != nil {
		return nil, err
	}
	t := &terminal{cmd: cmd, tty: tty, exited: make(chan struct{})}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := tty.Read(buf)
			t.mu.Lock()
			t.screen = append(t.screen, buf[:n]...)
			if extra := len(t.screen) - 1<<16; extra > 0 {
				t.screen = t.screen[extra:]
			}
			t.mu.Unlock()
			if err != nil {
				break
			}
		}
		t.err = cmd.Wait()
		tty.Close()
		close(t.exited)
	}()
	return t, nil
}

func (t *terminal) text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return plain(t.screen)
}

func (t *terminal) running() bool {
	select {
	case <-t.exited:
		return false
	default:
		return true
	}
}

func (t *terminal) stop() {
	syscall.Kill(-t.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-t.exited:
	case <-time.After(5 * time.Second):
		syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		<-t.exited
	}
}

type loginView struct {
	Active bool
	URL    string
	Sent   bool
}

var login struct {
	sync.Mutex
	term      *terminal
	sent      bool
	cancelled bool
}

var anyURL = regexp.MustCompile(`https://[^\s"'<>]+`)

func startLogin() error {
	login.Lock()
	defer login.Unlock()
	if login.term != nil && login.term.running() {
		return nil
	}
	term, err := startTerminal(home, "auth", "login")
	if err != nil {
		return err
	}
	login.term, login.sent, login.cancelled = term, false, false
	go func() {
		select {
		case <-term.exited:
		case <-time.After(10 * time.Minute):
			term.stop()
		}
	}()
	return nil
}

func sendLoginCode(code string) {
	login.Lock()
	defer login.Unlock()
	if login.term != nil && login.term.running() && code != "" {
		login.term.tty.WriteString(code + "\r")
		login.sent = true
	}
}

func cancelLogin() {
	login.Lock()
	term := login.term
	login.cancelled = true
	login.Unlock()
	if term != nil {
		term.stop()
	}
}

func loginState() loginView {
	login.Lock()
	defer login.Unlock()
	if login.term == nil {
		return loginView{}
	}
	text := login.term.text()
	if !login.term.running() {
		if login.term.err != nil && !login.cancelled {
			setNotice("Signing in didn't finish: " + lastLine(text))
		}
		login.term = nil
		refreshSetup()
		return loginView{}
	}
	v := loginView{Active: true, Sent: login.sent}
	for _, u := range anyURL.FindAllString(text, -1) {
		if v.URL == "" || (strings.Contains(u, "oauth") && !strings.Contains(v.URL, "oauth")) {
			v.URL = u
		}
	}
	return v
}

// enableRemoteControl answers yes to the question `claude remote-control`
// asks once per machine, in the folder whose server ran into it.
func enableRemoteControl() error {
	setup.Lock()
	project := setup.rcNeeded
	setup.Unlock()
	if project == "" {
		return errors.New("no server has asked for it")
	}
	term, err := startTerminal(filepath.Join(projectsDir, project), serverArgs(project)...)
	if err != nil {
		return err
	}
	defer term.stop()
	// If it runs for a while without asking, the question was answered some
	// other way and there is nothing left to do.
	asked, deadline := false, time.After(15*time.Second)
	for waiting := true; waiting; {
		select {
		case <-term.exited:
			return errors.New(lastLine(term.text()))
		case <-deadline:
			waiting = false
		case <-time.After(300 * time.Millisecond):
			asked = strings.Contains(term.text(), "Enable Remote Control?")
			waiting = !asked
		}
	}
	if asked {
		term.tty.WriteString("y\r")
		select { // give it a moment to save the answer
		case <-term.exited:
		case <-time.After(5 * time.Second):
		}
	}
	setup.Lock()
	setup.rcNeeded = ""
	setup.Unlock()
	if s := switchOff(project); s != nil {
		<-s.ended
	}
	switchOn(project)
	return nil
}
