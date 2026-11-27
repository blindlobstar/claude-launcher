package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTrustKeepsTheRestOfTheFile(t *testing.T) {
	home = t.TempDir()
	dir := filepath.Join(home, "projects", "demo")
	os.MkdirAll(dir, 0o755)
	dir, _ = filepath.EvalSymlinks(dir)
	path := filepath.Join(home, ".claude.json")
	original := `{"numStartups": 12345678901234567890, "ratio": 0.10, "tip": "a <b> & c", "projects": {"/other": {"hasTrustDialogAccepted": false, "history": [1, 2]}}}`
	os.WriteFile(path, []byte(original), 0o640)

	if err := trust(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	for _, want := range []string{
		`"numStartups": 12345678901234567890`,
		`"ratio": 0.10`,
		`"tip": "a <b> & c"`,
		`"/other": {`,
		`"hasTrustDialogAccepted": false`,
		`"` + dir + `": {` + "\n      " + `"hasTrustDialogAccepted": true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode changed to %v", info.Mode().Perm())
	}
	if err := trust(dir); err != nil { // a second time changes nothing
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); string(again) != got {
		t.Error("trusting twice rewrote the file")
	}
}

func TestTrustWithoutAFile(t *testing.T) {
	home = t.TempDir()
	if err := trust(home); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	if !strings.Contains(string(data), `"hasTrustDialogAccepted": true`) {
		t.Errorf("got %s", data)
	}
}

func TestProjectNames(t *testing.T) {
	for name, ok := range map[string]bool{"demo": true, "a-1": true, "": false, "-a": false, "A": false, "a/b": false, "..": false, "a b": false} {
		if nameRE.MatchString(name) != ok {
			t.Errorf("%q: want valid=%v", name, ok)
		}
	}
}

func TestDeleteProject(t *testing.T) {
	home, projectsDir = t.TempDir(), t.TempDir()
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "calls")
	// a stand-in claude that notes how it was called and fails with the words it is given
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n[ -z \"$PURGE_FAILS\" ] || { echo \"$PURGE_FAILS\" >&2; exit 1; }\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := filepath.Join(projectsDir, "demo")
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "file"), []byte("x"), 0o644)

	t.Setenv("PURGE_FAILS", "no can do")
	if err := deleteProject("demo"); err == nil || err.Error() != "claude purge: no can do" {
		t.Errorf("a failed purge should stop the delete and say why, got %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("the folder went although the purge failed")
	}
	// what the real one says about a folder Claude never worked in
	t.Setenv("PURGE_FAILS", "No Claude Code project data found for "+dir+" under "+home+".")
	if err := deleteProject("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the folder is still there: %v", err)
	}
	if calls, _ := os.ReadFile(log); string(calls) != strings.Repeat("purge "+dir+" --yes\n", 2) {
		t.Errorf("claude was called as:\n%s", calls)
	}
}

func TestServerArgs(t *testing.T) {
	permMode = "auto"
	if got := strings.Join(serverArgs("demo"), " "); got != "remote-control --no-create-session-in-dir --remote-control-session-name-prefix demo --permission-mode auto" {
		t.Errorf("got %s", got)
	}
}

func TestExtractGh(t *testing.T) {
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "gh_1_linux_amd64/LICENSE", Size: 1, Mode: 0o644})
	tw.Write([]byte("x"))
	tw.WriteHeader(&tar.Header{Name: "gh_1_linux_amd64/bin/gh", Size: 5, Mode: 0o755})
	tw.Write([]byte("linux"))
	tw.Close()
	gz.Close()

	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	f, _ := zw.Create("gh_1_macOS_arm64/bin/gh")
	f.Write([]byte("mac"))
	zw.Close()

	for archive, c := range map[string]struct {
		data []byte
		want string
	}{"gh_1_linux_amd64.tar.gz": {tgz.Bytes(), "linux"}, "gh_1_macOS_arm64.zip": {zipped.Bytes(), "mac"}} {
		got, err := extractGh(archive, c.data)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: got %q, %v", archive, got, err)
		}
	}
}

func TestPlain(t *testing.T) {
	got := plain([]byte("\x1b[1mBrowser\x1b[2Cdidn't open\x1b[0m\x1b]0;title\x07"))
	if got != "Browser  didn't open" {
		t.Errorf("got %q", got)
	}
}

func TestHostNames(t *testing.T) {
	for in, want := range map[string]string{"Claude.Monster": "claude.monster", "claude.local:8787": "claude.local", "[::1]:80": "::1", "192.168.1.14:8787": "192.168.1.14"} {
		if got := hostName(in); got != want {
			t.Errorf("hostName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]bool{"localhost:8787": true, "127.0.0.1": true, "[::1]:8787": true, "claude.local": false, ":8787": false, "0.0.0.0:8787": false} {
		if loopback(in) != want {
			t.Errorf("loopback(%q) != %v", in, want)
		}
	}
}

func TestOlder(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"2.88.1", "2.106.0", true}, {"2.106.0", "2.88.1", false}, {"2.1.332", "2.1.332", false}, {"2.1", "2.1.1", true}} {
		if older(c.a, c.b) != c.want {
			t.Errorf("older(%s, %s) != %v", c.a, c.b, c.want)
		}
	}
}

func TestGithubUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"login": "someone"}`))
	}))
	defer srv.Close()
	githubAPI = srv.URL
	for token, want := range map[string][2]string{
		"good": {"someone", ""}, "bad": {"", "GitHub rejected the token"}, "": {"", "no token where the launcher runs"},
	} {
		t.Setenv("GH_TOKEN", token)
		if login, note := githubUser(); login != want[0] || note != want[1] {
			t.Errorf("token %q: got %q, %q", token, login, note)
		}
	}
}

