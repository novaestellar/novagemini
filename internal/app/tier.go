package app

// Deteksi tier yang benar-benar dipakai Google.
//
// Probe ESY5D hanya mengirim daftar "capability" yang kadang tidak memuat tanda
// berbayar walau langganannya aktif — terukur pada tiga akun Pro operator
// (beberapa akun berbayar): daftar ESY5D-nya hanya berisi JASMINE.
//
// Sinyal yang lebih jujur ada di daftar model per akun (rpcids=otAQ7b). Akun
// berbayar menerima 32 model termasuk Argon; akun gratis 23 model tanpa Argon.
// Selisihnya stabil dan bisa diperiksa langsung, jadi tier ditentukan dari situ,
// bukan dari tebakan atas nama capability.

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// proModelHex adalah id model 4 Argon — hanya muncul untuk akun berbayar.
const proModelHex = "95a4c0abcba24364"

var modelHexRe = regexp.MustCompile(`"([0-9a-f]{16,20})"`)

// accountProModels mengambil daftar model milik satu akun lalu menilai apakah
// akun itu berbayar. Return kedua = jumlah model, untuk log/iagnostik.
func accountProModels(a CookieAccount, xsrf, proxyURL string) (bool, int, error) {
	if xsrf == "" {
		return false, 0, fmt.Errorf("tanpa XSRF token")
	}
	reqid := time.Now().UnixNano() % 1000000
	endpoint := fmt.Sprintf(
		"https://gemini.google.com/_/BardChatUi/data/batchexecute"+
			"?rpcids=otAQ7b&source-path=%%2Fapp&bl=%s&hl=en&_reqid=%d&rt=c",
		currentBL(proxyURL), reqid)
	endpoint = geminiURLWithAuthUser(endpoint, a.Cookie)

	form := url.Values{}
	form.Set("f.req", `[[["otAQ7b","[]",null,"generic"]]]`)
	form.Set("at", xsrf)

	headers := buildGeminiHeaders(a.Cookie, extractSAPISID(a.Cookie), "")
	delete(headers, "x-goog-ext-525001261-jspb")
	headers["x-goog-ext-525001261-jspb"] = "[0]"

	status, _, body, err := uploadPost(endpoint, headers, []byte(form.Encode()), proxyURL)
	if err != nil {
		return false, 0, err
	}
	if status != 200 {
		return false, 0, fmt.Errorf("otAQ7b HTTP %d", status)
	}
	raw := string(body)
	// Payload batchexecute membawa JSON yang di-escape (\" di dalam string), jadi
	// tanda kutipnya dibuka dulu sebelum memindai id model. Tanpa ini pemindaian
	// hanya menemukan satu id dan semua akun terlihat "gratis".
	raw = strings.ReplaceAll(raw, `\"`, `"`)
	hexes := modelHexRe.FindAllStringSubmatch(raw, -1)
	seen := map[string]bool{}
	for _, m := range hexes {
		seen[m[1]] = true
	}
	if len(seen) == 0 {
		return false, 0, fmt.Errorf("daftar model kosong")
	}
	pro := seen[proModelHex] || strings.Contains(raw, `"Argon"`)
	return pro, len(seen), nil
}
