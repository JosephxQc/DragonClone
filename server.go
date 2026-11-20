package main

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dragonclone/core"
)

//go:embed ui
var uiFiles embed.FS

// The backend is a small HTTP server on 127.0.0.1. The window talks to it
// with a random token in every request, and we check the Host header so a
// web page can't reach us through DNS tricks.

type server struct {
	token    string
	host     string
	jobs     *jobManager
	logger   *fileLogger
	lastSeen atomic.Int64
	byeAt    atomic.Int64
	done     chan struct{}
	once     sync.Once
}

// handoff is written to backend.json so the launcher (and a second launch
// of the app) can find a running backend.
type handoff struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func handoffPath() string { return filepath.Join(core.DataDir(), "backend.json") }

func randomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func runBackend(onReady func(url string)) error {
	os.MkdirAll(core.DataDir(), 0o755)
	logger := openLogger()
	defer logger.Close()
	logger.Printf("DragonClone %s backend starting (%s/%s, admin=%v)", version, runtime.GOOS, runtime.GOARCH, core.IsAdmin())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		logger.Printf("listen: %v", err)
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	s := &server{token: randomToken(), host: fmt.Sprintf("127.0.0.1:%d", port),
		jobs: &jobManager{logger: logger}, logger: logger, done: make(chan struct{})}
	s.lastSeen.Store(time.Now().UnixNano())

	b, _ := json.Marshal(handoff{Port: port, Token: s.token, PID: os.Getpid()})
	if err := os.WriteFile(handoffPath(), b, 0o600); err != nil {
		logger.Printf("writing %s: %v", handoffPath(), err)
		return err
	}
	defer os.Remove(handoffPath())

	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(logger, "http: ", 0)}
	go srv.Serve(ln)
	go s.watchdog()
	url := fmt.Sprintf("http://%s/?t=%s", s.host, s.token)
	logger.Printf("listening on %s", s.host)
	if onReady != nil {
		onReady(url)
	}
	<-s.done
	logger.Printf("backend exiting")
	srv.Close()
	return nil
}

// watchdog shuts us down once the window has been gone for a while and
// nothing is running. A running job keeps going even with the window closed;
// reopening the app reconnects to it.
func (s *server) watchdog() {
	for range time.Tick(2 * time.Second) {
		if s.jobs.busy() {
			continue
		}
		idle := time.Since(time.Unix(0, s.lastSeen.Load()))
		bye := s.byeAt.Load()
		if idle > 60*time.Second || (bye > 0 && time.Since(time.Unix(0, bye)) > 6*time.Second && idle > 5*time.Second) {
			s.quit()
			return
		}
	}
}

func (s *server) quit() { s.once.Do(func() { close(s.done) }) }

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(uiFiles, "ui")
	files := http.FileServer(http.FS(static))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	}))

	api := map[string]func(*http.Request) (any, error){
		"GET /api/info":             s.info,
		"GET /api/disks":            s.disks,
		"GET /api/roots":            func(*http.Request) (any, error) { return core.Roots(), nil },
		"GET /api/ls":               s.list,
		"POST /api/inspect":         s.inspect,
		"POST /api/plan":            s.plan,
		"POST /api/start":           s.start,
		"POST /api/cancel":          func(*http.Request) (any, error) { s.jobs.cancelJob(); return "ok", nil },
		"GET /api/job":              s.job,
		"GET /api/history":          func(*http.Request) (any, error) { return readHistory(), nil },
		"POST /api/mount":           s.mount,
		"POST /api/unmount":         s.unmount,
		"POST /api/show":            s.show,
		"POST /api/cancel-shutdown": func(*http.Request) (any, error) { return "ok", core.CancelShutdown() },
		"POST /api/ping":            func(*http.Request) (any, error) { s.byeAt.Store(0); return "ok", nil },
		"POST /api/bye":             func(*http.Request) (any, error) { s.byeAt.Store(time.Now().UnixNano()); return "ok", nil },
		"GET /api/log":              s.logText,
	}
	for pattern, fn := range api {
		mux.Handle(pattern, s.wrap(fn))
	}
	return s.guard(mux)
}

// guard rejects anything not addressed to us by our exact host name.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.host {
			http.Error(w, "wrong host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type apiError struct {
	Error string `json:"error"`
	Hint  string `json:"hint,omitempty"`
}

