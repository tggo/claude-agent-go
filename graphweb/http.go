package graphweb

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML []byte

// Handler returns the HTTP handler (UI, SSE stream and control endpoints).
// Use it to mount the UI in an existing server; Start does that for you.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/topology", s.handleTopology)
	mux.HandleFunc("GET /api/artifact/{seq}", s.handleArtifact)
	mux.HandleFunc("GET /api/review/{seq}/{n}", s.handleReviewItem)
	mux.HandleFunc("GET /api/breakpoints", s.handleGetBreakpoints)
	mux.HandleFunc("POST /api/answer", s.post(s.handleAnswer))
	mux.HandleFunc("POST /api/breakpoints", s.post(s.handleSetBreakpoints))
	mux.HandleFunc("POST /api/control", s.post(s.handleControl))
	return s.hostGuard(mux)
}

// Start listens on addr and serves in the background until ctx ends. An
// empty host binds 127.0.0.1 (":8799" means "127.0.0.1:8799"); an explicit
// non-loopback host is allowed through the Host check, since you asked for it.
// It returns the base URL.
func (s *Server) Start(ctx context.Context, addr string) (string, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("graphweb: addr %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return "", err
	}
	bound := ln.Addr().(*net.TCPAddr)
	if !bound.IP.IsLoopback() {
		s.mu.Lock()
		s.hosts[host] = true
		s.mu.Unlock()
	}
	hs := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if hs.Shutdown(sc) != nil {
			_ = hs.Close()
		}
	}()
	return "http://" + net.JoinHostPort(host, strconv.Itoa(bound.Port)), nil
}

