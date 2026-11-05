package app

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// browserMirrorInterval determines how often the server re-reads the cookie of
// accounts whose session belongs to a browser.
//
// This is a READ, not a rotation. Firefox keeps its cookies unencrypted in
// cookies.sqlite, so the browser is the single owner of the session and the
// server only copies what the browser already produced. Nothing here rotates
// __Secure-1PSIDTS, so the "two owners, one session" death described on
// CookieAccount.RotateDisabled cannot happen.
//
// Why it exists: a browser-owned account only had its copy refreshed by a
// manual `cookie_grab.py sync`, so between syncs the server kept using stale
// 1PSIDTS tokens the browser had already rotated away. That is one of the
// sources of the intermittent "Argon works, then it doesn't" behaviour.
const browserMirrorInterval = 20 * time.Minute

// browserSourcePrefix marks a cookie_source value as coming from a local
// Firefox profile, e.g. "firefox:animainventi".
const browserSourcePrefix = "firefox:"

// parseBrowserSource accepts "firefox:<profile>" (an optional ":au=N" suffix is
// tolerated and ignored - auth_user lives in its own column).
func parseBrowserSource(src string) (profile string, ok bool) {
	src = strings.TrimSpace(src)
	if src == "" || !strings.HasPrefix(src, browserSourcePrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(src, browserSourcePrefix)
	if i := strings.Index(rest, ":"); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	return rest, true
}

// firefoxProfileDir returns the cookies.sqlite directory of a named profile.
func firefoxProfileDir(profile string) string {
	base := os.Getenv("APPDATA")
	if base == "" {
		return ""
	}
	return filepath.Join(base, "Mozilla", "Firefox", "Profiles", profile)
}

// readFirefoxCookies membaca cookie Google dari sebuah direktori profil.
func readFirefoxCookies(profile string) (map[string]string, error) {
	dir := firefoxProfileDir(profile)
	if dir == "" {
		return nil, fmt.Errorf("APPDATA not set")
	}
	return readFirefoxCookiesFromDir(dir, profile)
}

// readFirefoxCookiesFromDir snapshots a profile's cookie store and returns the
// Google cookies it holds.
//
// The snapshot matters: Firefox holds cookies.sqlite open in WAL mode, so
// reading the live file can miss the newest commits or fail outright. Copying
// the database and its WAL first makes the read independent of whether the
// browser is running - and never touches the profile itself.
//
// The -shm file is deliberately NOT copied: it is a rebuildable shared-memory
// index that Firefox rewrites constantly, and a stale copy can make SQLite see
// a WAL that does not match - the read then silently returns zero rows. Without
// it SQLite rebuilds the index from the WAL, which is both correct and race-free.
func readFirefoxCookiesFromDir(dir, profile string) (map[string]string, error) {
	src := filepath.Join(dir, "cookies.sqlite")
	if _, err := os.Stat(src); err != nil {
		return nil, fmt.Errorf("profile %q has no cookies.sqlite: %v", profile, err)
	}

	tmp, err := os.MkdirTemp("", "nmcookies-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(src + suffix)
		if err != nil {
			if suffix == "" {
				return nil, err
			}
			continue // no WAL means everything is already checkpointed
		}
		if err := os.WriteFile(filepath.Join(tmp, "cookies.sqlite"+suffix), data, 0o600); err != nil {
			return nil, err
		}
	}

	db, err := sql.Open("sqlite", filepath.Join(tmp, "cookies.sqlite"))
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT name, value, expiry, host FROM moz_cookies
		WHERE host LIKE '%google.com' OR host LIKE '%google.co.id'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().Unix()
	out := map[string]string{}
	rank := map[string]int{}
	for rows.Next() {
		var name, value, host string
		var expiry int64
		if err := rows.Scan(&name, &value, &expiry, &host); err != nil {
			continue
		}
		if name == "" || value == "" {
			continue
		}
		// Firefox has used three different units for this column depending on
		// version: seconds, PRTime microseconds, and (measured on this machine)
		// milliseconds. Getting the unit wrong silently discards every cookie -
		// the mirror would then log "profile logged out" forever while the
		// browser is perfectly signed in. Pick the divisor by magnitude.
		switch {
		case expiry > 1e14: // microseconds (PRTime)
			expiry = expiry / 1e6
		case expiry > 1e11: // milliseconds
			expiry = expiry / 1e3
		}
		if expiry > 0 && expiry < now {
			continue // expired
		}
		// Satu nama cookie bisa muncul di beberapa host (profil yang pernah login
		// di dua region menyimpan dua set lengkap). Yang dipakai adalah host
		// berperingkat terendah - .google.com menang atas .google.co.id, karena
		// sesi Gemini selalu di .google.com dan memilih domain negara menghasilkan
		// halaman "Sign in" walau akunnya aktif. Sama seperti cookie_grab.py.
		r, ok := cookieHostRank(host)
		if !ok {
			r = 5
		}
		if cur, seen := rank[name]; seen && cur <= r {
			continue
		}
		rank[name] = r
		out[name] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// cookieHostRank mirrors COOKIE_NAME_RANK in tools/cookie_grab.py. A profile
// that has signed in during two different regions holds two complete cookie
// sets, and picking the country domain produces a "Sign in" page even though
// the account is plainly active.
func cookieHostRank(host string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "gemini.google.com":
		return -1, true
	case ".google.com", "google.com":
		return 0, true
	case "accounts.google.com":
		return 1, true
	case ".google.co.id", "google.co.id":
		return 3, true
	}
	return 5, false
}

// cookieHeaderFromMap rebuilds a "k=v; k=v" header from a cookie map.
func cookieHeaderFromMap(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

// browserSessionUsable answers whether a harvested cookie set is a signed-in
// Gemini session. The three names are the same set cookie_grab.py requires.
func browserSessionUsable(m map[string]string) bool {
	for _, need := range []string{"SAPISID", "__Secure-1PSID", "SID"} {
		if strings.TrimSpace(m[need]) == "" {
			return false
		}
	}
	return true
}

// mirrorBrowserOwnedAccounts refreshes the server's copy of every
// browser-owned account from its bound Firefox profile.
//
// Deliberately conservative: an account is only touched when the profile holds
// a usable session AND the header actually differs from what we store. A
// profile that is signed out is reported and left alone - the 15-minute
// capability probe already disables an account whose session died, and
// disabling here would fight the operator who is about to log back in.
func mirrorBrowserOwnedAccounts() int {
	refreshed := 0
	for _, a := range accountList() {
		if a.Status != "enabled" || a.RotateDisabled == 0 {
			continue
		}
		profile, ok := parseBrowserSource(a.CookieSource)
		if !ok {
			continue
		}
		cookies, err := readFirefoxCookies(profile)
		if err != nil {
			logf("[mirror] akun #%d (%s): profil tidak terbaca: %v", a.ID, a.Label, err)
			continue
		}
		if !browserSessionUsable(cookies) {
			logf("[mirror] akun #%d (%s): profil %q sudah logout - biarkan probe 15 menit yang menandainya",
				a.ID, a.Label, profile)
			continue
		}
		// Pengaman identitas: SID tidak diputar browser (yang berubah hanya
		// 1PSIDTS), jadi kalau cookie_source keliru menunjuk profil akun lain,
		// SID-nya tidak akan cocok dan penimpaan dibatalkan. Tanpa ini satu
		// salah ketik bisa membuat akun dilayani sebagai Google account orang lain.
		if a.CookieSID != "" && cookies["SID"] != a.CookieSID {
			logf("[mirror] akun #%d (%s): SID profil %q TIDAK cocok - "+
				"cookie_source keliru, penimpaan dibatalkan", a.ID, a.Label, profile)
			continue
		}
		header := cookieHeaderFromMap(cookies)
		if header == strings.TrimSpace(a.Cookie) {
			continue // nothing new from the browser
		}
		if err := accountSetCookie(a.ID, header); err != nil {
			logf("[mirror] akun #%d: gagal menulis cookie: %v", a.ID, err)
			continue
		}
		logf("[mirror] akun #%d (%s): cookie disegarkan dari profil %q (%d cookie)",
			a.ID, a.Label, profile, len(cookies))
		refreshed++
	}
	return refreshed
}
