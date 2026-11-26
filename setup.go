package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// What Claude needs on this machine: the programs (installed and updated for
// the current user, no root), the Claude sign-in and the GitHub token.
type program struct {
	Name    string
	Version string // "" when it isn't installed
	Latest  string // "" when not known
	Busy    string // "Installing…" or "Updating…" while that runs
	Note    string // why the last install or update failed
}

func (p program) Outdated() bool {
	return p.Version != "" && p.Latest != "" && older(p.Version, p.Latest)
}

type system struct {
	Claude, Gh, Git  program
	SignedIn         bool
	SigningIn        bool
	Plan             string
	Expires          time.Time // when the sign-in ends, zero if unknown
	GitHub           string    // the token's user, "" when not connected
	GitHubNote       string    // then, why
	RemoteControlOff bool      // a server asked the one-time "Enable Remote Control?" question
	Enabling         bool
	EnableNote       string // why enabling it failed
}

// Blocked means no project can run.
func (s system) Blocked() bool { return s.Claude.Version == "" || !s.SignedIn || s.RemoteControlOff }

// EndsSoon is the number of days until the sign-in ends when that is three or
// fewer, otherwise -1.
func (s system) EndsSoon() int {
	if !s.SignedIn || s.Expires.IsZero() {
		return -1
	}
	days := int(time.Until(s.Expires).Hours() / 24)
	if days > 3 {
		return -1
	}
	return max(days, 0)
}

var setup struct {
	sync.Mutex
	sys      system
	at       time.Time
	busy     map[string]string // action -> what it is doing
	note     map[string]string // action -> why its last run failed
	rcNeeded string            // a project whose server asked the Remote Control question
}

// Facts from the internet, fetched in the background about once an hour.
var remote struct {
	sync.Mutex
	at                 time.Time
	fetching           bool
	claude, gh         string // latest versions
	github, githubNote string
}

var (
	httpClient    = &http.Client{Timeout: 3 * time.Minute}
	githubAPI     = "https://api.github.com"
	claudeLatest  = "https://downloads.claude.ai/claude-code-releases/latest" // what the official installer reads
	versionFormat = regexp.MustCompile(`^\d+(\.\d+)+$`)
)

func remoteControlNeeded(project string) {
	setup.Lock()
	defer setup.Unlock()
	setup.rcNeeded, setup.at = project, time.Time{}
}

// remoteControlWorks is called when a server has stayed up: the question
// has been answered, here or some other way.
func remoteControlWorks() {
	setup.Lock()
	defer setup.Unlock()
	if setup.rcNeeded != "" {
		setup.rcNeeded, setup.at = "", time.Time{}
	}
}

func refreshSetup() {
	setup.Lock()
	defer setup.Unlock()
	setup.at = time.Time{}
}

func currentSystem() system {
	setup.Lock()
	defer setup.Unlock()
	if time.Since(setup.at) < time.Minute {
		return setup.sys
	}
	sys := system{Claude: program{Name: "claude"}, Gh: program{Name: "gh"}, Git: program{Name: "git"}}
	sys.Claude.Version = toolVersion(0, "claude", "--version")
	sys.Gh.Version = toolVersion(2, "gh", "--version")
	sys.Git.Version = toolVersion(2, "git", "--version")
	if sys.Claude.Version != "" {
		var status struct {
			LoggedIn                                     bool
			AuthMethod, SubscriptionType, LoginExpiresAt string
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, _ := exec.CommandContext(ctx, "claude", "auth", "status", "--json").Output()
		cancel()
		if json.Unmarshal(out, &status) == nil {
			// Remote Control only works with a claude.ai sign-in
			sys.SignedIn = status.LoggedIn && status.AuthMethod == "claude.ai"
			sys.Expires, _ = time.Parse(time.RFC3339, status.LoginExpiresAt)
			if t := status.SubscriptionType; t != "" {
				sys.Plan = strings.ToUpper(t[:1]) + t[1:] + " plan"
			}
		}
	}
	sys.Claude.Busy, sys.Claude.Note = setup.busy["claude"], setup.note["claude"]
	sys.Gh.Busy, sys.Gh.Note = setup.busy["gh"], setup.note["gh"]
	sys.RemoteControlOff, sys.Enabling = setup.rcNeeded != "", setup.busy["remote-control"] != ""
	sys.EnableNote = setup.note["remote-control"]

	remote.Lock()
	sys.Claude.Latest, sys.Gh.Latest, sys.GitHub, sys.GitHubNote = remote.claude, remote.gh, remote.github, remote.githubNote
	stale := time.Since(remote.at) > time.Hour && !remote.fetching
	remote.fetching = remote.fetching || stale
	remote.Unlock()
	if stale {
		go fetchRemote()
	}
	setup.sys, setup.at = sys, time.Now()
	return sys
}

func fetchRemote() {
	next := time.Now()
	claude, gh := "", ""
	if body, err := download(claudeLatest); err == nil && versionFormat.Match(bytes.TrimSpace(body)) {
		claude = string(bytes.TrimSpace(body))
	}
	if release, err := ghRelease(); err == nil {
		gh = release.version()
	}
	if claude == "" || gh == "" {
		next = next.Add(-50 * time.Minute) // try again in ten minutes
	}
	github, note := githubUser()
	remote.Lock()
	remote.at, remote.fetching, remote.claude, remote.gh, remote.github, remote.githubNote = next, false, claude, gh, github, note
	remote.Unlock()
	refreshSetup()
}

// githubUser returns the user the GH_TOKEN belongs to, or why there is none.
func githubUser() (login, note string) {
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		return "", "no token where the launcher runs"
	}
	req, _ := http.NewRequest("GET", githubAPI+"/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "couldn't reach GitHub"
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", "GitHub rejected the token"
	}
	var user struct{ Login string }
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&user) != nil || user.Login == "" {
		return "", "GitHub answered " + resp.Status
	}
	return user.Login, ""
}

