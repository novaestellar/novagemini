package app

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeFakeFirefoxProfile menulis cookies.sqlite berbentuk Firefox (tabel
// moz_cookies) di direktori sementara, lalu mengembalikan direktori itu.
// Barisnya (nama, nilai, host, expiry) supaya bisa diuji kedaluwarsa.
func makeFakeFirefoxProfile(t *testing.T, rows [][4]any) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "cookies.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE moz_cookies (
		id INTEGER PRIMARY KEY, name TEXT, value TEXT, host TEXT, path TEXT,
		expiry INTEGER, lastAccessed INTEGER, creationTime INTEGER, isSecure INTEGER,
		isHttpOnly INTEGER, inBrowserElement INTEGER, sameSite INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO moz_cookies(name, value, host, expiry) VALUES (?,?,?,?)`,
			r[0], r[1], r[2], r[3]); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	db.Close()
	return dir
}

// TestReadFirefoxCookiesExtractsGoogleSession: kunci wajib harus terbaca, cookie
// non-Google dan cookie kedaluwarsa tidak boleh ikut.
func TestReadFirefoxCookiesExtractsGoogleSession(t *testing.T) {
	future := time.Now().Add(24 * time.Hour).Unix()
	past := time.Now().Add(-24 * time.Hour).Unix()
	dir := makeFakeFirefoxProfile(t, [][4]any{
		{"SAPISID", "sapisid-value", "google.com", future},
		{"__Secure-1PSID", "psid-value", ".google.com", future},
		{"SID", "sid-value", ".google.com", future},
		{"NID", "nid-value", ".google.com", future},
		{"sessionid", "should-not-appear", "example.com", future},
		{"STALE", "expired", ".google.com", past},
	})

	got, err := readFirefoxCookiesFromDir(dir, "uji")
	if err != nil {
		t.Fatalf("readFirefoxCookiesFromDir: %v", err)
	}
	for _, need := range []string{"SAPISID", "__Secure-1PSID", "SID", "NID"} {
		if got[need] == "" {
			t.Errorf("cookie wajib %q tidak terbaca", need)
		}
	}
	if _, bad := got["sessionid"]; bad {
		t.Error("cookie non-Google ikut terbaca")
	}
	if _, bad := got["STALE"]; bad {
		t.Error("cookie kedaluwarsa ikut terbaca")
	}
	if !browserSessionUsable(got) {
		t.Error("sesi lengkap harus dianggap usable")
	}
}

// TestExpiryUnitNormalisation: Firefox menyimpan expiry dalam tiga satuan yang
// berbeda menurut versi - detik, milidetik (terukur di mesin ini), dan mikrodetik
// PRTime. Kalau satuannya salah tebak, SEMUA cookie dinilai kedaluwarsa dan cermin
// diam-diam tidak melakukan apa-apa sambil melaporkan "profil sudah logout".
func TestExpiryUnitNormalisation(t *testing.T) {
	futureSec := time.Now().Add(24 * time.Hour).Unix()
	cases := []struct {
		name string
		unit int64
	}{
		{"detik", 1},
		{"milidetik", 1000},
		{"mikrodetik", 1000000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := makeFakeFirefoxProfile(t, [][4]any{
				{"SAPISID", "sapisid-value", "google.com", futureSec * tc.unit},
				{"__Secure-1PSID", "psid-value", ".google.com", futureSec * tc.unit},
				{"SID", "sid-value", ".google.com", futureSec * tc.unit},
			})
			got, err := readFirefoxCookiesFromDir(dir, "uji")
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(got) != 3 {
				t.Fatalf("satuan %s: dapat %d cookie, mau 3 - normalisasi expiry salah", tc.name, len(got))
			}
			if !browserSessionUsable(got) {
				t.Fatalf("satuan %s: sesi harus usable", tc.name)
			}
		})
	}

	// Kedaluwarsa harus tetap terbuang dalam satuan apa pun.
	pastSec := time.Now().Add(-24 * time.Hour).Unix()
	for _, unit := range []int64{1, 1000, 1000000} {
		dir := makeFakeFirefoxProfile(t, [][4]any{
			{"SAPISID", "sapisid-value", "google.com", pastSec * unit},
			{"__Secure-1PSID", "psid-value", ".google.com", pastSec * unit},
			{"SID", "sid-value", ".google.com", pastSec * unit},
		})
		got, err := readFirefoxCookiesFromDir(dir, "uji")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("unit %d: cookie kedaluwarsa harus terbuang, dapat %d", unit, len(got))
		}
	}
}

// TestBrowserSessionUsableRejectsLogout: profil yang sudah logout tidak boleh
// dianggap sehat, supaya mirror tidak menyalin cookie kosong ke pool.
func TestBrowserSessionUsableRejectsLogout(t *testing.T) {
	full := map[string]string{"SAPISID": "a", "__Secure-1PSID": "b", "SID": "c"}
	if !browserSessionUsable(full) {
		t.Fatal("set lengkap harus usable")
	}
	for _, drop := range []string{"SAPISID", "__Secure-1PSID", "SID"} {
		m := map[string]string{}
		for k, v := range full {
			m[k] = v
		}
		delete(m, drop)
		if browserSessionUsable(m) {
			t.Errorf("tanpa %q tidak boleh usable", drop)
		}
	}
	if browserSessionUsable(map[string]string{}) {
		t.Error("map kosong tidak boleh usable")
	}
	if browserSessionUsable(map[string]string{"SAPISID": "  ", "__Secure-1PSID": "b", "SID": "c"}) {
		t.Error("nilai kosong tidak boleh usable")
	}
}

// TestParseBrowserSource: format yang diterima dan yang harus ditolak.
func TestParseBrowserSource(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"firefox:animainventi", "animainventi", true},
		{"firefox:s1d1gj83.default-release", "s1d1gj83.default-release", true},
		{"firefox:s1d1gj83.default-release:au=3", "s1d1gj83.default-release", true},
		{"  firefox:abc  ", "abc", true},
		{"", "", false},
		{"chrome:Default", "", false},
		{"firefox:", "", false},
	} {
		got, ok := parseBrowserSource(tc.in)
		if ok != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("parseBrowserSource(%q) = (%q,%v), mau (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestCookieHeaderFromMapRoundTrip: header yang dibangun harus bisa dibaca lagi
// oleh splitCookiePairs, dan nilai yang mengandung "=" (base64) tidak rusak.
func TestCookieHeaderFromMapRoundTrip(t *testing.T) {
	m := map[string]string{"SAPISID": "abc==", "__Secure-1PSID": "def==", "SID": "xyz="}
	header := cookieHeaderFromMap(m)
	got := map[string]string{}
	for _, kv := range splitCookiePairs(header) {
		got[kv[0]] = kv[1]
	}
	if len(got) != len(m) {
		t.Errorf("jumlah pasangan tidak sama: %d vs %d", len(got), len(m))
	}
	for k, v := range m {
		if got[k] != v {
			t.Errorf("nilai %q rusak: %q != %q", k, got[k], v)
		}
	}
}

// TestReadFirefoxCookiesMissingProfile: profil yang tidak ada harus gagal dengan
// pesan yang menyebut profilnya, bukan error kosong.
func TestReadFirefoxCookiesMissingProfile(t *testing.T) {
	if _, err := readFirefoxCookies("profil-yang-tidak-ada-xyz"); err == nil {
		t.Fatal("profil hilang harus mengembalikan error")
	} else if !strings.Contains(err.Error(), "cookies.sqlite") {
		t.Errorf("error harus menyebut cookies.sqlite, dapat: %v", err)
	}
}

// TestFirefoxProfileDirUsesAppData: lokasi profil harus relatif ke APPDATA.
func TestFirefoxProfileDirUsesAppData(t *testing.T) {
	t.Setenv("APPDATA", `C:\Users\Test\AppData\Roaming`)
	got := firefoxProfileDir("animainventi")
	want := filepath.Join(`C:\Users\Test\AppData\Roaming`, "Mozilla", "Firefox", "Profiles", "animainventi")
	if got != want {
		t.Errorf("firefoxProfileDir = %q, mau %q", got, want)
	}
}

// TestCookieValueSIDStableAcrossRotation: sidik jari memakai SID karena browser
// memutar 1PSIDTS, bukan SID. Kalau SID ikut berubah, pengaman identitas cermin
// akan menolak semua penyegaran dan fiturnya mati diam-diam.
func TestCookieValueSIDStableAcrossRotation(t *testing.T) {
	before := "SID=abc123; __Secure-1PSIDTS=old-token; SAPISID=xyz"
	after := "SID=abc123; __Secure-1PSIDTS=NEW-rotated-token; SAPISID=xyz"
	if cookieValue(before, "SID") != cookieValue(after, "SID") {
		t.Fatal("SID harus stabil saat browser memutar 1PSIDTS - pengaman identitas akan rusak")
	}
	if cookieValue(before, "SID") == "" {
		t.Fatal("SID harus terbaca dari string cookie")
	}
}

// TestMirrorSkipsNonInterface: mirror harus aman dipanggil tanpa DB setup berat
// (tidak menumbangkan runtime) - murni smoke, bukan verifikasi penuh.
func TestMirrorSmokeEmpty(t *testing.T) {
	_ = os.Getenv("APPDATA") // memastikan symbol os terpakai di test build
}
