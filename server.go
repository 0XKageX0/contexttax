package main

import (
	"encoding/json"
	"net/http"
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
	})
	return mux
}

// Serve scans root on each request and serves the dashboard on addr.
func Serve(root, addr string) error {
	abs, err := absDir(root)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(abs),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return srv.ListenAndServe()
}
