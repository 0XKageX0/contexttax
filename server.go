package main

import (
	"encoding/json"
	"errors"
	"fmt"
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

func newHandler(root string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/report", func(w http.ResponseWriter, r *http.Request) {
		writeReport(w, root)
	})
	mux.HandleFunc("POST /api/hold", func(w http.ResponseWriter, r *http.Request) {
		gate(w, r, root, true)
	})
	mux.HandleFunc("POST /api/release", func(w http.ResponseWriter, r *http.Request) {
		gate(w, r, root, false)
	})
	return mux
}

func gate(w http.ResponseWriter, r *http.Request, root string, hold bool) {
	if crossOrigin(r) {
		writeAPIError(w, http.StatusForbidden, "cross-origin request rejected")
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
	if hold {
		err = Hold(root, body.Path)
	} else {
		err = Release(root, body.Path)
	}
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, os.ErrNotExist):
			status = http.StatusNotFound
		case errors.Is(err, errAlreadyHeld), errors.Is(err, errInUse):
			status = http.StatusConflict
		}
		writeAPIError(w, status, err.Error())
		return
	}
	writeReport(w, root)
}

func writeReport(w http.ResponseWriter, root string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	rep, err := Scan(root)
	enc := json.NewEncoder(w)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = enc.Encode(map[string]string{"error": err.Error()})
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

// Serve scans root on each request and serves the dashboard on addr.
// addr is forced to 127.0.0.1. A wildcard or other host is not bound.
// The local page is the interface, so a browser is opened once it is listening.
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
	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(abs),
		ReadHeaderTimeout: 5 * time.Second,
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

// crossOrigin reports whether a hold or release POST came from another site.
// A missing Origin is allowed so a local curl still works. Browsers send Origin
// on POST, and Sec-Fetch-Site: cross-site is always rejected.
func crossOrigin(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return true
	}
	host := u.Hostname()
	if host != "127.0.0.1" && !strings.EqualFold(host, "localhost") && host != "::1" {
		return true
	}
	oport := u.Port()
	if oport == "" {
		if u.Scheme == "https" {
			oport = "443"
		} else {
			oport = "80"
		}
	}
	_, rport, err := net.SplitHostPort(r.Host)
	if err != nil || rport == "" || oport != rport {
		return true
	}
	return false
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
