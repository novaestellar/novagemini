package app

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// probe.go — pemeriksaan kapabilitas akun tanpa memakai kuota.
//
// Google mengekspos status akun lewat RPC ESY5D (batchexecute). Responsnya
// memuat daftar capability aktif, daftar rollout model, dan sisa kuota per
// jenis media. Semua ini bisa dibaca tanpa mengirim satu pun job generate,
// jadi aman dipanggil berkala untuk mengisi dashboard DAN untuk memilih akun.
//
// Kenapa penting: sebelumnya status akun hanya disimpulkan dari error saat
// request (health/fail_count). Akibatnya akun free dan akun berbayar tampak
// sama saja sampai dipakai — dan karena pemilihan akun murni round-robin,
// request untuk model yang tidak dimiliki sebuah akun tetap dikirim ke akun
// itu, lalu Google menjawab 200 dengan model yang lebih rendah. Klien melihat
// "sukses" padahal modelnya salah. Probe ini memisahkan tiga hal:
//   1. sesi hidup/mati        -> SNlM0e ada/tidak
//   2. tier akun              -> daftar capability (VIDEO_GENERATION, CANVAS, ...)
//   3. model yang di-rollout  -> blok 177 (Argon4p0, Flash3p8, Flash3p7, ...)
//   4. sisa kuota media       -> *_soft di blok 192

// AccountCapability adalah hasil satu kali probe.
type AccountCapability struct {
	AccountID  int64    `json:"account_id"`
	Label      string   `json:"label"`
	Alive      bool     `json:"alive"`
	Tier       string   `json:"tier"`        // "pro" | "free" | "unknown"
	Caps       []string `json:"caps"`        // capability mentah dari Google
	Rollouts   []string `json:"rollouts"`    // nama rollout model (Argon4p0, Flash3p8, ...)
	Features   []string `json:"features"`    // nama model yang terlihat (Veo 3.1, Nano Banana 2, ...)
	QuotaImage int      `json:"quota_image"` // -1 = tidak diketahui
	QuotaMusic int      `json:"quota_music"`
	QuotaThink int      `json:"quota_think"`
	QuotaVideo int      `json:"quota_video"`
	CheckedAt  int64    `json:"checked_at"`
	Err        string   `json:"error,omitempty"`
}

var (
	probeMu    sync.Mutex
	probeCache = map[int64]AccountCapability{}
)