func run(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// toolVersion returns the given word of the first line a version command
// prints, or "" when the program isn't there.
func toolVersion(word int, name string, args ...string) string {
	out, err := run(15*time.Second, name, args...)
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(out), "\n")
	if fields := strings.Fields(first); len(fields) > word {
		return fields[word]
	}
	return strings.TrimSpace(first)
}

// older reports whether version a comes before version b.
func older(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func runSetup(action string) {
	missing := func(name string) bool { _, err := exec.LookPath(name); return err != nil }
	var do func() error
	doing := "Updating…"
	switch action {
	case "claude":
		do = updateClaude
		if missing("claude") {
			do, doing = installClaude, "Installing…"
		}
	case "gh":
		do = installGh
		if missing("gh") {
			doing = "Installing…"
		}
	case "remote-control":
		do, doing = enableRemoteControl, "Enabling…"
	default:
		return
	}
	setup.Lock()
	defer setup.Unlock()
	if setup.busy[action] != "" {
		return
	}
	if setup.busy == nil {
		setup.busy, setup.note = map[string]string{}, map[string]string{}
	}
	setup.busy[action], setup.at = doing, time.Time{}
	go func() {
		note := ""
		if err := do(); err != nil {
			note = err.Error()
		}
		setup.Lock()
		defer setup.Unlock()
		setup.busy[action], setup.note[action], setup.at = "", note, time.Time{}
	}()
}

// installClaude runs the official installer, which installs for the current
// user into ~/.local/bin.
func installClaude() error {
	resp, err := httpClient.Get("https://claude.ai/install.sh")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("claude.ai/install.sh: " + resp.Status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-s")
	cmd.Stdin = resp.Body
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, lastLine(plain(out)))
	}
	return nil
}

// updateClaude applies an update; running servers only pick it up when they
// restart, so they are restarted.
func updateClaude() error {
	if out, err := run(10*time.Minute, "claude", "update"); err != nil {
		return fmt.Errorf("%v: %s", err, lastLine(plain(out)))
	}
	restartAll()
	return nil
}

type release struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r release) version() string { return strings.TrimPrefix(r.Tag, "v") }

func ghRelease() (release, error) {
	var r release
	body, err := download(githubAPI + "/repos/cli/cli/releases/latest")
	if err == nil {
		err = json.Unmarshal(body, &r)
	}
	return r, err
}

// installGh downloads the latest gh release for this machine, checks it
// against the release's checksum file and puts it in ~/.local/bin.
func installGh() error {
	rel, err := ghRelease()
	if err != nil {
		return err
	}
	version := rel.version()
	archive := fmt.Sprintf("gh_%s_linux_%s.tar.gz", version, runtime.GOARCH)
	if runtime.GOOS == "darwin" {
		archive = fmt.Sprintf("gh_%s_macOS_%s.zip", version, runtime.GOARCH)
	}
	urls := map[string]string{}
	for _, a := range rel.Assets {
		urls[a.Name] = a.URL
	}
	sumsName := fmt.Sprintf("gh_%s_checksums.txt", version)
	if urls[archive] == "" || urls[sumsName] == "" {
		return errors.New("no gh release file named " + archive)
	}
	sums, err := download(urls[sumsName])
	if err != nil {
		return err
	}
	data, err := download(urls[archive])
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if !strings.Contains(string(sums), hex.EncodeToString(sum[:])+"  "+archive) {
		return errors.New("checksum of " + archive + " does not match")
	}
	binary, err := extractGh(archive, data)
	if err != nil {
		return err
	}
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(bin, ".gh.new")
	if err := os.WriteFile(tmp, binary, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(bin, "gh")); err != nil {
		return err
	}
	// makes plain git use gh, and so GH_TOKEN, for github.com
	if out, err := run(time.Minute, "gh", "auth", "setup-git", "--hostname", "github.com", "--force"); err != nil {
		return fmt.Errorf("installed, but gh auth setup-git failed: %s", lastLine(string(out)))
	}
	return nil
}

func download(url string) ([]byte, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(url + ": " + resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

// extractGh returns the bin/gh file from a release archive (.tar.gz or .zip).
func extractGh(archive string, data []byte) ([]byte, error) {
	if strings.HasSuffix(archive, ".zip") {
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range z.File {
			if strings.HasSuffix(f.Name, "/bin/gh") {
				r, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer r.Close()
				return io.ReadAll(r)
			}
		}
		return nil, errors.New("no bin/gh in " + archive)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	for t := tar.NewReader(gz); ; {
		h, err := t.Next()
		if err != nil {
			return nil, errors.New("no bin/gh in " + archive)
		}
		if strings.HasSuffix(h.Name, "/bin/gh") {
			return io.ReadAll(t)
		}
	}
}