func (s *server) wrap(fn func(*http.Request) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-DragonClone-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/bye" {
			s.lastSeen.Store(time.Now().UnixNano())
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		out, err := fn(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(apiError{Error: err.Error(), Hint: core.ErrorHint(err)})
			return
		}
		json.NewEncoder(w).Encode(out)
	})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func (s *server) info(*http.Request) (any, error) {
	host, _ := os.Hostname()
	return map[string]any{
		"version": version, "os": runtime.GOOS, "admin": core.IsAdmin(), "host": host,
		"dataDir": core.DataDir(), "logFile": s.logger.Path(), "cpus": runtime.NumCPU(),
	}, nil
}

func (s *server) disks(*http.Request) (any, error) {
	d, err := core.Disks()
	if d == nil {
		d = []core.DiskInfo{}
	}
	return d, err
}

type entry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Image bool   `json:"image"`
}

var imageExts = []string{".img", ".gz", ".vhd", ".iso", ".bin", ".raw", ".dd"}

func isImageName(n string) bool {
	n = strings.ToLower(n)
	for _, e := range imageExts {
		if strings.HasSuffix(n, e) {
			return true
		}
	}
	return false
}

func (s *server) list(r *http.Request) (any, error) {
	path := r.URL.Query().Get("path")
	if path == "" {
		return nil, errors.New("no folder given")
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" && len(path) == 2 && path[1] == ':' {
		path += `\`
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := struct {
		Path    string  `json:"path"`
		Parent  string  `json:"parent"`
		Entries []entry `json:"entries"`
	}{Path: path, Entries: []entry{}}
	if p := filepath.Dir(path); p != path {
		out.Parent = p
	}
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, "$") || strings.HasPrefix(name, ".") || name == "System Volume Information" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		en := entry{Name: name, Dir: e.IsDir(), Mtime: info.ModTime().Unix()}
		if !en.Dir {
			en.Size = info.Size()
			en.Image = isImageName(name)
		}
		out.Entries = append(out.Entries, en)
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out, nil
}

func (s *server) inspect(r *http.Request) (any, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return core.Resolve(req.Path, core.RoleSource, core.KindFile)
}

func (s *server) plan(r *http.Request) (any, error) {
	var req JobRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if s.jobs.busy() {
		return nil, errBusy
	}
	return makePlan(&req)
}

func (s *server) start(r *http.Request) (any, error) {
	var req JobRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	s.logger.Printf("start %s: %q -> %q (verify=%v skipBad=%v live=%v force=%v)",
		req.Kind, req.Source, req.Target, req.Verify, req.SkipBad, req.AllowLive, req.ForceDismount)
	resp, err := s.jobs.start(&req)
	if err != nil {
		s.logger.Printf("start refused: %v", err)
	}
	return resp, err
}

func (s *server) job(r *http.Request) (any, error) {
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	return s.jobs.snapshot(since), nil
}

func (s *server) pathArg(r *http.Request) (string, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil {
		return "", err
	}
	if req.Path == "" {
		return "", errors.New("no file given")
	}
	return filepath.Clean(req.Path), nil
}

func (s *server) mount(r *http.Request) (any, error) {
	p, err := s.pathArg(r)
	if err != nil {
		return nil, err
	}
	letters, err := core.MountImage(p)
	s.logger.Printf("mount %s -> %q %v", p, letters, err)
	return map[string]string{"letters": letters}, err
}

func (s *server) unmount(r *http.Request) (any, error) {
	p, err := s.pathArg(r)
	if err != nil {
		return nil, err
	}
	return "ok", core.UnmountImage(p)
}

func (s *server) show(r *http.Request) (any, error) {
	p, err := s.pathArg(r)
	if err != nil {
		return nil, err
	}
	return "ok", core.ShowInFolder(p)
}

func (s *server) logText(*http.Request) (any, error) {
	b, err := os.ReadFile(s.logger.Path())
	if len(b) > 512<<10 {
		b = b[len(b)-512<<10:]
	}
	return string(b), err
}

// ---- log file -------------------------------------------------------------------

type fileLogger struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

func openLogger() *fileLogger {
	dir := filepath.Join(core.DataDir(), "logs")
	os.MkdirAll(dir, 0o755)
	// keep the last 20 logs
	if ents, err := os.ReadDir(dir); err == nil && len(ents) > 20 {
		for _, e := range ents[:len(ents)-20] {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	p := filepath.Join(dir, "dragonclone-"+time.Now().Format("2006-01-02_15-04-05")+".log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		return &fileLogger{path: p}
	}
	return &fileLogger{f: f, path: p}
}

func (l *fileLogger) Printf(format string, args ...any) {
	fmt.Fprintf(l, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

func (l *fileLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return len(p), nil
	}
	return l.f.Write(p)
}

func (l *fileLogger) Path() string { return l.path }

func (l *fileLogger) Close() {
	if l.f != nil {
		l.f.Close()
	}
}
