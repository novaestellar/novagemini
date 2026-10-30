package app

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// media_store.go — menyimpan artefak media ke disk kalau ukurannya besar.
//
// Kenapa: respons media dikirim sebagai base64 di dalam teks. Untuk gambar
// (50-110 KB) itu wajar. Untuk music (7-9 MB) dan video (puluhan MB), base64
// membengkak ~33% dan membuat payload terlalu besar - 9router memutus di
// ~129s, dan klien yang menampung konteks ikut kepenuhan.
//
// Aturan: artefak >= ambang disimpan ke disk dan diganti URL; di bawah ambang
// tetap base64 supaya respons tetap self-contained untuk gambar kecil.

const (
	// defaultMediaInlineKB: di bawah ini tetap base64 (1 MB).
	defaultMediaInlineKB = 1024
)

// mediaDir mengembalikan direktori penyimpanan artefak, membuatnya bila perlu.
func mediaDir() string {
	dir := filepath.Join(".", "data", "media")
	_ = os.MkdirAll(dir, 0755)
	return dir
}

// artifactExt memetakan MIME ke ekstensi file.
// (vision.go sudah punya mediaExt untuk upload; ini versi untuk artefak hasil.)
func artifactExt(mime string) string {
	m := strings.ToLower(mime)
	switch {
	case strings.Contains(m, "jpeg"), strings.Contains(m, "jpg"):
		return ".jpg"
	case strings.Contains(m, "png"):
		return ".png"
	case strings.Contains(m, "webp"):
		return ".webp"
	case strings.Contains(m, "gif"):
		return ".gif"
	case strings.Contains(m, "mpeg"), strings.Contains(m, "mp3"):
		return ".mp3"
	case strings.Contains(m, "wav"):
		return ".wav"
	case strings.Contains(m, "ogg"):
		return ".ogg"
	case strings.Contains(m, "mp4"):
		return ".mp4"
	case strings.Contains(m, "webm"):
		return ".webm"
	// Canvas mengembalikan dokumen HTML utuh. Tanpa cabang ini ia jatuh ke ".bin"
	// sehingga panel menyajikannya sebagai unduhan octet-stream, bukan halaman
	// yang bisa dibuka — canvas jadi tidak pernah muncul sebagai artifact.
	case strings.Contains(m, "text/html"):
		return ".html"
	default:
		return ".bin"
	}
}

// saveArtifact menulis byte artefak ke disk dan mengembalikan path relatif
// terhadap mediaDir (mis. "2026-10-22/ab12cd34.jpg").
func saveArtifact(data []byte, mime string) (string, error) {
	day := time.Now().Format("2006-01-02")
	dir := filepath.Join(mediaDir(), day)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	name := hex.EncodeToString(buf) + artifactExt(mime)
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, data, 0644); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join(day, name)), nil
}

// mediaInlineLimitBytes membaca ambang dari config (0 = pakai default).
func mediaInlineLimitBytes() int {
	kb := rtCfg().MediaInlineKB
	if kb <= 0 {
		kb = defaultMediaInlineKB
	}
	return kb * 1024
}

// artifactLink menghasilkan markdown untuk satu artefak: base64 kalau kecil,
// URL kalau besar. Kalau simpan gagal, jatuh ke base64 supaya hasil tidak hilang.
func artifactLink(a MediaArtifact, baseURL string) string {
	isImage := strings.HasPrefix(a.Mime, "image/")
	isCanvas := strings.HasPrefix(a.Mime, "text/html")
	inline := func() string {
		dataURL := "data:" + a.Mime + ";base64," + base64.StdEncoding.EncodeToString(a.Data)
		switch {
		case isImage:
			return "![image](" + dataURL + ")"
		case isCanvas:
			return "[canvas](" + dataURL + ")"
		default:
			return "[media](" + dataURL + ")"
		}
	}

	// Kalau fitur simpan-ke-disk menyala, SEMUA artefak ditulis ke disk —
	// termasuk gambar kecil. Dulu gambar di bawah ambang inline dikirim sebagai
	// data URL saja, dan akibatnya tidak pernah muncul di galeri (galeri hanya
	// membaca isi data/media). Selain itu base64 160 KB di dalam markdown ikut
	// membengkakkan payload ke klien. URL /files/… jauh lebih ringan dan bisa
	// dibuka langsung. Ambang inline sekarang hanya berlaku saat fitur ini mati.
	if !rtCfg().SaveMediaToDisk {
		return inline()
	}

	rel, err := saveArtifact(a.Data, a.Mime)
	if err != nil {
		return inline()
	}

	u := strings.TrimRight(baseURL, "/") + "/files/" + rel
	switch {
	case isImage:
		return fmt.Sprintf("![image](%s)", u)
	case strings.HasPrefix(a.Mime, "video/"):
		return fmt.Sprintf("[video](%s)", u)
	case strings.HasPrefix(a.Mime, "audio/"):
		return fmt.Sprintf("[audio](%s)", u)
	case isCanvas:
		// Canvas: dokumen HTML yang bisa dibuka langsung di browser.
		return fmt.Sprintf("[canvas](%s)", u)
	default:
		return fmt.Sprintf("[media](%s)", u)
	}
}

// artifactBaseURL menentukan base URL dari request yang sedang dilayani,
// supaya link bisa diakses klien (localhost maupun IP LAN).
func artifactBaseURL(r *http.Request) string {
	if r == nil {
		return fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	host := r.Host
	if host == "" {
		host = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	}
	return scheme + "://" + host
}

// handleMediaFile melayani GET /files/<yyyy-mm-dd>/<name>.
// Path traversal ditolak: hanya nama file tunggal di bawah satu folder tanggal.
func handleMediaFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/files/")
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		writeJSON(w, 400, map[string]string{"error": "bad path"})
		return
	}
	parts := strings.Split(rel, "/")
	if len(parts) != 2 {
		writeJSON(w, 400, map[string]string{"error": "bad path"})
		return
	}
	full := filepath.Join(mediaDir(), parts[0], parts[1])
	abs, err := filepath.Abs(full)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad path"})
		return
	}
	root, _ := filepath.Abs(mediaDir())
	if !strings.HasPrefix(abs, root) {
		writeJSON(w, 403, map[string]string{"error": "forbidden"})
		return
	}
	if st, err := os.Stat(abs); err != nil || st.IsDir() {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, abs)
}
