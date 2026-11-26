// claude-launcher: a web page that creates project folders and keeps one
// `claude remote-control` server running per project. It runs as whoever
// starts it and is set up through the environment (see README).
package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed page.html htmx.min.js
var assets embed.FS

var (
	home, _     = os.UserHomeDir()
	addr        = env("LAUNCHER_ADDR", "127.0.0.1:8787")
	password    = os.Getenv("LAUNCHER_PASSWORD")
	projectsDir = "" // the folder it is started in: one subfolder per project
	permMode    = env("LAUNCHER_PERMISSION_MODE", "default")
	page        = template.Must(template.ParseFS(assets, "page.html"))
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	os.Unsetenv("LAUNCHER_PASSWORD") // Claude inherits the rest of the environment, not this
	os.Setenv("PATH", filepath.Join(home, ".local", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	if password == "" && !loopback(addr) {
		log.Fatal("set LAUNCHER_PASSWORD to listen on " + addr + ": whoever opens this page can sign Claude in to an account of their own")
	}
	var err error
	if projectsDir, err = os.Getwd(); err != nil {
		log.Fatal(err)
	}
	if projectsDir == "/" {
		log.Fatal("start claude-launcher in the folder that holds the projects, not in /")
	}

	mux := http.NewServeMux()
	for _, part := range []string{"alert", "projects", "system"} {
		mux.HandleFunc("GET /"+part, func(w http.ResponseWriter, r *http.Request) { render(w, part, buildView()) })
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { render(w, "page", buildView()) })
	mux.HandleFunc("GET /htmx.min.js", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "htmx.min.js")
	})
	mux.HandleFunc("POST /projects", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(r.FormValue("name"))
		if err := createProject(name); err != nil {
			setNotice(err.Error())
		} else {
			switchOn(name)
		}
		done(w, true)
	})
	mux.HandleFunc("POST /projects/{name}/{action}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		switch action := r.PathValue("action"); {
		case !slices.Contains(listProjects(), name):
			setNotice("no such project: " + name)
		case action == "on":
			switchOn(name)
		case action == "off":
			switchOff(name)
		case action == "trust":
			if err := trust(filepath.Join(projectsDir, name)); err != nil {
				setNotice(err.Error())
			} else {
				switchOff(name)
				switchOn(name)
			}
		}
		done(w, true)
	})
	mux.HandleFunc("POST /setup/{action}", func(w http.ResponseWriter, r *http.Request) {
		runSetup(r.PathValue("action"))
		done(w, true)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		if err := startLogin(); err != nil {
			setNotice(err.Error())
		}
		w.Header().Set("HX-Trigger", "refresh")
		render(w, "alert", buildView())
	})
	mux.HandleFunc("POST /login/code", func(w http.ResponseWriter, r *http.Request) {
		sendLoginCode(strings.TrimSpace(r.FormValue("code")))
		done(w, false)
	})
	mux.HandleFunc("POST /login/cancel", func(w http.ResponseWriter, r *http.Request) {
		cancelLogin()
		w.Header().Set("HX-Trigger", "refresh")
		render(w, "alert", buildView())
	})
	// the two parts of the sign-in box that change while it is open
	mux.HandleFunc("GET /login/link", func(w http.ResponseWriter, r *http.Request) {
		login := loginState()
		if login.URL != "" || !login.Active {
			w.WriteHeader(286) // htmx stops asking
		}
		render(w, "login-link", login)
	})
	mux.HandleFunc("GET /login/status", func(w http.ResponseWriter, r *http.Request) {
		if login := loginState(); login.Active {
			render(w, "login-status", login)
			return
		}
		w.Header().Set("HX-Retarget", "#alert") // finished: the box gives way to the alert area
		w.Header().Set("HX-Reswap", "innerHTML")
		w.Header().Set("HX-Trigger", "refresh")
		render(w, "alert", buildView())
	})

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP) // SIGHUP: its terminal or SSH session closed
		<-sig
		stopAll()
		os.Exit(0)
	}()
	log.Printf("listening on %s, projects in %s", addr, projectsDir)
	srv := &http.Server{Addr: addr, Handler: guard(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, name, data); err != nil {
		log.Print(err)
	}
}

// done answers a button: the lists reload, and the alert area shows the new
// state unless the sign-in box is open there (its code field must survive).
func done(w http.ResponseWriter, showAlert bool) {
	w.Header().Set("HX-Trigger", "refresh")
	if !showAlert || loginState().Active {
		w.Header().Set("HX-Reswap", "none")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	render(w, "alert", buildView())
}

func guard(next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if password != "" {
			_, given, _ := r.BasicAuth()
			got := sha256.Sum256([]byte(given))
			if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="claude-launcher"`)
				http.Error(w, "password required", http.StatusUnauthorized)
				return
			}
		} else if !loopback(r.Host) {
			// without a password the page answers only to its loopback name,
			// so a website can't reach it by pointing its own name at 127.0.0.1
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			// htmx sends this header and another site can't make a browser send it
			if r.Header.Get("HX-Request") != "true" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			setNotice("")
		}
		next.ServeHTTP(w, r)
	})
}

func loopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

var notice struct {
	sync.Mutex
	text  string
	until time.Time
}

func setNotice(text string) {
	notice.Lock()
	defer notice.Unlock()
	notice.text, notice.until = text, time.Now().Add(20*time.Second)
}

func currentNotice() string {
	notice.Lock()
	defer notice.Unlock()
	if time.Now().After(notice.until) {
		return ""
	}
	return notice.text
}

var (
	ansiMove    = regexp.MustCompile(`\x1b\[\d*;?\d*[Hf]`)
	ansiForward = regexp.MustCompile(`\x1b\[(\d*)C`)
	ansiOther   = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b[()][A-Z0-9]|\x1b[=>78]`)
)

// plain turns terminal output into text.
func plain(raw []byte) string {
	s := strings.ToValidUTF8(string(raw), "")
	s = ansiMove.ReplaceAllString(s, "\n")
	s = ansiForward.ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.Atoi(m[2 : len(m)-1])
		return strings.Repeat(" ", max(n, 1))
	})
	return ansiOther.ReplaceAllString(s, "")
}

func lastLine(text string) string {
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
