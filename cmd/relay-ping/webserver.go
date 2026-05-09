package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"relay-ping/internal/check/latencymatrix"
)

type matrixWebServer struct {
	mu       sync.Mutex
	writeMu  sync.Mutex
	clients  map[*websocket.Conn]struct{}
	lastJSON []byte
	webRoot  string
	upgrader websocket.Upgrader
	server   *http.Server
}

const webDir = "web"

func startMatrixWebServer(addr string) (*matrixWebServer, string, error) {
	if _, err := os.Stat(webDir); err != nil {
		return nil, "", fmt.Errorf("web directory %q not found: %w", webDir, err)
	}

	m := &matrixWebServer{
		clients: make(map[*websocket.Conn]struct{}),
		webRoot: filepath.Clean(webDir),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", m.handleIndex)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(filepath.Join(m.webRoot, "static")))))
	mux.HandleFunc("/ws", m.handleWS)
	mux.HandleFunc("/pair-log", m.handlePairLog)
	mux.HandleFunc("/pair-log-stream", m.handlePairLogStream)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}
	hostPort := ln.Addr().String()
	url := fmt.Sprintf("http://%s", hostPort)
	m.server = &http.Server{Handler: mux}
	go func() {
		_ = m.server.Serve(ln)
	}()
	return m, url, nil
}

func (m *matrixWebServer) stop(ctx context.Context) error {
	m.mu.Lock()
	for c := range m.clients {
		_ = c.Close()
	}
	m.clients = map[*websocket.Conn]struct{}{}
	m.mu.Unlock()
	if m.server != nil {
		return m.server.Shutdown(ctx)
	}
	return nil
}

func (m *matrixWebServer) publishProgress(p latencymatrix.Progress) {
	payload := map[string]any{
		"type":        "progress",
		"currentFrom": p.CurrentFrom,
		"currentTo":   p.CurrentTo,
		"completed":   p.Completed,
		"total":       p.Total,
		"note":        p.Note,
		"result":      p.Result,
		"ts":          time.Now().UnixMilli(),
	}
	m.publish(payload)
}

func (m *matrixWebServer) publishDone(res latencymatrix.Result) {
	n := len(res.Servers)
	totalPairs := n * n
	payload := map[string]any{
		"type":      "done",
		"result":    res,
		"completed": totalPairs,
		"total":     totalPairs,
		"note":      "complete",
		"ts":        time.Now().UnixMilli(),
	}
	m.publish(payload)
}

func (m *matrixWebServer) publish(payload map[string]any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	m.mu.Lock()
	m.lastJSON = b
	clients := make([]*websocket.Conn, 0, len(m.clients))
	for c := range m.clients {
		clients = append(clients, c)
	}
	m.mu.Unlock()
	deadline := time.Now().Add(30 * time.Second)
	for _, c := range clients {
		_ = c.SetWriteDeadline(deadline)
		if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
			_ = c.Close()
			m.mu.Lock()
			delete(m.clients, c)
			m.mu.Unlock()
		}
	}
}

func (m *matrixWebServer) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.clients[c] = struct{}{}
	last := m.lastJSON
	m.mu.Unlock()
	if len(last) > 0 {
		m.writeMu.Lock()
		_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		_ = c.WriteMessage(websocket.TextMessage, last)
		m.writeMu.Unlock()
	}
	go func() {
		defer func() {
			_ = c.Close()
			m.mu.Lock()
			delete(m.clients, c)
			m.mu.Unlock()
		}()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func (m *matrixWebServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(m.webRoot, "index.html"))
}

func (m *matrixWebServer) handlePairLog(w http.ResponseWriter, r *http.Request) {
	clean, err := resolveAndValidateLogPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(clean)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, f)
}

func (m *matrixWebServer) handlePairLogStream(w http.ResponseWriter, r *http.Request) {
	clean, err := resolveAndValidateLogPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(clean)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	reader := bufio.NewReader(f)
	send := func(line string) {
		b, _ := json.Marshal(map[string]string{"line": line})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	send("[stream] connected")
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			send(strings.TrimRight(line, "\r\n"))
		}
		if err == nil {
			continue
		}
		if err == io.EOF {
			select {
			case <-r.Context().Done():
				send("[stream] closed")
				return
			case <-time.After(300 * time.Millisecond):
				continue
			}
		}
		send("[stream] error: " + err.Error())
		return
	}
}

func resolveAndValidateLogPath(pathArg string) (string, error) {
	p := strings.TrimSpace(pathArg)
	if p == "" {
		return "", fmt.Errorf("missing path")
	}
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) {
		clean = filepath.Clean(filepath.Join(".", clean))
	}
	rel, err := filepath.Rel(filepath.Clean("tmp"), clean)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return "", fmt.Errorf("forbidden path")
	}
	return clean, nil
}

func openBrowser(url string) {
	_ = exec.Command("xdg-open", url).Start()
}
