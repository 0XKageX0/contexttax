package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	_ "embed"
)

//go:embed web/index.html
var indexHTML []byte

const tokenPlaceholder = "__CONTEXTTAX_TOKEN__"
const noncePlaceholder = "__CSP_NONCE__"

func newToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type app struct {
	root  string
	port  string
	token string
}

func newHandler(root string) http.Handler {
	return newApp(root, "").routes()
}

func newApp(root, port string) *app {
	return &app{root: root, port: port, token: newToken()}
}

var scanSlots = make(chan struct{}, 2)

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.serveIndex)
	mux.HandleFunc("GET /api/report", func(w http.ResponseWriter, r *http.Request) {
		a.writeReport(w)
	})
	mux.HandleFunc("POST /api/hold", func(w http.ResponseWriter, r *http.Request) {
		a.gate(w, r, "hold")
	})
	mux.HandleFunc("POST /api/release", func(w http.ResponseWriter, r *http.Request) {
		a.gate(w, r, "release")
	})
	mux.HandleFunc("POST /api/finish", func(w http.ResponseWriter, r *http.Request) {
		a.gate(w, r, "finish")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, a.port) {
			setAPIHeaders(w)
			writeAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func setAPIHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
}

func (a *app) serveIndex(w http.ResponseWriter, r *http.Request) {
	nonce := newToken()[:16]
	csp := "default-src 'none'; script-src 'nonce-" + nonce + "'; script-src-attr 'none'; style-src 'nonce-" + nonce + "'; img-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'"
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := bytes.ReplaceAll(indexHTML, []byte(tokenPlaceholder), []byte(a.token))
	page = bytes.ReplaceAll(page, []byte(noncePlaceholder), []byte(nonce))
	_, _ = w.Write(page)
}

func (a *app) gate(w http.ResponseWriter, r *http.Request, op string) {
	setAPIHeaders(w)
	if err := a.authorize(r); err != nil {
		writeAPIError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "expected JSON with a path")
		return
	}
	var err error
	switch op {
	case "hold":
		err = Hold(a.root, body.Path)
	case "release":
		err = Release(a.root, body.Path)
	default:
		err = FinishHold(a.root, body.Path)
	}
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case os.IsNotExist(err):
			status = http.StatusNotFound
		case errors.Is(err, errAlreadyHeld), errors.Is(err, errInUse):
			status = http.StatusConflict
		}
		fmt.Fprintf(os.Stderr, "contexttax: %s %s: %v\n", op, body.Path, err)
		writeAPIError(w, status, clientMessage(err))
		return
	}
	a.writeReport(w)
}

func (a *app) authorize(r *http.Request) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(media, "application/json") {
		return errNotInstruction
	}
	if !tokenMatch(r.Header.Get("X-Contexttax-Token"), a.token) {
		return errNotInstruction
	}
	if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin, r.Host) {
		return errNotInstruction
	}
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "", "none", "same-origin":
	default:
		return errNotInstruction
	}
	return nil
}

func tokenMatch(got, want string) bool {
	if len(got) != len(want) || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func originAllowed(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(u.Scheme, "http") || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

func clientMessage(err error) string {
	switch {
	case errors.Is(err, errNotInstruction):
		return "that path is not an instruction file inside the repository"
	case errors.Is(err, errAlreadyHeld):
		return errAlreadyHeld.Error()
	case errors.Is(err, errNotHeld):
		return errNotHeld.Error()
	case errors.Is(err, errInUse):
		return errInUse.Error()
	case os.IsNotExist(err):
		return "that file does not exist"
	case os.IsPermission(err):
		return "permission denied"
	default:
		return "could not update that file"
	}
}

func (a *app) writeReport(w http.ResponseWriter) {
	setAPIHeaders(w)
	select {
	case scanSlots <- struct{}{}:
		defer func() { <-scanSlots }()
	default:
		writeAPIError(w, http.StatusServiceUnavailable, "the scanner is busy")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	rep, err := Scan(a.root)
	enc := json.NewEncoder(w)
	if err != nil {
		fmt.Fprintf(os.Stderr, "contexttax: scan: %v\n", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = enc.Encode(map[string]string{"error": "could not scan the repository"})
		return
	}
	_ = enc.Encode(rep)
}

func writeAPIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func hostAllowed(hostport, wantPort string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host = strings.Trim(hostport, "[]")
		port = ""
	}
	host = strings.Trim(host, "[]")
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
	default:
		return false
	}
	if wantPort != "" && port != wantPort {
		return false
	}
	return true
}

// Serve scans root on each request and serves the dashboard on addr.
// addr is forced to 127.0.0.1. A wildcard or other host is not bound.
func Serve(root, addr string) error {
	abs, err := absDir(root)
	if err != nil {
		return err
	}
	addr, err = listenAddr(addr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		port = ""
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           newApp(abs, port).routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	go openBrowser(localURL(ln.Addr().String()))
	return srv.Serve(ln)
}

// listenAddr rewrites loopback and wildcard hosts to 127.0.0.1 and rejects
// every other host. The port is kept.
func listenAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = ":7070"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", fmt.Errorf("listen address %q must be host:port", addr)
	}
	switch strings.ToLower(host) {
	case "", "localhost", "0.0.0.0", "::", "[::]", "127.0.0.1", "::1":
		return net.JoinHostPort("127.0.0.1", port), nil
	default:
		return "", fmt.Errorf("contexttax only listens on 127.0.0.1, not %s", host)
	}
}

func localURL(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "http://127.0.0.1:7070/"
	}
	return "http://127.0.0.1:" + port + "/"
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
