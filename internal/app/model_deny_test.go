package app

import (
	"testing"
	"time"
)

// Cooldown per (akun, model) adalah satu-satunya bagian dari penanganan
// "upstream menjawab model lain" yang state-nya hidup di memori. Kalau logikanya
// salah, gejalanya halus: akun sehat berhenti dipakai untuk sebuah model, atau
// sebaliknya request yang sama terus mengulang perjalanan 10-20 detik ke Google.
func TestModelDenyCooldown(t *testing.T) {
	modelDenyMu.Lock()
	modelDeny = map[string]int64{}
	modelDenyMu.Unlock()
	t.Cleanup(func() {
		modelDenyMu.Lock()
		modelDeny = map[string]int64{}
		modelDenyMu.Unlock()
	})

	argon := Models["gemini-4-argon"]
	flash := Models["gemini-3.6-flash"]

	if modelDenied(7, argon) {
		t.Fatal("akun yang belum ditandai tidak boleh dianggap ditolak")
	}

	markModelDenied(7, argon)
	if !modelDenied(7, argon) {
		t.Error("setelah ditandai, akun+model harus dianggap ditolak")
	}
	// Cooldown harus per pasangan: akun lain dan model lain tidak terpengaruh.
	if modelDenied(8, argon) {
		t.Error("akun lain tidak boleh terkena cooldown akun 7")
	}
	if modelDenied(7, flash) {
		t.Error("model lain tidak boleh terkena cooldown Argon")
	}

	// ID akun 0 / kosong dipakai jalur anonim: jangan pernah ditandai.
	markModelDenied(0, argon)
	if modelDenied(0, argon) {
		t.Error("akun 0 (anonim) tidak boleh masuk peta cooldown")
	}

	// Kedaluwarsa: paksa waktu habis, lalu pastikan entri dibersihkan.
	modelDenyMu.Lock()
	modelDeny[modelDenyKey(7, argon)] = time.Now().Add(-time.Second).Unix()
	modelDenyMu.Unlock()
	if modelDenied(7, argon) {
		t.Error("entri yang sudah lewat waktu harus dianggap tidak ditolak")
	}
	modelDenyMu.Lock()
	_, masihAda := modelDeny[modelDenyKey(7, argon)]
	modelDenyMu.Unlock()
	if masihAda {
		t.Error("entri kedaluwarsa harus dihapus saat diperiksa, bukan menumpuk")
	}
}
