package app

import "testing"

// Regresi: cooldown model dulu berkunci (akun|hex|tool) saja, sehingga
// `gemini-3.6-flash` dan `gemini-3.6-flash-thinking` berbagi kunci. Begitu
// Google menyajikan model lain untuk yang biasa, varian thinking ikut diblokir
// dan permintaannya jatuh ke jalur anonim — muncul "needs a signed-in PAID
// account" padahal akunnya pro.
func TestModelDenyKeySeparatesVariants(t *testing.T) {
	plain := Models["gemini-3.6-flash"]
	thinking := Models["gemini-3.6-flash-thinking"]

	if plain.HexID != thinking.HexID {
		t.Fatalf("asumsi tes tidak berlaku: hex berbeda (%s vs %s)", plain.HexID, thinking.HexID)
	}
	if modelDenyKey(6, plain) == modelDenyKey(6, thinking) {
		t.Fatal("varian thinking berbagi kunci cooldown dengan varian biasa")
	}

	// Cooldown untuk varian biasa tidak boleh menyaring varian thinking.
	markModelDenied(6, plain)
	t.Cleanup(func() {
		modelDenyMu.Lock()
		modelDeny = map[string]int64{}
		modelDenyMu.Unlock()
	})
	if !modelDenied(6, plain) {
		t.Fatal("model yang ditandai tidak terbaca sebagai cooldown")
	}
	if modelDenied(6, thinking) {
		t.Fatal("cooldown varian biasa menjalar ke varian thinking")
	}

	// Akun lain tetap terpisah.
	if modelDenied(7, plain) {
		t.Fatal("cooldown menjalar ke akun lain")
	}
}

// Argon punya beberapa tingkat penalaran dengan hex yang sama; masing-masing
// harus bisa di-cooldown sendiri.
func TestModelDenyKeySeparatesThinkLevels(t *testing.T) {
	base := Models["gemini-4-argon"]
	high := Models["gemini-4-argon-high"]
	if base.ThinkLevel == high.ThinkLevel {
		t.Fatalf("asumsi tes tidak berlaku: think level sama (%d)", base.ThinkLevel)
	}
	if modelDenyKey(6, base) == modelDenyKey(6, high) {
		t.Fatal("argon biasa dan argon-high berbagi kunci cooldown")
	}
}
