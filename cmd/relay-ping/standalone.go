package main

import (
    "fmt"
    "log"
    "net"
    "net/http"
    "os"
    "os/exec"
    "path/filepath"
)

func startStandaloneWebServer(addr string) {
    if _, err := os.Stat(webDir); err != nil {
        log.Fatalf("web directory %q not found: %v", webDir, err)
    }
    webRoot := filepath.Clean(webDir)

    ln, err := net.Listen("tcp", addr)
    if err != nil {
        log.Fatalf("listen: %v", err)
    }
    url := fmt.Sprintf("http://%s", ln.Addr().String())
    mux := http.NewServeMux()
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        if r.URL.RawQuery == "" {
            http.Redirect(w, r, "/?webxdc=1", http.StatusFound)
            return
        }
        http.ServeFile(w, r, filepath.Join(webRoot, "index.html"))
    })
    mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(filepath.Join(webRoot, "static")))))

    fmt.Printf("standalone web ui: %s\n", url)
    _ = exec.Command("xdg-open", url+"/?webxdc=1").Start()
    _ = (&http.Server{Handler: mux}).Serve(ln)
}
