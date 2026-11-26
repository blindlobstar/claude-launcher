package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

type projectRow struct {
	Name, Path string // Path is Name escaped for a URL
	State      string // off, starting, on, failed
	Since      string
	URL        string // where its server says to open it
	Reason     string // why it failed, in a few words
	Untrusted  bool
}

func listProjects() []string {
	entries, _ := os.ReadDir(projectsDir)
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names
}

func rowFor(name string) projectRow {
	row := projectRow{Name: name, Path: url.PathEscape(name), State: "off"}
	if s := serverFor(name); s != nil {
		row.State, row.Since, row.URL, row.Reason, row.Untrusted = s.status()
	}
	return row
}

func createProject(name string) error {
	if !nameRE.MatchString(name) {
		return errors.New("a project name is lowercase letters, digits and dashes, 40 at most")
	}
	dir := filepath.Join(projectsDir, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	return trust(dir)
}

// trust marks a folder as trusted the way the Claude Code docs describe doing
// it by hand, so `claude remote-control` starts there without its prompt.
func trust(dir string) error {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".claude.json")
	for range 5 {
		cfg := map[string]any{}
		mode := fs.FileMode(0o600)
		before, statErr := os.Stat(path)
		if statErr == nil {
			mode = before.Mode().Perm()
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.UseNumber() // keeps every number exactly as written
			if err := dec.Decode(&cfg); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return statErr
		}
		projects, ok := cfg["projects"].(map[string]any)
		if !ok {
			if cfg["projects"] != nil {
				return fmt.Errorf("%s: \"projects\" is not an object", path)
			}
			projects = map[string]any{}
			cfg["projects"] = projects
		}
		entry, ok := projects[dir].(map[string]any)
		if !ok {
			entry = map[string]any{}
			projects[dir] = entry
		}
		if entry["hasTrustDialogAccepted"] == true {
			return nil
		}
		entry["hasTrustDialogAccepted"] = true

		tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.launcher-*")
		if err != nil {
			return err
		}
		enc := json.NewEncoder(tmp)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		err = enc.Encode(cfg)
		if err == nil {
			err = tmp.Chmod(mode)
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		// Claude Code rewrites this file too: replace it only if it is still
		// the version that was read, otherwise read it again
		after, afterErr := os.Stat(path)
		same := statErr != nil && afterErr != nil
		if statErr == nil && afterErr == nil {
			same = after.ModTime().Equal(before.ModTime()) && after.Size() == before.Size()
		}
		if err == nil && same {
			return os.Rename(tmp.Name(), path)
		}
		os.Remove(tmp.Name())
		if err != nil {
			return err
		}
	}
	return errors.New("~/.claude.json kept changing, try again")
}