// probeAccount membaca status akun lewat ESY5D tanpa memakai kuota.
func probeAccount(a CookieAccount) AccountCapability {
	out := AccountCapability{
		AccountID:  a.ID,
		Label:      a.Label,
		QuotaImage: -1,
		QuotaMusic: -1,
		QuotaThink: -1,
		QuotaVideo: -1,
		CheckedAt:  time.Now().Unix(),
	}

	proxyURL := proxyURLByID(a.ProxyID)
	invalidateXSRF(a.Cookie)
	xsrf, err := getXSRF(a.Cookie, proxyURL)
	if err != nil || xsrf == "" {
		out.Err = "session dead: " + probeErr(err)
		return out
	}
	out.Alive = true

	form := url.Values{}
	form.Set("f.req", `[[["ESY5D","[]",null,"generic"]]]`)
	form.Set("at", xsrf)

	reqid := time.Now().UnixNano() % 1000000
	endpoint := fmt.Sprintf(
		"https://gemini.google.com/_/BardChatUi/data/batchexecute"+
			"?rpcids=ESY5D&source-path=%%2Fapp&bl=%s&hl=en&_reqid=%d&rt=c",
		currentBL(proxyURL), reqid)

	headers := buildGeminiHeaders(a.Cookie, extractSAPISID(a.Cookie), "")
	endpoint = geminiURLWithAuthUser(endpoint, a.Cookie)
	delete(headers, "x-goog-ext-525001261-jspb")

	status, _, body, perr := uploadPost(endpoint, headers, []byte(form.Encode()), proxyURL)
	if perr != nil {
		out.Err = "probe request: " + perr.Error()
		return out
	}
	if status != 200 {
		out.Err = fmt.Sprintf("probe HTTP %d", status)
		return out
	}

	// Payload ESY5D berbentuk: [["wrb.fr","ESY5D","<json>",...]] di dalam
	// pembungkus )]}' . Ambil string JSON-nya lalu telusuri. Regex atas teks
	// mentah tidak dipakai: kutip di dalamnya ter-escape (\" ) sehingga pola
	// biasa gagal cocok — itu sebabnya versi lama selalu menghasilkan caps null.
	root := decodeBatchexecute(string(body))
	collectProbeFields(root, &out)

	has := func(name string) bool {
		for _, c := range out.Caps {
			if c == name {
				return true
			}
		}
		return false
	}
	switch {
	case has("VIDEO_GENERATION") || has("CANVAS") || has("IMAGEN_4") || has("DEEP_RESEARCH"):
		out.Tier = "pro"
	case len(out.Caps) > 0:
		out.Tier = "free"
	default:
		out.Tier = "unknown"
	}
	// Rollout Argon / Flash3p8 hanya diberikan ke akun berbayar; kalau caps
	// kosong tapi rollout-nya ada, itu tetap akun berbayar.
	if out.Tier != "pro" {
		for _, r := range out.Rollouts {
			if strings.HasPrefix(r, "Argon4p0") || strings.HasPrefix(r, "Flash3p8") {
				out.Tier = "pro"
				break
			}
		}
	}
	if out.Tier == "unknown" && out.Alive {
		out.Tier = "free"
	}
	// Sumber kedua yang lebih jujur: daftar model akun (otAQ7b). Akun berbayar
	// menerima model Argon; akun gratis tidak. ESY5D saja tidak cukup — tiga akun
	// Pro operator dilaporkan tanpa cap berbayar, dan daftar model-lah yang
	// membuktikan sebaliknya.
	if out.Alive {
		if pro, n, err := accountProModels(a, xsrf, proxyURL); err == nil {
			if pro {
				out.Tier = "pro"
			} else if out.Tier == "pro" {
				// ESY5D bilang pro tapi daftar model tidak memuat Argon: percayai
				// daftar model, karena itu daftar yang benar-benar bisa dipakai.
				out.Tier = "free"
			}
			logf("[probe] %s: %d model, tier=%s", accountDisplayName(&a), n, out.Tier)
		}
	}
	// Penetapan operator menang atas tebakan probe.
	// Payload ESY5D tidak selalu mencantumkan cap berbayar walau langganannya
	// aktif (terukur: tiga akun Pro milik operator dilaporkan hanya
	// JASMINE/EXTENSIONS). Probe hanya bisa membaca apa yang dikirim Google,
	// sedangkan tagihan langganan diketahui operator — jadi nilai manual
	// disimpan dan dipakai untuk routing.
	if ov := strings.TrimSpace(a.TierOverride); ov == "pro" || ov == "free" {
		out.Tier = ov
	}

	// Video tidak punya entri _soft; kalau akun punya VIDEO_GENERATION
	// anggap tersedia, kalau tidak berarti 0.
	if has("VIDEO_GENERATION") {
		out.QuotaVideo = -1 // tidak diketahui, tapi berhak
	} else {
		out.QuotaVideo = 0
	}

	return out
}

// decodeBatchexecute mengambil string payload JSON dari respons batchexecute.
func decodeBatchexecute(raw string) interface{} {
	// Buang prefix )]}' dan baris panjang opsional.
	if i := strings.Index(raw, "[["); i > 0 {
		raw = raw[i:]
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var rows []interface{}
		if err := json.Unmarshal([]byte(line), &rows); err != nil {
			continue
		}
		for _, row := range rows {
			arr, ok := row.([]interface{})
			if !ok || len(arr) < 3 {
				continue
			}
			if tag, _ := arr[0].(string); tag != "wrb.fr" {
				continue
			}
			payload, ok := arr[2].(string)
			if !ok || payload == "" {
				continue
			}
			var inner interface{}
			if err := json.Unmarshal([]byte(payload), &inner); err == nil {
				return inner
			}
		}
	}
	return nil
}

