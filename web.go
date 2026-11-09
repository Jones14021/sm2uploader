package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

//go:embed web.html
var webPage []byte

func webHandler(printer *Printer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			methodNotAllowedResponse(w, r.Method)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webPage)
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowedResponse(w, r.Method)
			return
		}
		status, err := readPrinterStatus(printer)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("/api/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowedResponse(w, r.Method)
			return
		}
		if r.Header.Get("X-Sm2uploader") != "1" {
			http.Error(w, "Invalid request", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, FILE_SIZE_MAX+(1<<20))
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, "Invalid or oversized upload", http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "Select a file", http.StatusBadRequest)
			return
		}
		defer file.Close()
		payload := NewPayload(file, header.Filename, header.Size)
		if err := Connector.Upload(printer, payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"name": payload.Name, "size": payload.Size})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil || (host != "127.0.0.1" && host != "localhost") {
			http.Error(w, "Invalid host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Scheme != "http" || !strings.EqualFold(parsed.Host, r.Host) {
				http.Error(w, "Invalid origin", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; object-src 'none'; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	})
}

func startWebInterface(printer *Printer) error {
	listener, err := net.Listen("tcp", "127.0.0.1:8889")
	if err != nil {
		return fmt.Errorf("web interface: %w", err)
	}
	log.Printf("Web interface: http://%s", listener.Addr())
	server := &http.Server{Handler: webHandler(printer), ReadHeaderTimeout: 10 * time.Second}
	return server.Serve(listener)
}
