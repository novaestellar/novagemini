package app

import "testing"

// Setelah sebuah akun berbayar sekali menjawab Argon dengan benar, dia naik ke
// tingkat pertama dan pemilihan memakai kecepatan terukur — bukan lagi terkunci
// pada pemegang flag rollout, yang belum tentu yang tercepat.
func TestArgonProvenAccountJoinsFirstTier(t *testing.T) {
	withTestDB(t)
	rollout := insertTestAccount(t, "argon-rollout-slow")
	paid := insertTestAccount(t, "argon-paid-fast")

	seed := func(id int64, tier string, rollouts []string) {
		probeMu.Lock()
		probeCache[id] = AccountCapability{AccountID: id, Alive: true, Tier: tier, Rollouts: rollouts}
		probeMu.Unlock()
	}
	seed(rollout, "pro", []string{"Argon4p0:2"})
	seed(paid, "pro", []string{"Flash3p6PaidV2Rollout:2"})
	t.Cleanup(func() {
		probeMu.Lock()
		delete(probeCache, rollout)
		delete(probeCache, paid)
		probeMu.Unlock()
		latMu.Lock()
		delete(latBoard, rollout)
		delete(latBoard, paid)
		latMu.Unlock()
		provenMu.Lock()
		provenModels = map[string]int64{}
		provenMu.Unlock()
	})

	// pemegang rollout lambat (9 s), akun berbayar tanpa flag jauh lebih cepat (1 s)
	noteLatency(rollout, 9000)
	noteLatency(paid, 1000)

	argon := Models["gemini-4-argon"]

	// sebelum ada bukti: pemegang rollout menang walau lambat.
	got, ok := pickCookieAccountFor(argon, nil)
	if !ok || got == nil || got.ID != rollout {
		t.Fatalf("sebelum terbukti, pemegang rollout harus menang; dapat %+v ok=%v", got, ok)
	}

	// sesudah terbukti menyajikan Argon: yang lebih cepat yang dipakai.
	markModelProven(paid, argon)
	if !modelProven(paid, argon) {
		t.Fatal("markModelProven tidak tercatat")
	}
	got, ok = pickCookieAccountFor(argon, nil)
	if !ok || got == nil || got.ID != paid {
		t.Fatalf("setelah terbukti, akun tercepat harus menang; dapat %+v ok=%v", got, ok)
	}
}

func TestModelProvenScopedPerModel(t *testing.T) {
	withTestDB(t)
	id := insertTestAccount(t, "proven-scope")
	argon := Models["gemini-4-argon"]
	flash := Models["gemini-3.6-flash"]
	t.Cleanup(func() {
		provenMu.Lock()
		provenModels = map[string]int64{}
		provenMu.Unlock()
	})
	markModelProven(id, argon)
	if !modelProven(id, argon) {
		t.Fatal("harus tercatat untuk model yang sama")
	}
	if modelProven(id, flash) {
		t.Fatal("bukti untuk Argon tidak boleh dipakai model lain")
	}
	if modelProven(0, argon) || modelProven(id, ModelConfig{}) {
		t.Fatal("input kosong tidak boleh dianggap terbukti")
	}
}