// collectProbeFields menelusuri struktur ESY5D dan mengisi caps / rollouts / kuota.
func collectProbeFields(node interface{}, out *AccountCapability) {
	switch v := node.(type) {
	case []interface{}:
		// Kumpulan string: bisa daftar capability, rollout, atau kuota media.
		if strs, ok := stringSlice(v); ok && len(strs) > 0 {
			switch {
			case looksLikeSoftQuota(strs):
				for _, s := range strs {
					name, n, ok := splitNameCount(s)
					if !ok {
						continue
					}
					switch name {
					case "image_generation_soft":
						out.QuotaImage = n
					case "music_generation_soft":
						out.QuotaMusic = n
					case "deep_think_soft":
						out.QuotaThink = n
					}
				}
			case looksLikeRollout(strs):
				out.Rollouts = mergeStrings(out.Rollouts, strs)
			case looksLikeCaps(strs):
				out.Caps = mergeStrings(out.Caps, strs)
			}
			for _, s := range strs {
				if name, _, ok := splitNameCount(s); ok {
					if disp := rolloutDisplayName(name); disp != "" && !containsString(out.Features, disp) {
						out.Features = append(out.Features, disp)
					}
				}
			}
		}
		for _, child := range v {
			collectProbeFields(child, out)
		}
	case map[string]interface{}:
		for k, child := range v {
			switch k {
			case "177":
				collectProbeFields(child, out)
				continue
			case "192":
				collectProbeFields(child, out)
				continue
			}
			collectProbeFields(child, out)
		}
	}
}

func stringSlice(v []interface{}) ([]string, bool) {
	out := make([]string, 0, len(v))
	for _, e := range v {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, len(out) > 0
}

func looksLikeSoftQuota(strs []string) bool {
	for _, s := range strs {
		if strings.Contains(s, "_soft:") {
			return true
		}
	}
	return false
}

func looksLikeRollout(strs []string) bool {
	hits := 0
	for _, s := range strs {
		if _, _, ok := splitNameCount(s); ok {
			hits++
		}
	}
	return hits > 0 && hits == len(strs)
}

func looksLikeCaps(strs []string) bool {
	for _, s := range strs {
		if s == "" || s != strings.ToUpper(s) {
			return false
		}
		for _, r := range s {
			if !(r >= 'A' && r <= 'Z') && r != '_' && !(r >= '0' && r <= '9') {
				return false
			}
		}
	}
	return true
}

func splitNameCount(s string) (string, int, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, false
	}
	return s[:i], n, true
}

