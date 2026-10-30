package app

// Satu profil browser bisa memegang banyak akun Google sekaligus. Semua akun itu
// berbagi SATU set cookie — password, 2FA, dan sesi tetap satu — dan yang
// membedakan hanya nomor urut akun: `authuser` di query string dan
// `X-Goog-AuthUser` di header. Tanpa nomor itu, request selalu dilayani akun
// pertama di profil tersebut.
//
// Konsekuensi praktisnya besar: sepuluh akun di satu profil cukup satu kali login
// manual per akun, dan seluruh pipeline (probe, chat, media) bekerja untuk
// masing-masing hanya dengan mengganti angka ini.

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

type authUserEntry struct {
	n  int64
	at time.Time
}

// authUserCache menyimpan pemetaan cookie → nomor akun. Kuncinya cookie utuh
// karena hampir semua jalur request hanya membawa string cookie, bukan id akun.
// TTL pendek supaya perubahan di panel langsung terpakai tanpa restart.
var authUserCache sync.Map

const authUserCacheTTL = 30 * time.Second

// accountSetAuthUser menyimpan nomor akun untuk satu baris accounts.
//
// Nomor itu juga dituliskan sebagai penanda di ujung cookie
// (`; __NOVA_AU=n`). Alasannya praktis: hampir semua jalur request di kode ini
// hanya membawa string cookie, bukan id akun, dan dua akun dari profil browser
// yang sama punya cookie yang PERSIS sama — pencarian berdasarkan cookie jadi
// ambigu dan akun kedua selalu dilayani sebagai akun pertama. Penanda membuat
// tiap baris unik, dan nilai asing di Cookie diabaikan Google.
func accountSetAuthUser(id, n int64) error {
	if n < 0 {
		n = 0
	}
	var cookie string
	if err := getDB().QueryRow(`SELECT cookie FROM accounts WHERE id=?`, id).Scan(&cookie); err == nil {
		cookie = withAuthUserMarker(cookie, n)
		if _, err := getDB().Exec(`UPDATE accounts SET auth_user=?, cookie=? WHERE id=?`, n, cookie, id); err != nil {
			return err
		}
		invalidateAuthUserCache()
		invalidateXSRF(cookie)
		return nil
	}
	if _, err := getDB().Exec(`UPDATE accounts SET auth_user=? WHERE id=?`, n, id); err != nil {
		return err
	}
	invalidateAuthUserCache()
	return nil
}

const authUserMarker = "__NOVA_AU"

// parseAuthUserMarker membaca nomor akun yang dititipkan di ujung cookie.
func parseAuthUserMarker(cookie string) (int64, bool) {
	for _, kv := range strings.Split(cookie, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || name != authUserMarker {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// stripAuthUserMarker membuang penanda sebelum cookie dikirim ke Google.
func stripAuthUserMarker(cookie string) string {
	if !strings.Contains(cookie, authUserMarker+"=") {
		return cookie
	}
	parts := strings.Split(cookie, ";")
	out := parts[:0]
	for _, kv := range parts {
		if strings.HasPrefix(strings.TrimSpace(kv), authUserMarker+"=") {
			continue
		}
		out = append(out, kv)
	}
	return strings.Join(out, "; ")
}

// withAuthUserMarker mengganti penanda lama dengan nilai baru.
func withAuthUserMarker(cookie string, n int64) string {
	base := stripAuthUserMarker(cookie)
	if n <= 0 {
		return base
	}
	return base + "; " + authUserMarker + "=" + strconv.FormatInt(n, 10)
}

func invalidateAuthUserCache() {
	authUserCache.Range(func(k, _ any) bool {
		authUserCache.Delete(k)
		return true
	})
}

// authUserForCookie mengembalikan nomor akun yang terdaftar dengan cookie ini.
// Cookie yang tidak terdaftar (jalur anonim) selalu 0.
func authUserForCookie(cookie string) int64 {
	if strings.TrimSpace(cookie) == "" {
		return 0
	}
	if n, ok := parseAuthUserMarker(cookie); ok {
		return n
	}
	if v, ok := authUserCache.Load(cookie); ok {
		if e, ok := v.(authUserEntry); ok && time.Since(e.at) < authUserCacheTTL {
			return e.n
		}
	}
	var n int64
	db := getDB()
	if db != nil {
		_ = db.QueryRow(`SELECT auth_user FROM accounts WHERE cookie=?`, cookie).Scan(&n)
	}
	authUserCache.Store(cookie, authUserEntry{n: n, at: time.Now()})
	return n
}

// appPageURL mengembalikan URL halaman Gemini untuk akun yang dituju.
//
// Google memilih akun di halaman lewat PATH, bukan query: `/u/3/app`. Memakai
// `?authuser=3` dijawab 302 ke bentuk path itu, dan karena klien kita tidak
// selalu mengikuti redirect, hasilnya "fetch /app: HTTP 302" — akun sehat
// terlihat mati.
func appPageURL(cookie string) string {
	n := authUserForCookie(cookie)
	if n <= 0 {
		return "https://gemini.google.com/app"
	}
	return "https://gemini.google.com/u/" + strconv.FormatInt(n, 10) + "/app"
}

// geminiURLWithAuthUser menambahkan `authuser` ke endpoint Gemini kalau akun ini
// memang bukan akun indeks 0. Endpoint yang sudah punya query string disambung
// dengan `&`, bukan `?`.
func geminiURLWithAuthUser(endpoint, cookie string) string {
	endpoint = stripAuthUserParam(endpoint)
	n := authUserForCookie(cookie)
	if n <= 0 {
		return endpoint
	}
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	return endpoint + sep + "authuser=" + strconv.FormatInt(n, 10)
}

// stripAuthUserParam membuang `authuser=` yang sudah ada supaya pemanggilan
// berulang (retry, refresh XSRF) tidak menumpuk parameter yang sama.
func stripAuthUserParam(endpoint string) string {
	parts := strings.Split(endpoint, "&")
	out := parts[:0]
	for _, p := range parts {
		if strings.HasPrefix(p, "authuser=") {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "&")
}
