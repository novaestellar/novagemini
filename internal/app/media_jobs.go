package app

// /v1/media —— 媒体生成的通用异步任务端点(生图 / 音乐 / 视频 / 画布)。
//
// 跟 videos.go 一个思路,只是泛化到所有媒体工具(inner[49] != 0):
//   POST /v1/media               建任务,立即返回 job JSON (含 id / status:"queued")
//   GET  /v1/media/{id}          轮询状态 queued|in_progress|completed|failed
//   GET  /v1/media/{id}/content  完成后取第一份产物的原始字节
//
// 为什么异步:生图几秒、音乐 ~30s、视频几十秒到几分钟,同步把一个 HTTP 请求
// 挂在那里不现实(9router 侧还会在 ~129s 掐断)。把等待挪到后台 goroutine,
// 前台立即返 id,/v1/chat/completions 遇到媒体模型时也走这条路(见 server.go)。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// mediaJobMaxConcurrent membatasi berapa job media latar yang boleh jalan
// bersamaan. Tanpa ini, puluhan permintaan media bisa menumpuk dan masing-masing
// memegang slot IP — persis yang bikin akun/exit kelelahan. Kalau penuh, job
// tetap diterima tapi bertahan "queued" sampai ada slot bebas.
const mediaJobMaxConcurrent = 4

// mediaJobSemaphore adalah kunci konkurensi(kanal ber-buffer). Satu token = satu
// job yang sedang menyentuh upstream.
var mediaJobSemaphore = make(chan struct{}, mediaJobMaxConcurrent)

// mediaJobTimeout adalah batas atas satu job. Menutup goroutine yang menggantung
// (mis. exit diam-diam berhenti mengirim) supaya tidak menahan slot selamanya.
// 40 menit: 600s generasi + 600s polling musik + 900s video, plus ruang
// untuk pengalihan akun. Job async tidak terikat klien, jadi murah menunggu.
const mediaJobTimeout = 40 * time.Minute

type mediaJob struct {
	ID        string             `json:"id"`
	Object    string             `json:"object"` // "media.job"
	Model     string             `json:"model"`
	Status    string             `json:"status"` // queued | in_progress | completed | failed
	CreatedAt int64              `json:"created_at"`
	Prompt    string             `json:"prompt,omitempty"`
	Error     string             `json:"error,omitempty"`
	Content   string             `json:"content,omitempty"` // final markdown (artifact links)
	Artifacts []mediaJobArtifact `json:"artifacts,omitempty"`

	// dedupKey mengidentifikasi permintaan yang sama selagi job masih in-flight
	// (lihat findOrCreateMediaJob). Tidak di-ekspor: klien tidak perlu melihatnya.
	dedupKey string
}

type mediaJobArtifact struct {
	Mime   string `json:"mime"`
	Bytes  int    `json:"bytes"`
	URL    string `json:"url,omitempty"`    // /files/... when saved to disk
	Base64 string `json:"base64,omitempty"` // only when small

	// data adalah salinan byte di memori; sengaja tidak di-ekspor supaya tidak
	// ikut ke JSON (base64 besar / byte mentah tak perlu dikirim dua kali).
	data []byte
}

var (
	mediaJobs   = map[string]*mediaJob{}
	mediaJobsMu sync.Mutex
)

// putMediaJob menyimpan job dan sekalian membersihkan yang lebih tua dari 2 jam,
// sama seperti putVideoJob.
func putMediaJob(j *mediaJob) {
	mediaJobsMu.Lock()
	cutoff := time.Now().Unix() - 2*3600
	for id, old := range mediaJobs {
		if old.CreatedAt < cutoff {
			delete(mediaJobs, id)
		}
	}
	mediaJobs[j.ID] = j
	mediaJobsMu.Unlock()
}

func getMediaJob(id string) *mediaJob {
	mediaJobsMu.Lock()
	defer mediaJobsMu.Unlock()
	return mediaJobs[id]
}

// mediaDedupKey menghasilkan kunci dedup untuk sebuah permintaan media.
//
// Default: sha256(model + "\x00" + prompt + "\x00" + imageDigest). Bila klien
// mengirim header Idempotency-Key, nilai itu yang dipakai (scoped per model):
// "idem:" + model + "\x00" + key. Dengan begitu satu idempotency key yang sama
// pada model berbeda tetap dianggap permintaan berbeda.
func mediaDedupKey(model, prompt string, images []pendingUpload, idemKey string) string {
	if idemKey != "" {
		return "idem:" + model + "\x00" + idemKey
	}
	sum := sha256.Sum256([]byte(model + "\x00" + prompt + "\x00" + imageDigest(images)))
	return hex.EncodeToString(sum[:])
}