func mergeStrings(dst, add []string) []string {
	for _, s := range add {
		if !containsString(dst, s) {
			dst = append(dst, s)
		}
	}
	return dst
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// rolloutDisplayName menerjemahkan nama rollout internal Google ke nama model
// yang dikenal pengguna. Hanya dipakai untuk tampilan dashboard.
func rolloutDisplayName(code string) string {
	switch {
	case strings.HasPrefix(code, "Argon4p0"):
		return "4 Argon"
	case strings.HasPrefix(code, "Flash3p8"):
		return "3.8 Flash"
	case strings.HasPrefix(code, "Flash3p7"):
		return "3.7 Flash"
	case strings.HasPrefix(code, "Flash3p6"):
		return "3.6 Flash"
	case strings.HasPrefix(code, "FlashLite"):
		return "3.5 Flash-Lite"
	case strings.HasPrefix(code, "Fast3.2"):
		return "Fast 3.2"
	}
	return ""
}

// supportsModel menjawab: apakah akun ini punya akses ke model yang diminta,
// menurut hasil probe terakhir. Kalau data probe tidak ada / akun tidak hidup,
// jawabannya true — lebih baik mencoba daripada menolak berdasarkan data basi.
func (c AccountCapability) supportsModel(mc ModelConfig) bool {
	if !c.Alive {
		return true
	}
	hasCap := func(name string) bool { return containsString(c.Caps, name) }
	hasRollout := func(prefix string) bool {
		for _, r := range c.Rollouts {
			if strings.HasPrefix(r, prefix) {
				return true
			}
		}
		return false
	}
	switch mc.Tool {
	case toolVideo:
		return hasCap("VIDEO_GENERATION")
	case toolImage:
		return hasCap("IMAGEN_4") || c.QuotaImage != 0
	case toolMusic:
		// Kuota musik dipakai sebagai izin percobaan, BUKAN bukti kemampuan:
		// akun nova-ff2 melaporkan quota_music=4 tanpa cap MUSIC di daftar probe,
		// dan Google menjawab "3.8 Flash". Tapi web akun yang sama masih bisa
		// membuat musik, jadi cap yang tidak tercantum TIDAK boleh dipakai untuk
		// memblokir — probe bisa terpotong, dan memblokir akun yang sebenarnya
		// mampu jauh lebih mahal daripada satu percobaan yang gagal cepat.
		return c.QuotaMusic != 0
	case toolCanvas:
		return hasCap("CANVAS") || c.Tier == "pro" || len(c.Caps) == 0
	}
	switch mc.HexID {
	case hexArgon4:
		return hasRollout("Argon4p0")
	case hexFlash38:
		// Kandidat: akun dengan rollout 3.8/3.7 EKSPLISIT, atau akun berbayar
		// (punya Argon4p0) yang daftar rollout-nya terpotong.
		//
		// Kenapa perlu jalur kedua: probe babang2 hanya mengembalikan 907 byte
		// (vs 3.593 byte nova-ff2), jadi daftar rollout-nya tidak lengkap - hanya
		// "Argon4p0:3" - padahal uji langsung membuktikan babang2 MENYAJIKAN
		// "3.8 Flash". Filter yang menuntut Flash3p8 eksplisit membuat 3.7/3.8
		// terkunci ke nova-ff2 saja. Padahal nova-ff2 justru akun TERLAMBAT
		// (median 90,6s vs babang2 21,1s; untuk 3.8-thinking 148,4s vs 19,7s).
		// Gejalanya: "3.7-flash lambat sekali" - bukan Google-nya, tapi kita
		// memaksa satu akun lambat menangani semuanya.
		//
		// acc1 tidak ikut lolos: dia tidak punya Argon4p0 dan tidak punya
		// Flash3p8, jadi tetap tersaring (dan memang hanya menyajikan 3.6).
		return hasRollout("Flash3p8") || hasRollout("Flash3p7") || hasRollout("Argon4p0")
	case hexFlash36:
		// 3.6 punya jebakan sendiri: akun yang JUGA punya rollout Flash3p8 akan
		// menyajikan 3.8 saat diminta 3.6 (Google menaikkan, bukan menurunkan).
		//
		// Dulu akun seperti itu justru DISARING dari 3.6 supaya tidak kena
		// penolakan. Akibatnya 3.6 (dan varian thinking-nya) mati sama sekali di
		// akun utama yang punya 3.8 — "needs a signed-in PAID account" padahal
		// akunnya pro. Sekarang 3.8/3.7 diterima karena lebih tinggi dari 3.6;
		// yang ditolak hanya yang lebih rendah (3.5 Flash-Lite), dan itu dijaga
		// di modelServedMatches.
		return hasRollout("Flash3p6") || hasRollout(hexFlash36) ||
			hasRollout("Flash3p8") || hasRollout("Flash3p7") ||
			hasRollout("Argon4p0") || len(c.Rollouts) == 0
	}
	return true
}

// supportsModelPaidOnly menjawab: akun ini TIDAK punya bukti rollout untuk model
// tersebut, tapi bukti lain menunjukkan dia berbayar dan model itu terdaftar di
// daftar modelnya (otAQ7b).
//
// Dipakai pemilih akun sebagai TINGKAT KEDUA — pilihan terakhir, hanya ketika
// tidak ada satu pun akun yang punya bukti rollout.
//
// Kenapa Argon butuh ini: tier "pro" sendiri berasal dari daftar model otAQ7b
// yang memuat Argon (lihat penentuan tier di atas) — sumber yang di komentar itu
// justru disebut lebih jujur daripada ESY5D. Tapi supportsModel(hexArgon4) hanya
// membaca daftar rollout ESY5D, dan payload ESY5D memang bisa terpotong (kasus
// babang2: 907 byte, daftar rollout-nya tidak lengkap walau modelnya disajikan).
// Akibatnya akun berbayar yang daftar modelnya memuat Argon tetap tersaring, dan
// Argon terkunci pada akun yang kebetulan flag rollout-nya ikut terkirim —
// gejalanya: "kenapa Argon cuma ada di dua akun, padahal sisanya pro?".
//
// Akun FREE tidak pernah lolos di sini: model ini selalu dijawab model lain oleh
// akun free, dan deteksi downgrade mengubahnya jadi 502 pasti.
//
// 2026-11-05: gerbang ini tadinya HANYA hexArgon4. Akibatnya 3.1 Pro dan
// 3.8/3.7 Flash tidak digatek sama sekali - akun free jatuh ke "usaha terakhir"
// di pemilih, dipilih, lalu diturunkan Google ke 3.6 Flash. Terukur di mesin ini:
// gemini-3.8-flash memakai akun non-pro dan berakhir 502 downgrade. Operator
// mengoreksi: Argon, 3.1 Pro, 3.7, 3.8 semuanya khusus akun pro.
func (c AccountCapability) supportsModelPaidOnly(mc ModelConfig) bool {
	switch mc.HexID {
	case hexArgon4, hexPro31, hexFlash38:
		return c.Tier == "pro"
	}
	return false
}

// probeAllAccounts memeriksa semua akun, menyimpan hasilnya di cache dan DB.
func probeAllAccounts() map[int64]AccountCapability {
	probeMu.Lock()
	defer probeMu.Unlock()

	out := map[int64]AccountCapability{}
	for _, a := range accountList() {
		c := mergeProbeWithPrevious(probeAccount(a), probeCache[a.ID])
		out[a.ID] = c
		saveProbeResult(c)
	}
	probeCache = out
	return out
}

// mergeProbeWithPrevious menjaga pengetahuan lama saat probe gagal.
//
// Dua kasus, dan keduanya pernah merusak routing tanpa gejala:
//
//  1. Gagal karena jalur keluar (proxy mati, DNS, timeout). Dulu hasilnya
//     ditulis apa adanya: alive=0, tier="", caps kosong untuk SEMUA akun.
//     Akibatnya aturan "pro dulu" mati total dan model berbayar jatuh ke
//     anonim — padahal cookie-nya sehat dan request biasa jalan.
//  2. Gagal karena sesi memang mati. alive=false itu benar, tapi tier dan caps
//     terakhir tetap berguna: begitu cookie diperbarui, routing langsung tahu
//     akun ini pro tanpa menunggu probe berikutnya.
func mergeProbeWithPrevious(cur, prev AccountCapability) AccountCapability {
	if prev.AccountID == 0 {
		return cur
	}
	if isTransportError(cur.Err) {
		// Bukan salah sesi: pertahankan seluruh hasil terakhir, cukup perbarui
		// keterangan errornya supaya panel tetap jujur.
		prev.Err = cur.Err
		prev.CheckedAt = cur.CheckedAt
		return prev
	}
	if cur.Tier == "" && prev.Tier != "" {
		cur.Tier = prev.Tier
	}
	if len(cur.Caps) == 0 && len(prev.Caps) > 0 {
		cur.Caps = prev.Caps
	}
	if len(cur.Rollouts) == 0 && len(prev.Rollouts) > 0 {
		cur.Rollouts = prev.Rollouts
	}
	if cur.QuotaImage < 0 && prev.QuotaImage >= 0 {
		cur.QuotaImage = prev.QuotaImage
	}
	if cur.QuotaMusic < 0 && prev.QuotaMusic >= 0 {
		cur.QuotaMusic = prev.QuotaMusic
	}
	if cur.QuotaThink < 0 && prev.QuotaThink >= 0 {
		cur.QuotaThink = prev.QuotaThink
	}
	if cur.QuotaVideo < 0 && prev.QuotaVideo >= 0 {
		cur.QuotaVideo = prev.QuotaVideo
	}
	return cur
}

// probeSnapshot mengembalikan hasil probe terakhir tanpa memanggil Google.
func probeSnapshot() map[int64]AccountCapability {
	probeMu.Lock()
	defer probeMu.Unlock()
	cp := map[int64]AccountCapability{}
	for k, v := range probeCache {
		cp[k] = v
	}
	return cp
}

// probeForAccount mengembalikan hasil probe satu akun (zero value kalau belum ada).
func probeForAccount(id int64) AccountCapability {
	probeMu.Lock()
	defer probeMu.Unlock()
	return probeCache[id]
}

// loadProbeCache memuat hasil probe terakhir dari DB supaya routing sudah pintar
// sejak detik pertama setelah restart, tanpa menunggu probe pertama selesai.
func loadProbeCache() {
	rows, err := getDB().Query(
		`SELECT account_id, tier, caps, rollouts, quota_image, quota_music, quota_think,
		        quota_video, alive, err, checked_at FROM account_caps`)
	if err != nil {
		return
	}
	defer rows.Close()
	probeMu.Lock()
	defer probeMu.Unlock()
	for rows.Next() {
		var c AccountCapability
		var caps, rollouts string
		var alive int
		if err := rows.Scan(&c.AccountID, &c.Tier, &caps, &rollouts, &c.QuotaImage,
			&c.QuotaMusic, &c.QuotaThink, &c.QuotaVideo, &alive, &c.Err, &c.CheckedAt); err != nil {
			continue
		}
		c.Alive = alive != 0
		c.Caps = splitNonEmpty(caps)
		c.Rollouts = splitNonEmpty(rollouts)
		probeCache[c.AccountID] = c
	}
}

func saveProbeResult(c AccountCapability) {
	_, err := getDB().Exec(
		upsert("account_caps",
			[]string{"account_id", "tier", "caps", "rollouts", "quota_image", "quota_music",
				"quota_think", "quota_video", "alive", "err", "checked_at"},
			[]string{"account_id"}),
		c.AccountID, c.Tier, strings.Join(c.Caps, ","), strings.Join(c.Rollouts, ","),
		c.QuotaImage, c.QuotaMusic, c.QuotaThink, c.QuotaVideo, boolToInt(c.Alive), c.Err, c.CheckedAt)
	if err != nil {
		logf("[probe] simpan hasil akun #%d gagal: %v", c.AccountID, err)
	}
}

func splitNonEmpty(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// capabilityJSON membantu handler admin mengirim hasil probe sebagai JSON.
func capabilityJSON(c AccountCapability) map[string]interface{} {
	return map[string]interface{}{
		"account_id":  c.AccountID,
		"label":       c.Label,
		"alive":       c.Alive,
		"tier":        c.Tier,
		"caps":        c.Caps,
		"rollouts":    c.Rollouts,
		"features":    c.Features,
		"quota_image": c.QuotaImage,
		"quota_music": c.QuotaMusic,
		"quota_think": c.QuotaThink,
		"quota_video": c.QuotaVideo,
		"checked_at":  c.CheckedAt,
		"error":       c.Err,
	}
}

func probeErr(err error) string {
	if err == nil {
		return "no SNlM0e"
	}
	return err.Error()
}
