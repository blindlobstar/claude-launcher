package main

import (
	"fmt"
	"os"
	"strings"
)

// What the page shows, worked out from the state of the machine and servers.
type view struct {
	Host, Dir          string
	Notice             string // an action that just failed
	Blocked            bool   // no folder can be switched on: the list is paused
	Alert              *alert
	Projects           []projectRow
	On                 int
	Programs, Accounts []line
	Login              loginView
}

// The one thing that needs attention, shown as a card under the header with
// the button that fixes it.
type alert struct {
	Kind   string // warn or err
	Title  string
	Detail string
	Post   string // the request its button makes
	Label  string
	Busy   bool // being fixed right now: a spinner instead of a button
}

// One row of Programs or Accounts.
type line struct {
	Class       string // ok, warn, err, busy or ""
	Spin        bool
	Hollow      bool
	Name        string
	Text, Dim   string
	Right       string // dim text where the button would be
	Post, Label string // the button
	Quiet       bool
	Sub         string
}

func buildView() view {
	login := loginState()
	sys := currentSystem()
	sys.SigningIn = login.Active
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	v := view{
		Host: host, Dir: shortDir(projectsDir, home), Notice: currentNotice(), Blocked: sys.Blocked(), Alert: alertFor(sys), Login: login,
		Programs: []line{programLine(sys.Claude, true), programLine(sys.Gh, true), programLine(sys.Git, false)},
		Accounts: accountLines(sys),
	}
	for _, name := range listProjects() {
		row := rowFor(name)
		if row.State == "on" {
			v.On++
		}
		v.Projects = append(v.Projects, row)
	}
	return v
}

// shortDir writes a folder inside the home folder with a ~.
func shortDir(dir, home string) string {
	if home != "" && home != "/" && (dir == home || strings.HasPrefix(dir, home+"/")) {
		return "~" + dir[len(home):]
	}
	return dir
}

func alertFor(s system) *alert {
	const cant = "folders can't be switched on"
	switch {
	case s.Claude.Version == "" && s.Claude.Busy != "":
		return &alert{Kind: "err", Title: "Installing Claude Code…", Busy: true}
	case s.Claude.Version == "":
		a := &alert{Kind: "err", Title: "Claude Code isn't installed", Detail: cant, Post: "/setup/claude", Label: "Install"}
		if s.Claude.Note != "" {
			a.Detail = "installing failed: " + s.Claude.Note
		}
		return a
	case s.SigningIn:
		return nil // the sign-in box takes the alert's place
	case !s.SignedIn:
		return &alert{Kind: "err", Title: "Claude isn't signed in", Detail: cant, Post: "/login", Label: "Log in"}
	case s.RemoteControlOff && s.Enabling:
		return &alert{Kind: "err", Title: "Enabling Remote Control…", Busy: true}
	case s.RemoteControlOff:
		a := &alert{Kind: "err", Title: "Remote Control isn't enabled here", Detail: cant, Post: "/setup/remote-control", Label: "Enable"}
		if s.EnableNote != "" {
			a.Detail = "enabling failed: " + s.EnableNote
		}
		return a
	}
	if days := s.EndsSoon(); days >= 0 {
		when := fmt.Sprintf("in %d days", days)
		switch days {
		case 0:
			when = "today"
		case 1:
			when = "tomorrow"
		}
		return &alert{Kind: "warn", Title: "Claude sign-in ends " + when, Detail: "then the folders go offline", Post: "/login", Label: "Log in again"}
	}
	return nil
}

// programLine shows a button only when there is something to do: Install when
// the program is missing, Update when a newer version exists. managed is false
// for programs the launcher can't install.
func programLine(p program, managed bool) line {
	l := line{Class: "ok", Name: p.Name, Text: p.Version}
	switch {
	case p.Busy != "":
		l.Class, l.Spin, l.Right = "busy", true, p.Busy
	case p.Version == "":
		l.Class, l.Hollow, l.Dim = "", true, "not installed"
		switch {
		case !managed:
			l.Sub = "comes from the system's package manager"
		case p.Name == "claude":
			l.Class = "err" // its Install button is in the alert
		default:
			l.Post, l.Label = "/setup/"+p.Name, "Install"
		}
	case !managed:
		l.Right = "system package"
	case p.Outdated():
		l.Dim, l.Post, l.Label = "→ "+p.Latest, "/setup/"+p.Name, "Update"
		if p.Name == "claude" {
			l.Sub = "updating restarts the folders that are on"
		}
	case p.Latest != "":
		l.Right = "up to date"
	}
	if p.Note != "" && p.Busy == "" && l.Class != "err" { // a missing claude reports it in the alert
		l.Sub = "last try failed: " + p.Note
	}
	return l
}

func accountLines(s system) []line {
	claude := line{Name: "Claude"}
	switch {
	case s.Claude.Version == "":
		claude.Hollow, claude.Dim = true, "needs Claude Code"
	case s.SigningIn:
		claude.Class, claude.Spin, claude.Text = "busy", true, "signing in…"
	case !s.SignedIn:
		claude.Class, claude.Hollow, claude.Dim = "err", true, "not signed in"
	default:
		claude.Class, claude.Text, claude.Sub = "ok", "signed in", s.Plan
		if !s.Expires.IsZero() {
			claude.Sub = strings.TrimPrefix(claude.Sub+" · until "+s.Expires.Local().Format("2 Jan"), " · ")
		}
		if s.EndsSoon() >= 0 {
			claude.Class = "warn" // its button is in the alert
		} else {
			claude.Post, claude.Label, claude.Quiet = "/login", "Log in again", true
		}
	}
	github := line{Class: "ok", Name: "GitHub", Text: "connected", Sub: "as " + s.GitHub}
	if s.GitHub == "" {
		github = line{Hollow: true, Name: "GitHub", Dim: "not connected", Sub: s.GitHubNote}
		if s.GitHubNote == "" {
			github.Dim = "checking…"
		}
	}
	return []line{claude, github}
}
