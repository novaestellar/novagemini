package app

import (
	"testing"
	"time"
)

// Cooldown seluruh pool pernah membuat permintaan prompt besar gagal instan
// selama lima menit penuh. modelDenySoonest adalah dasar keputusan "tunggu
// sebentar" versus "menyerah", jadi dua hal harus benar:
//   - hanya pasangan (akun, model) yang cocok yang dihitung, bukan varian lain
//     dalam keluarga yang sama (pelajaran dari bug kunci deny sebelumnya);
//   - yang dilaporkan adalah sisa cooldown TERDEKAT, bukan yang terjauh.
func TestModelDenySoonestPicksNearestAndMatchesOnlySameVariant(t *testing.T) {
	argon := Models["gemini-4-argon"]
	flash := Models["gemini-3.6-flash"]

	modelDenyMu.Lock()
	modelDeny = map[string]int64{}
	now := time.Now().Unix()
	modelDeny["7"+modelDenyKey(7, argon)[len("7"):]] = now + 300
	modelDeny["9"+modelDenyKey(9, argon)[len("9"):]] = now + 4
	modelDeny["8"+modelDenyKey(8, flash)[len("8"):]] = now + 1
	modelDenyMu.Unlock()
	t.Cleanup(func() {
		modelDenyMu.Lock()
		modelDeny = map[string]int64{}
		modelDenyMu.Unlock()
	})

	d, ok := modelDenySoonest(argon)
	if !ok {
		t.Fatal("harusnya ada cooldown Argon yang terdeteksi")
	}
	if d > 6*time.Second {
		t.Fatalf("harusnya sisa cooldown terdekat (~4s), dapat %s", d)
	}
	if d < 2*time.Second {
		t.Fatalf("sisa cooldown terdekat tidak masuk akal: %s", d)
	}

	// Model lain tidak boleh ikut terhitung.
	if _, ok := modelDenySoonest(Models["gemini-3.1-pro"]); ok {
		t.Fatal("model lain tidak boleh ikut terhitung ke dalam cooldown Argon")
	}
}

// Pool yang cooldown harus dipetakan ke 503, bukan 400: 400 dibaca klien
// sebagai "promptmu salah, jangan diulang", padahal request yang sama berhasil
// setelah cooldown lewat.
func TestPoolUnavailableIsRetryableNotBadRequest(t *testing.T) {
	err := &PoolUnavailableError{Model: "4 Argon", Enabled: 10, RetryAfterSec: 12}
	if got := httpStatusForError(err); got != 503 {
		t.Fatalf("PoolUnavailableError harus 503, dapat %d", got)
	}
	if got := httpStatusForError(&PromptTooLongError{Bytes: 200000, Budget: 128000}); got != 400 {
		t.Fatalf("PromptTooLongError harus tetap 400, dapat %d", got)
	}
	msg := err.Error()
	if msg == "" {
		t.Fatal("pesan galat tidak boleh kosong")
	}
}

// Batas tunggu mengikuti konfigurasi runtime supaya operator bisa mematikannya
// (0 = tolak instan seperti perilaku lama).
func TestDenyWaitBudgetFollowsRuntimeConfig(t *testing.T) {
	rtMu.Lock()
	prev := rtVal.DenyWaitBudgetSec
	rtMu.Unlock()
	t.Cleanup(func() {
		rtMu.Lock()
		rtVal.DenyWaitBudgetSec = prev
		rtMu.Unlock()
	})

	setDenyWaitForTest(7)
	if got := denyWaitBudget(); got != 7*time.Second {
		t.Fatalf("harusnya 7s, dapat %s", got)
	}
	setDenyWaitForTest(0)
	if got := denyWaitBudget(); got != 0 {
		t.Fatalf("0 harus berarti tidak menunggu, dapat %s", got)
	}
}

func setDenyWaitForTest(sec int) {
	rtMu.Lock()
	rtVal.DenyWaitBudgetSec = sec
	rtMu.Unlock()
}
