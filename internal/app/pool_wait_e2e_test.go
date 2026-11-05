package app

import (
	"strings"
	"testing"
	"time"
)

// Menguji cabang yang dulu bikin operator melihat 400 "context_length_exceeded"
// padahal prompt-nya sah dan hanya pool-nya sedang cooldown. Deterministik:
// deny map di-seed langsung, jadi tidak ada jaringan yang disentuh.
func TestPoolUnavailableWhenAllAccountsCoolingDown(t *testing.T) {
	withTestDB(t)
	acct := insertTestAccount(t, "pool-wait-acct")
	argon := Models["gemini-4-argon"]

	modelDenyMu.Lock()
	modelDeny = map[string]int64{}
	modelDeny[modelDenyKey(acct, argon)] = time.Now().Unix() + 300 // jauh melebihi budget
	modelDenyMu.Unlock()
	t.Cleanup(func() {
		modelDenyMu.Lock()
		modelDeny = map[string]int64{}
		modelDenyMu.Unlock()
	})

	old := rtCfg()
	next := old
	next.MaxPromptBytes = 1000
	next.DenyWaitBudgetSec = 10
	rtMu.Lock()
	rtVal = next
	rtMu.Unlock()
	t.Cleanup(func() {
		rtMu.Lock()
		rtVal = old
		rtMu.Unlock()
	})

	big := strings.Repeat("x", 2000)
	_, err := streamGenerateWithFiles(nil, big, "terakhir", argon, nil, nil, nil)
	if err == nil {
		t.Fatal("harusnya ditolak, bukan dikirim ke jalur anonim")
	}
	pue, isPUE := err.(*PoolUnavailableError)
	if !isPUE {
		t.Fatalf("harusnya PoolUnavailableError (503, retryable), dapat %T: %v", err, err)
	}
	if pue.Enabled != 1 {
		t.Fatalf("harusnya melaporkan 1 akun enabled, dapat %d", pue.Enabled)
	}
	if pue.RetryAfterSec <= 0 || pue.RetryAfterSec > 301 {
		t.Fatalf("RetryAfterSec tidak masuk akal: %d", pue.RetryAfterSec)
	}
}

// Kalau cooldown terdekat tinggal sebentar (di bawah budget), jangan menyerah:
// akun yang sama harus dicoba ulang setelah menunggu. Tidak bisa diuji tanpa
// jaringan untuk kaki terakhirnya, tapi keputusan tunggu-vs-serah sendiri
// bisa: budget nol berarti modelDenySoonest tak dipakai untuk menunggu, jadi
// cukup uji denyWaitBudget() yang sudah diliput di atas.
func TestPoolUnavailableMessageStatesRecoveryNotShorten(t *testing.T) {
	err := error(&PoolUnavailableError{
		Model: "4 Argon", Enabled: 10, PromptBytes: 136261, RetryAfterSec: 45,
		Detail: "no signed-in account is free for 4 Argon right now: the prompt is 136261 bytes so the anonymous path cannot carry it as an attachment, and every enabled account is cooling down after Google served a different model for it. This clears by itself - the nearest account frees up in about 45s; the same request will go through then.",
	})
	for _, want := range []string{"cooling down", "clears by itself"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("pesan harus menyebut %q: %v", want, err)
		}
	}
	for _, bad := range []string{"shorten the prompt", "context_length_exceeded"} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("pesan tidak boleh lagi menuduh prompt: %q ada di %v", bad, err)
		}
	}
}