// hostGuard rejects requests whose Host is not an allowed name: a page on
// evil.example resolving to 127.0.0.1 still sends Host: evil.example.
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Host
		if hh, _, err := net.SplitHostPort(h); err == nil {
			h = hh
		}
		h = strings.Trim(h, "[]")
		s.mu.Lock()
		ok := s.hosts[h]
		s.mu.Unlock()
		if !ok {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// post wraps a mutating handler: read-only servers refuse, the token must
// match, and a cross-origin Origin header is refused even with the token.
func (s *Server) post(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.readOnly {
			http.Error(w, "read-only replay", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Graphweb-Token")), []byte(s.token)) != 1 {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h(w, r)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	boot, _ := json.Marshal(map[string]any{"token": s.token, "readOnly": s.readOnly})
	page := bytes.Replace(indexHTML, []byte("/*GRAPHWEB_BOOT*/null"), boot, 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(page)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handleEvents streams the journal as SSE. Event ids are journal sequence
// numbers: a reconnect (Last-Event-ID header, or ?from=<seq>) continues after
// that id, and a fresh connection replays from the start, so reload works.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	var after int64
	for _, v := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("from")} {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > after {
			after = n
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	bw := bufio.NewWriter(w)
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	for {
		s.mu.Lock()
		var batch [][]byte
		if int(after) < len(s.raw) {
			batch = append(batch, s.raw[after:]...)
		}
		wait := s.changed
		s.mu.Unlock()
		for _, line := range batch {
			after++
			fmt.Fprintf(bw, "id: %d\ndata: %s\n\n", after, line)
		}
		if len(batch) == 0 {
			bw.WriteString(": ok\n\n")
		}
		if bw.Flush() != nil {
			return
		}
		fl.Flush()
		if len(batch) > 0 {
			continue // more may have arrived while writing
		}
		select {
		case <-wait:
		case <-beat.C:
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	run := r.URL.Query().Get("run")
	s.mu.Lock()
	defer s.mu.Unlock()
	if run == "" {
		run = s.latest
	}
	for i := len(s.events) - 1; i >= 0; i-- {
		if ev := s.events[i]; ev.Type == TypeGraphStart && ev.Topology != nil && (run == "" || ev.Run == run) {
			writeJSON(w, http.StatusOK, ev.Topology)
			return
		}
	}
	http.Error(w, "no run yet", http.StatusNotFound)
}

func (s *Server) handleGetBreakpoints(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"nodes": s.Breakpoints()})
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Answer string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Answer(in.Answer); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSetBreakpoints(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Nodes []string `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.SetBreakpoints(in.Nodes)
	writeJSON(w, http.StatusOK, map[string]any{"nodes": s.Breakpoints()})
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Control(in.Action); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- replay ----

// JournalNames are the file names looked for in a directory, in order.
// DefaultJournal is the one to pick when writing: it cannot clash with the
// free-form journal.jsonl some programs keep for their own notes, which is
// only used when no graphweb.jsonl exists.
var JournalNames = []string{DefaultJournal, "journal.jsonl"}

// DefaultJournal is the conventional journal file name inside a runs dir.
const DefaultJournal = "graphweb.jsonl"

// JournalFile resolves path to a journal: a file is itself, a directory means
// the first of JournalNames found in it.
func JournalFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return path, nil
	}
	for _, n := range JournalNames {
		p := filepath.Join(path, n)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("graphweb: no %s in %s", strings.Join(JournalNames, " or "), path)
}

// NewReplay loads a journal (a JSONL file, or a directory holding
// journal.jsonl) into a read-only server: same UI, no controls, the whole
// run available to the scrubber at once.
func NewReplay(path string) (*Server, error) {
	file, err := JournalFile(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := newServer("", nil)
	s.readOnly = true
	rd := bufio.NewReaderSize(f, 1<<20)
	for n := 1; ; n++ {
		line, err := rd.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			var ev Event
			if jerr := json.Unmarshal(t, &ev); jerr != nil {
				return nil, fmt.Errorf("graphweb: %s line %d: %w", file, n, jerr)
			}
			if ev.Type == "" {
				return nil, fmt.Errorf("graphweb: %s line %d: not a graphweb event (no type)", file, n)
			}
			// Renumber so ids are dense even if a journal was edited.
			ev.Seq = int64(len(s.events)) + 1
			out, _ := json.Marshal(ev)
			s.events = append(s.events, ev)
			s.raw = append(s.raw, out)
			if ev.Type == TypeGraphStart {
				s.latest = ev.Run
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if len(s.events) == 0 {
		return nil, fmt.Errorf("graphweb: %s: empty journal", file)
	}
	return s, nil
}

// ---- artifacts ----

// guessMime types a file by extension, with a fallback for the common ones the
// OS mime database may lack.
func guessMime(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if m := mime.TypeByExtension(ext); m != "" {
		return m
	}
	switch ext {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".json":
		return "application/json"
	case ".webp":
		return "image/webp"
	case ".txt", ".log":
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

// handleArtifact streams the file of the artifact event with the given seq.
// The path comes only from the journal, never from the request. The response
// is sandboxed so an html/svg artifact cannot script the UI's origin.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	s.mu.Lock()
	var a *Event
	if err == nil && seq >= 1 && seq <= int64(len(s.events)) {
		if ev := s.events[seq-1]; ev.Type == TypeArtifact && ev.Artifact != nil {
			a = &ev
		}
	}
	s.mu.Unlock()
	if a == nil {
		http.NotFound(w, r)
		return
	}
	serveFile(w, r, a.Artifact.Path, a.Artifact.Mime)
}

// serveFile streams path with its mime type (guessed when empty), sandboxed.
func serveFile(w http.ResponseWriter, r *http.Request, path, ct string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "file is gone", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.Error(w, "file is gone", http.StatusNotFound)
		return
	}
	if ct == "" {
		ct = guessMime(path)
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}

// handleReviewItem streams item n of the review carried by the interrupt
// event seq. As with artifacts, the path comes only from the journal.
func (s *Server) handleReviewItem(w http.ResponseWriter, r *http.Request) {
	seq, err1 := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	n, err2 := strconv.Atoi(r.PathValue("n"))
	var path string
	s.mu.Lock()
	if err1 == nil && err2 == nil && seq >= 1 && seq <= int64(len(s.events)) {
		if ev := s.events[seq-1]; ev.Type == TypeInterrupt && n >= 0 && n < len(ev.Items) {
			path = ev.Items[n].Path
		}
	}
	s.mu.Unlock()
	if path == "" {
		http.NotFound(w, r)
		return
	}
	serveFile(w, r, path, "")
}