func TestAlertAndButtons(t *testing.T) {
	ok := system{Claude: program{Name: "claude", Version: "2.1.332", Latest: "2.1.332"}, SignedIn: true, Expires: time.Now().Add(9 * 24 * time.Hour)}
	if a := alertFor(ok); a != nil || ok.Blocked() {
		t.Errorf("healthy system: alert %+v, blocked %v", a, ok.Blocked())
	}
	soon := ok
	soon.Expires = time.Now().Add(50 * time.Hour)
	if a := alertFor(soon); a == nil || a.Kind != "warn" || a.Title != "Claude sign-in ends in 2 days" || soon.Blocked() {
		t.Errorf("sign-in ending: %+v", a)
	}
	out := ok
	out.SignedIn = false
	if a := alertFor(out); a == nil || a.Post != "/login" || !out.Blocked() {
		t.Errorf("signed out: %+v", a)
	}
	missing := system{Claude: program{Name: "claude"}}
	if a := alertFor(missing); a == nil || a.Label != "Install" {
		t.Errorf("not installed: %+v", a)
	}

	for _, c := range []struct {
		p       program
		managed bool
		label   string
	}{
		{program{Name: "gh"}, true, "Install"},
		{program{Name: "gh", Version: "2.88.1", Latest: "2.106.0"}, true, "Update"},
		{program{Name: "gh", Version: "2.106.0", Latest: "2.106.0"}, true, ""},
		{program{Name: "gh", Version: "2.106.0"}, true, ""},
		{program{Name: "claude"}, true, ""}, // the alert has its Install button
		{program{Name: "git"}, false, ""},
	} {
		if got := programLine(c.p, c.managed).Label; got != c.label {
			t.Errorf("%+v: button %q, want %q", c.p, got, c.label)
		}
	}
}

func TestReviewFixes(t *testing.T) {
	for _, c := range [][3]string{
		{"/Users/boris/projects", "/Users/boris", "~/projects"},
		{"/Users/boris", "/Users/boris", "~"},
		{"/Users/boris-work/projects", "/Users/boris", "/Users/boris-work/projects"},
		{"/srv/projects", "/", "/srv/projects"},
		{"/srv/projects", "", "/srv/projects"},
	} {
		if got := shortDir(c[0], c[1]); got != c[2] {
			t.Errorf("shortDir(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
	if l := programLine(program{Name: "gh", Note: "checksum does not match"}, true); l.Sub != "last try failed: checksum does not match" || l.Label != "Install" {
		t.Errorf("a failed gh install should say why: %+v", l)
	}
	off := system{Claude: program{Name: "claude", Version: "2.1.332"}, SignedIn: true, RemoteControlOff: true, EnableNote: "boom"}
	if a := alertFor(off); a == nil || a.Detail != "enabling failed: boom" || a.Label != "Enable" {
		t.Errorf("a failed Enable should say why: %+v", a)
	}

	s := &server{name: "x"}
	s.Write([]byte("first line\nContinue at https://claude.ai/code/" + strings.Repeat("é", 400)))
	s.flush()
	if len(s.lines) != 2 || !strings.HasSuffix(s.url, strings.Repeat("é", 400)) || !utf8.ValidString(s.lines[1]) || len([]rune(s.lines[1])) != 300 {
		t.Errorf("last line without a line break: %d lines, url %d bytes, line %d runes", len(s.lines), len(s.url), len([]rune(s.lines[len(s.lines)-1])))
	}
}