// imageDigest adalah hash stabil dari payload gambar yang masuk, berurutan
// (mime + byte data). Kosong bila tidak ada gambar.
func imageDigest(images []pendingUpload) string {
	if len(images) == 0 {
		return ""
	}
	h := sha256.New()
	for _, im := range images {
		h.Write([]byte(im.Mime))
		h.Write([]byte{0})
		h.Write(im.Data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// findOrCreateMediaJob adalah inti dedup. Di bawah satu kunci mutex:
// bersihkan job basi (>2 jam), cari job in-flight dengan kunci sama, kalau ada
// kembalikan itu (deduped=true); kalau tidak, buat job "queued" baru
// (deduped=false).
//
// Kunci hanya "dipakai" selama status queued/in_progress. Job yang sudah
// completed/failed tidak pernah dicocokkan, jadi permintaan identik berikutnya
// selalu membuat job baru — syarat "dua kali bertanya = dua generasi" tetap
// terjaga. Status diperiksa saat lookup, bukan lewat timer.
func findOrCreateMediaJob(model, prompt string, images []pendingUpload, idemKey string) (*mediaJob, bool) {
	key := mediaDedupKey(model, prompt, images, idemKey)
	now := time.Now().Unix()
	cutoff := now - 2*3600

	mediaJobsMu.Lock()
	defer mediaJobsMu.Unlock()

	// Sapu job basi sekaligus (map tidak boleh tumbuh tanpa batas).
	for id, old := range mediaJobs {
		if old.CreatedAt < cutoff {
			delete(mediaJobs, id)
		}
	}
	// In-flight dengan kunci sama -> pakai ulang, jangan buat job kedua.
	for _, j := range mediaJobs {
		if j.dedupKey == key && (j.Status == "queued" || j.Status == "in_progress") {
			return j, true
		}
	}
	j := &mediaJob{
		ID:        "media_" + randHex(16),
		Object:    "media.job",
		Model:     model,
		Status:    "queued",
		CreatedAt: now,
		Prompt:    prompt,
		dedupKey:  key,
	}
	mediaJobs[j.ID] = j
	return j, false
}

// startOrDedupMediaJob adalah SATU-SATUNYA titik masuk pembuatan job media,
// dipakai baik oleh cabang async handleChatCompletions maupun handleCreateMedia
// supaya kedua jalur tidak bisa berbeda perilaku. Hanya job baru yang dijalankan
// di latar; job yang di-dedup tidak menyentuh upstream.
func startOrDedupMediaJob(model, prompt string, images []pendingUpload, baseURL, idemKey string) (*mediaJob, bool) {
	j, deduped := findOrCreateMediaJob(model, prompt, images, idemKey)
	if !deduped {
		go runMediaJob(j, images, baseURL)
	}
	return j, deduped
}

// runMediaJob menjalankan satu job media di latar.
//
// Penting: goroutine ini **independen** dari klien yang memicunya. Ia memakai
// context.Background(), bukan request context, supaya klien yang pergi
// (9router keburu timeout) tidak membatalkan job yang sebenarnya masih jalan.
// Bandingkan dengan jalur /v1/chat yang justru ingin dibatalkan saat klien pergi.

// mediaPoolHealthy menjawab apakah ada akun yang baru saja terbukti hidup.
//
// Kenapa dipakai sebelum job jalan: pekerja media wajib lewat sesi web Gemini,
// dan pool kita bisa berisi akun yang sesinya sudah dicabut Google. Tanpa
// pemeriksaan ini, job menghabiskan slot (maksimal 4 bersamaan) hanya untuk
// memutar akun yang semuanya mati - persis gejala "job menggantung 300 detik
// padahal sebelumnya selesai 140 detik". Lebih baik gagal cepat dengan alasan
// yang bisa ditindaklanjuti daripada menahan slot 40 menit.
func mediaPoolHealthy(maxAge time.Duration) (bool, string) {
	accounts := accountList()
	if len(accounts) == 0 {
		return true, "" // tanpa akun, jalur anonim yang menangani
	}
	now := time.Now().Unix()
	for _, a := range accounts {
		if a.Status != "enabled" {
			continue
		}
		if a.LastOkAt > 0 && now-a.LastOkAt < int64(maxAge.Seconds()) {
			return true, ""
		}
	}
	// Tidak ada yang baru sukses. Itu BUKAN bukti mati: pool yang sedang tidak
	// dipakai (tidak ada permintaan chat selama 15 menit) akan selalu gagal di
	// sini, sementara probe 15 menit sudah menjawab liveness dengan tepat.
	// Nilai probe yang dipakai, bukan kebaruan permintaan - kalau tidak, media
	// hanya bisa dipakai kalau ada yang mengobrol lebih dulu.
	for _, a := range accounts {
		if a.Status != "enabled" {
			continue
		}
		if a.LastOkAt == 0 {
			return true, "" // pool baru diisi, jangan dihukum
		}
		cap := probeForAccount(a.ID)
		if cap.Alive || cap.CheckedAt == 0 {
			return true, ""
		}
	}
	return false, "no healthy account in the pool (every enabled session failed its " +
		"last check) - sign in again in the browser, then re-run the sync"
}

func runMediaJob(j *mediaJob, images []pendingUpload, baseURL string) {
	// Tunggu slot dulu; selama menunggu status tetap "queued" (bukan in_progress).
	mediaJobSemaphore <- struct{}{}
	defer func() { <-mediaJobSemaphore }()

	setMediaJobStatus(j, "in_progress", "")

	// Gerbang kesehatan pool: gagal cepat lebih baik daripada menahan slot.
	if ok, why := mediaPoolHealthy(15 * time.Minute); !ok {
		recordRequest("media.jobs", j.Model, j.Prompt, "", nil, 503, why, false)
		finishMediaJob(j, nil, "", why)
		return
	}

	_, mc, err := resolveModel(j.Model)
	if err != nil {
		recordRequest("media.jobs", j.Model, j.Prompt, "", nil, 400, err.Error(), false)
		finishMediaJob(j, nil, "", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), mediaJobTimeout)
	defer cancel()

	text, _, res, err := callGemini(ctx, j.Prompt, j.Prompt, mc, nil, images, nil, nil, baseURL)
	if err != nil {
		recordRequest("media.jobs", j.Model, j.Prompt, "", res, 502, err.Error(), false)
		finishMediaJob(j, nil, "", err.Error())
		return
	}
	recordRequest("media.jobs", j.Model, j.Prompt, text, res, 200, "", false)

	// Bangun artefak terstruktur DENGAN MENURUNKAN dari konten yang sudah
	// dirender callGemini. callGemini memanggil appendArtifactMarkdownWithBase,
	// yang lewat artifactLink sudah mengambil SATU keputusan simpan-ke-disk vs
	// inline dan meng-embed jawabannya sebagai link markdown di `text`.
	// Di sini kita TIDAK memanggil saveArtifact lagi — kalau memanggil, byte yang
	// sama ditulis dua kali dengan dua nama file (itulah bug artefak ganda).
	arts := deriveMediaJobArtifacts(text, res.Artifacts)
	finishMediaJob(j, arts, text, "")
}

// mediaLinkRe mencocokkan kelima bentuk link yang dihasilkan artifactLink,
// mengambil HANYA target di dalam tanda kurung:
//
//	![image](<target>)  [video](..)  [audio](..)  [media](..)  [canvas](..)
//
// Target bisa berupa URL (/files/...) atau data URL (data:<mime>;base64,<...>).
// Base64 tak mengandung ')', dan URL /files/... juga tidak, jadi [^)]* aman.
//
// `canvas` WAJIB ikut: dokumen canvas tidak lewat res.Artifacts (isinya inline
// di StreamGenerate, bukan hasil unduhan), jadi satu-satunya jejaknya di konten
// adalah link ini. Tanpa cabang ini job canvas selesai dengan artifacts kosong —
// itulah sebabnya dulu tidak pernah ada artifact berjenis canvas.
var mediaLinkRe = regexp.MustCompile(`!\[image\]\(([^)]*)\)|\[(video|audio|media|canvas)\]\(([^)]*)\)`)

// mediaFileSize mengembalikan ukuran file untuk URL /files/<rel> (absolut maupun
// relatif). Dipakai saat artefak disintesis dari link — byte-nya sudah ada di
// disk, tapi tidak dipegang memori, jadi Bytes tidak bisa diisi dari len(data).
func mediaFileSize(url string) int {
	i := strings.Index(url, "/files/")
	if i < 0 {
		return 0
	}
	rel := strings.TrimPrefix(url[i:], "/files/")
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || strings.Contains(rel, "..") {
		return 0
	}
	root, err := filepath.Abs(mediaDir())
	if err != nil {
		return 0
	}
	st, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil || st.IsDir() {
		return 0
	}
	return int(st.Size())
}

// linkKindMime menebak MIME dari jenis link saat sumber byte tidak tersedia
// (kasus canvas: byte-nya sudah ditulis ke disk oleh artifactLink, bukan dipegang
// res.Artifacts).
func linkKindMime(kind string) string {
	switch kind {
	case "image":
		return "image/*"
	case "video":
		return "video/mp4"
	case "audio":
		return "audio/mpeg"
	case "canvas":
		return "text/html"
	default:
		return "application/octet-stream"
	}
}

// deriveMediaJobArtifacts membangun daftar artefak terstruktur dari konten
// final markdown + daftar artefak mentah, TANPA menyimpan byte ke disk.
//
// Konten final sudah memuat jawaban untuk tiap artefak, berurutan sama dengan
// res.Artifacts (appendArtifactMarkdownWithBase menambahkan link di ujung teks,
// satu per artefak, urut). Kita parse link itu berurutan lalu pasangkan dengan
// res.Artifacts pada indeks yang sama:
//
//   - target data URL  -> Base64 diisi, URL dikosongkan
//   - target /files/.. -> URL diisi, Base64 dikosongkan
//
// Mime dan Bytes selalu diambil dari entri res.Artifacts yang bersangkutan, dan
// salinan byte mentah tetap disimpan di field data (dipakai GET /v1/media/{id}/content).
//
// Dua kasus khusus:
//   - Canvas: byte-nya tidak pernah masuk res.Artifacts (dokumen diambil dari
//     payload inline lalu ditulis ke disk oleh artifactLink). Jadi entri
//     disintesis dari link-nya sendiri, Mime text/html. Tanpa ini job canvas
//     selesai dengan artifacts kosong.
//   - Kalau konten tidak punya link sama sekali (mis. artefak tanpa link):
//     jangan mengarang entri.
func deriveMediaJobArtifacts(content string, src []MediaArtifact) []mediaJobArtifact {
	matches := mediaLinkRe.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}
	// Ambil target + jenisnya dari tiap match:
	//   grup 1 = target ![...], grup 2 = jenis [..], grup 3 = target [..]
	type link struct {
		kind   string
		target string
	}
	links := make([]link, 0, len(matches))
	for _, m := range matches {
		if m[1] != "" {
			links = append(links, link{kind: "image", target: m[1]})
			continue
		}
		links = append(links, link{kind: m[2], target: m[3]})
	}

	// Preamble bisa saja mengandung markdown biasa yang kebetulan cocok. Link
	// artefak selalu ditambahkan di UJUNG konten oleh appendArtifactMarkdownWithBase,
	// jadi kalau jumlah link > jumlah artefak, pakai N link TERAKHIR — itu milik
	// artefak, sisanya bagian preamble.
	if len(src) > 0 && len(links) > len(src) {
		links = links[len(links)-len(src):]
	}
	if len(src) > 0 && len(links) < len(src) {
		// Kurang dari yang diharapkan: hanya pasangkan yang ada, demi keselamatan
		// jangan mengarang entri.
		src = src[:len(links)]
	}

	arts := make([]mediaJobArtifact, 0, len(links))
	for i, ln := range links {
		var ma mediaJobArtifact
		if len(src) > 0 {
			a := src[i]
			ma = mediaJobArtifact{Mime: a.Mime, Bytes: len(a.Data), data: a.Data}
		} else {
			// Canvas / artefak tanpa byte di memori: turunkan dari jenis link.
			ma = mediaJobArtifact{Mime: linkKindMime(ln.kind)}
		}
		if strings.HasPrefix(ln.target, "data:") {
			// data:<mime>;base64,<...> — base64 dimulai setelah koma pertama.
			if idx := strings.Index(ln.target, ";base64,"); idx >= 0 {
				ma.Base64 = ln.target[idx+len(";base64,"):]
				// Mime bisa dibaca dari data URL kalau sumber kosong.
				if len(src) == 0 {
					if mi := strings.Index(ln.target, ":"); mi > 0 {
						ma.Mime = ln.target[mi+1 : idx]
					}
				}
			}
		} else {
			ma.URL = ln.target
			if len(src) == 0 {
				ma.Bytes = mediaFileSize(ln.target)
			}
		}
		arts = append(arts, ma)
	}
	return arts
}

func setMediaJobStatus(j *mediaJob, status, errStr string) {
	mediaJobsMu.Lock()
	j.Status = status
	if errStr != "" {
		j.Error = errStr
	}
	mediaJobsMu.Unlock()
}

func finishMediaJob(j *mediaJob, arts []mediaJobArtifact, content, errStr string) {
	mediaJobsMu.Lock()
	defer mediaJobsMu.Unlock()
	if errStr != "" {
		j.Status = "failed"
		j.Error = errStr
		return
	}
	j.Artifacts = arts
	j.Content = content
	j.Status = "completed"
}

// handleCreateMedia menangani POST /v1/media. Body berbentuk chat
// ({"model":...,"messages":[...]}); prompt dan gambar diekstrak dengan logika
// yang sama seperti /v1/chat/completions, jadi image_url data URL tetap jalan.
func handleCreateMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": map[string]string{"message": "method not allowed"}})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "invalid JSON"}})
		return
	}

	model := getStr(req, "model")
	if model == "" {
		model = rtCfg().DefaultModel
	}
	// Validasi lewat konfigurasi dasar (bukan resolveModel): model yang butuh
	// login tapi belum ada cookie harusnya lolos ke job, bukan dibalas
	// "bukan model media" di sini.
	if base, ok := Models[model]; !ok || base.Tool == 0 {
		writeJSON(w, 400, map[string]any{"error": map[string]string{
			"message": "model must be a media model (gemini-image, gemini-music, gemini-video, gemini-canvas)",
			"type":    "invalid_request_error"}})
		return
	}

	messagesRaw, _ := req["messages"].([]interface{})
	var messages []map[string]interface{}
	for _, m := range messagesRaw {
		if mm, ok := m.(map[string]interface{}); ok {
			messages = append(messages, mm)
		}
	}
	images, err := collectImages(messages, "")
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{
			"message": err.Error(), "type": "invalid_request_error"}})
		return
	}
	toolsRaw, _ := req["tools"].([]interface{})
	var tools []map[string]interface{}
	for _, t := range toolsRaw {
		if tm, ok := t.(map[string]interface{}); ok {
			tools = append(tools, tm)
		}
	}
	prompt, _ := messagesToPrompt(messages, tools, req["tool_choice"])
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "empty prompt"}})
		return
	}

	j, deduped := startOrDedupMediaJob(model, prompt, images, artifactBaseURL(r), r.Header.Get("Idempotency-Key"))
	w.Header().Set("x-novagemini-job", j.ID)
	note := "queued as async media job " + j.ID
	if deduped {
		w.Header().Set("x-novagemini-job-dedup", "1")
		note = "deduped to in-flight job " + j.ID
	}
	recordRequest("media.jobs", model, prompt, "", nil, 202, note, false)
	writeJSON(w, 200, j)
}

// handleMediaItem menangani GET /v1/media/{id} dan /v1/media/{id}/content.
func handleMediaItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"error": map[string]string{"message": "method not allowed"}})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/media/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	j := getMediaJob(id)
	if j == nil {
		writeJSON(w, 404, map[string]any{"error": map[string]string{
			"message": "media job not found", "type": "not_found"}})
		return
	}
	if len(parts) == 2 && parts[1] == "content" {
		mediaJobsMu.Lock()
		status := j.Status
		var data []byte
		var mime string
		if len(j.Artifacts) > 0 {
			data = j.Artifacts[0].data
			mime = j.Artifacts[0].Mime
		}
		mediaJobsMu.Unlock()
		if status != "completed" || len(data) == 0 {
			writeJSON(w, 404, map[string]any{"error": map[string]string{
				"message": "media not ready (status=" + status + ")", "type": "not_found"}})
			return
		}
		w.Header().Set("Content-Type", mime)
		w.WriteHeader(200)
		_, _ = w.Write(data)
		return
	}
	writeJSON(w, 200, j)
}
