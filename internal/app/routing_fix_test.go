package app

import (
	"testing"
	"time"
)

// Proxy yang dimatikan tidak boleh dianggap "ada proxy". Dulu pool berisi satu
// proxy enabled=0 membuat setiap request masuk jalur proxy, gagal memilih, lalu
// dilaporkan "rph limit reached" â€” padahal penghitungnya nol dan jalur langsung
// sehat. Gejalanya: 502 instan pada model berbayar.
func TestAcquireSlotIgnoresDisabledProxy(t *testing.T) {
	proxyMu.Lock()
	old := proxyCache
	proxyCache = []Proxy{{ID: 1, Name: "warp-off", URL: "http://127.0.0.1:40000", Enabled: false}}
	proxyMu.Unlock()
	t.Cleanup(func() {
		proxyMu.Lock()
		proxyCache = old
		proxyMu.Unlock()
		releaseSlot(0)
	})

	p, ok, err := acquireSlot(0)
	if err != nil {
		t.Fatalf("proxy mati membuat request ditolak: %v", err)
	}
	if !ok {
		t.Fatal("slot langsung tidak diberikan")
	}
	if p.ID != 0 {
		t.Errorf("dipakai proxy #%d padahal semuanya dimatikan", p.ID)
	}
	releaseSlot(0)

	// Kontrol: proxy yang AKTIF tetap dipakai.
	proxyMu.Lock()
	proxyCache = []Proxy{{ID: 1, Name: "warp-on", URL: "http://127.0.0.1:40000", Enabled: true}}
	proxyMu.Unlock()
	if p2, ok2, err2 := acquireSlot(0); err2 != nil {
		t.Fatalf("proxy aktif tidak terpakai: %v", err2)
	} else if !ok2 || p2.ID != 1 {
		t.Errorf("proxy aktif tidak dipilih: ok=%v id=%d", ok2, p2.ID)
	} else {
		releaseSlot(1)
	}
}

// 3.6 yang dilayani 3.8 bukan penurunan kelas: 3.8 lebih tinggi dan konteksnya
// tidak mengecil. Yang harus ditolak adalah penurunan nyata ke 3.5 Flash-Lite.
func TestModelServedMatchesAcceptsUpgrade(t *testing.T) {
	m36 := Models["gemini-3.6-flash"]
	cases := []struct {
		served string
		want   bool
	}{
		{"3.6 Flash", true},
		{"3.6 Flash High", true},
		{"3.8 Flash", true},
		{"3.7 Flash", true},
		{"3.5 Flash-Lite", false},
		{"4 Argon", false},
	}
	for _, c := range cases {
		if got := modelServedMatches(m36, c.served); got != c.want {
			t.Errorf("3.6 vs %q: got %v want %v", c.served, got, c.want)
		}
	}

	m38 := Models["gemini-3.8-flash"]
	if !modelServedMatches(m38, "3.8 Flash") {
		t.Error("3.8 harus cocok dengan 3.8 Flash")
	}
	if modelServedMatches(m38, "3.5 Flash-Lite") {
		t.Error("3.8 tidak boleh menerima Flash-Lite")
	}

	argon := Models["gemini-4-argon"]
	if !modelServedMatches(argon, "4 Argon") || modelServedMatches(argon, "3.8 Flash") {
		t.Error("pencocokan Argon salah")
	}
}

// Akun yang punya rollout 3.8 tidak boleh disaring dari permintaan 3.6 lagi â€”
// itu yang membuat 3.6 dan 3.6-thinking mati di akun utama.
func TestSupportsModelFlash36OnNewerAccount(t *testing.T) {
	c := AccountCapability{
		AccountID: 6, Tier: "pro", Alive: true,
		Rollouts: []string{"Flash3p6PaidV2Rollout:3", "Flash3p8:1", "Argon4p0:2"},
	}
	if !c.supportsModel(Models["gemini-3.6-flash"]) {
		t.Error("akun dengan rollout 3.8 disaring dari 3.6")
	}
	if !c.supportsModel(Models["gemini-3.6-flash-thinking"]) {
		t.Error("akun dengan rollout 3.8 disaring dari 3.6-thinking")
	}

	// Akun tanpa rollout apa pun tetap dianggap belum diprobe -> tidak disaring.
	empty := AccountCapability{AccountID: 9, Alive: true}
	if !empty.supportsModel(Models["gemini-3.6-flash"]) {
		t.Error("akun tanpa data rollout disaring")
	}
}

// Cooldown model harus kedaluwarsa sendiri.
func TestModelDenyExpires(t *testing.T) {
	mc := Models["gemini-3.6-flash"]
	markModelDenied(6, mc)
	t.Cleanup(func() {
		modelDenyMu.Lock()
		modelDeny = map[string]int64{}
		modelDenyMu.Unlock()
	})
	if !modelDenied(6, mc) {
		t.Fatal("cooldown tidak aktif")
	}
	// Majukan jam: entri kedaluwarsa harus dihapus saat dibaca.
	modelDenyMu.Lock()
	modelDeny[modelDenyKey(6, mc)] = time.Now().Add(-time.Second).Unix()
	modelDenyMu.Unlock()
	if modelDenied(6, mc) {
		t.Fatal("cooldown kedaluwarsa masih dianggap aktif")
	}
}
