package app

import (
	"testing"
	"time"
)

// Bukti dari riwayat harus ditanam ulang saat start, kalau tidak akun berbayar
// yang tidak punya flag rollout tidak akan pernah dilayani (dan karenanya tidak
// akan pernah terbukti juga).
func TestSeedProvenFromHistory(t *testing.T) {
	withTestDB(t)
	id := insertTestAccount(t, "history-proven")
	argon := Models["gemini-4-argon"]

	resetProven := func() {
		provenMu.Lock()
		provenModels = map[string]int64{}
		provenMu.Unlock()
	}
	resetProven()
	t.Cleanup(resetProven)

	now := time.Now().Unix()
	if _, err := getDB().Exec(
		`INSERT INTO requests (ts, model, upstream_model, account_id, status, total_ms, prompt_chars, response_chars, prompt_tokens, output_tokens)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`, now, "gemini-4-argon", "4 Argon", id, 200, 5000, 10, 10, 3, 3); err != nil {
		t.Fatalf("insert riwayat: %v", err)
	}
	// baris yang TIDAK boleh jadi bukti: model lain, dan label hulu tidak cocok
	if _, err := getDB().Exec(
		`INSERT INTO requests (ts, model, upstream_model, account_id, status, total_ms, prompt_chars, response_chars, prompt_tokens, output_tokens)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`, now, "gemini-4-argon", "3.5 Flash-Lite", id, 200, 5000, 10, 10, 3, 3); err != nil {
		t.Fatalf("insert riwayat-2: %v", err)
	}

	seedProvenFromHistory()
	if !modelProven(id, argon) {
		t.Fatal("bukti cocok dari riwayat tidak tertanam")
	}
	if modelProven(id, Models["gemini-3.6-flash"]) {
		t.Fatal("bukti tidak boleh bocor ke model lain")
	}
}
