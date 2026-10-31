package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Akses jaringan: bind address dan tunnel keluar.
//
// Dua hal yang dijaga di sini:
//   1. Mengubah bind ke 0.0.0.0 TIDAK boleh terjadi kalau panel belum punya
//      password — kalau tidak, seluruh mesin di jaringan bisa membuka panel dan
//      melihat cookie akun. Penolakan itu di sini, bukan di UI.
//   2. Tunnel dijalankan sebagai proses anak (cloudflared) dan URL-nya diambil
//      dari stdout; tidak ada kredensial yang ditulis ke disk.

var tunnelCandidates = []string{
	`C:\Users\Design\AppData\Roaming\9router\bin\cloudflared.exe`,
	`C:\Program Files\cloudflared\cloudflared.exe`,
	`C:\Program Files (x86)\cloudflared\cloudflared.exe`,
}

type tunnelState struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	url     string
	err     string
	started time.Time
}

var tunnel tunnelState

func cloudflaredPath() string {
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p
	}
	for _, p := range tunnelCandidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func tunnelSnapshot() map[string]interface{} {
	tunnel.mu.Lock()
	defer tunnel.mu.Unlock()
	running := tunnel.cmd != nil && tunnel.cmd.Process != nil
	out := map[string]interface{}{
		"running": running,
		"url":     tunnel.url,
		"error":   tunnel.err,
	}
	if running && !tunnel.started.IsZero() {
		out["uptime_sec"] = int(time.Since(tunnel.started).Seconds())
	}
	return out
}

var tunnelURLRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

func tunnelStart() error {
	tunnel.mu.Lock()
	if tunnel.cmd != nil && tunnel.cmd.Process != nil {
		tunnel.mu.Unlock()
		return nil
	}
	tunnel.url = ""
	tunnel.err = ""
	tunnel.mu.Unlock()

	bin := cloudflaredPath()
	if bin == "" {
		return fmt.Errorf("cloudflared tidak ditemukan (pasang cloudflared atau taruh di PATH)")
	}
	port := cfg.Port
	cmd := exec.Command(bin, "tunnel", "--no-autoupdate", "--url", fmt.Sprintf("http://127.0.0.1:%d", port))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	tunnel.mu.Lock()
	tunnel.cmd = cmd
	tunnel.started = time.Now()
	tunnel.mu.Unlock()

	scan := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if u := tunnelURLRe.FindString(line); u != "" {
				tunnel.mu.Lock()
				tunnel.url = u
				tunnel.mu.Unlock()
			}
		}
	}
	go scan(stdout)
	go scan(stderr)
	go func() {
		_ = cmd.Wait()
		tunnel.mu.Lock()
		tunnel.cmd = nil
		if tunnel.url == "" && tunnel.err == "" {
			tunnel.err = "cloudflared berhenti sebelum memberi URL"
		}
		tunnel.mu.Unlock()
	}()
	return nil
}

func tunnelStop() {
	tunnel.mu.Lock()
	cmd := tunnel.cmd
	tunnel.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	tunnel.mu.Lock()
	tunnel.cmd = nil
	tunnel.url = ""
	tunnel.mu.Unlock()
}

func validHost(h string) bool {
	h = strings.TrimSpace(h)
	if h == "127.0.0.1" || h == "localhost" || h == "0.0.0.0" || h == "::" || h == "::1" {
		return true
	}
	return net.ParseIP(h) != nil
}

// handleAdminNetwork: GET status, PATCH host.
func handleAdminNetwork(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		writeJSON(w, 200, map[string]interface{}{
			"host":            cfg.Host,
			"port":            cfg.Port,
			"admin_token_set": cfg.AdminToken != "" || os.Getenv("ADMIN_TOKEN") != "",
			"lan_ready":       cfg.Host == "0.0.0.0" || cfg.Host == "::",
			"needs_restart":   false,
			"tunnel":          tunnelSnapshot(),
			"cloudflared":     cloudflaredPath() != "",
			"platform":        runtime.GOOS,
		})
	case "PATCH":
		var p struct {
			Host string `json:"host"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		h := strings.TrimSpace(p.Host)
		if !validHost(h) {
			writeJSON(w, 400, map[string]string{"error": "host tidak valid"})
			return
		}
		// 0.0.0.0 membuka panel ke seluruh jaringan. Tanpa password itu
		// sama dengan membagikan cookie akun ke siapa pun di LAN.
		if h != "127.0.0.1" && h != "localhost" && h != "::1" {
			if cfg.AdminToken == "" && os.Getenv("ADMIN_TOKEN") == "" {
				writeJSON(w, 400, map[string]string{
					"error": "set ADMIN_TOKEN dulu: membuka ke 0.0.0.0 tanpa password berarti siapa pun di jaringan bisa membuka panel",
				})
				return
			}
		}
		cfg.Host = h
		if err := saveConfigFile(); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{
			"host":          cfg.Host,
			"needs_restart": true,
			"note":          "alamat bind dibaca saat proses start; jalankan ulang novagemini untuk menerapkannya",
		})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// handleAdminTunnel: POST {action: start|stop}.
func handleAdminTunnel(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		writeJSON(w, 200, tunnelSnapshot())
	case "POST":
		var p struct {
			Action string `json:"action"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &p)
		switch strings.ToLower(strings.TrimSpace(p.Action)) {
		case "start":
			if err := tunnelStart(); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
			// beri waktu cloudflared mencetak URL
			for i := 0; i < 20; i++ {
				time.Sleep(500 * time.Millisecond)
				if s := tunnelSnapshot(); s["url"] != "" || s["error"] != "" {
					break
				}
			}
			writeJSON(w, 200, tunnelSnapshot())
		case "stop":
			tunnelStop()
			writeJSON(w, 200, tunnelSnapshot())
		default:
			writeJSON(w, 400, map[string]string{"error": "action harus start atau stop"})
		}
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}
