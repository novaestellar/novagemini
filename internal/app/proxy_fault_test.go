package app

import (
	"strings"
	"testing"
)

// Kegagalan tingkat MODEL tidak boleh menaikkan fail_count proxy. Terukur
// 2026-11-06 14:33: brd-10 last_error berisi "upstream downgraded: requested
// gemini-music", dan karena itu dihitung sebagai kegagalan proxy, seluruh 16
// proxy berakhir fail>=5 sekaligus - lalu fallback_direct=false membuat setiap
// permintaan ditolak 429 sebelum sempat keluar. Layanan mati total.
//
// Yang benar: hanya kegagalan TRANSPORT (dial, refused, reset, timeout, TLS,
// auth proxy) yang dihitung. Aturan yang sama dengan markCookieByStatus.
func TestIsProxyFaultOnlyTransport(t *testing.T) {
	transport := []string{
		`Post "https://gemini.google.com": proxyconnect tcp: dial tcp 127.0.0.1:40000: connectex: No connection could be made`,
		`dial tcp 1.2.3.4:443: i/o timeout`,
		`dial tcp: lookup brd.superproxy.io: no such host`,
		`proxyconnect tcp: connection refused`,
		`read tcp: connection reset by peer`,
		`TLS handshake timeout`,
		`proxy authentication required (407)`,
		`proxyconnect tcp: unexpected EOF`,
	}
	for _, e := range transport {
		if !isProxyFault(e) {
			t.Errorf("kegagalan transport harus dihitung sebagai kesalahan proxy: %q", e)
		}
	}

	// Ini jawaban model / klien / kuota - BUKAN kesalahan proxy.
	modelLevel := []string{
		`upstream downgraded: requested gemini-music but Google served "3.8 Flash"`,
		`upstream returned a canned refusal instead of an answer (model gemini-3.6-flash)`,
		`upstream HTTP 429`,
		`limit resets at 12:00`,
		`upstream returned no content frame (raw 663 bytes)`,
		`context canceled`,
		`Post "https://gemini.google.com/...": context canceled`,
		`all proxy slots full: concurrent limit reached`,
		``,
	}
	for _, e := range modelLevel {
		if isProxyFault(e) {
			t.Errorf("kegagalan tingkat model TIDAK boleh dihitung sebagai kesalahan proxy: %q", e)
		}
	}
}

// Pesan asli dari brd-10 harus lewat bersih - ini bukti nyata dari insiden.
func TestIsProxyFaultRealIncidentString(t *testing.T) {
	real := `upstream downgraded: requested gemini-music but Google served "3.8 Flash" (this account has no access to that model)`
	if isProxyFault(real) {
		t.Error("pesan insiden nyata dari brd-10 tidak boleh dianggap kesalahan proxy")
	}
}

// Cek kapital tidak boleh mengubah hasil.
func TestIsProxyFaultCaseInsensitive(t *testing.T) {
	if !isProxyFault("ProxyConnect TCP: DIAL TCP") {
		t.Error("pencocokan harus tidak peka huruf besar-kecil")
	}
	if isProxyFault("UPSTREAM DOWNGRADED") {
		t.Error("huruf besar pada error model tidak boleh membuatnya jadi kesalahan proxy")
	}
}

var _ = strings.ToLower // jaga import kalau tes lain di berkas ini tidak memakai strings
