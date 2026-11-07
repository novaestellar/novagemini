package app

import "testing"

// Akun berbayar yang daftar modelnya memuat Argon (tier "pro") tetapi flag
// rollout ESY5D-nya tidak ikut terkirim harus tetap jadi kandidat TERAKHIR
// untuk gemini-4-argon — bukan tersaring habis seperti sebelumnya.
func TestArgonFallsBackToPaidAccountWithoutRollout(t *testing.T) {
	withTestDB(t)
	proven := insertTestAccount(t, "argon-proven")
	paid := insertTestAccount(t, "argon-paid-unproven")
	free := insertTestAccount(t, "argon-free")

	seed := func(id int64, tier string, rollouts []string) {
		probeMu.Lock()
		probeCache[id] = AccountCapability{AccountID: id, Alive: true, Tier: tier, Rollouts: rollouts}
		probeMu.Unlock()
	}
	seed(proven, "pro", []string{"Argon4p0:2"})
	seed(paid, "pro", []string{"Flash3p6PaidV2Rollout:2"})
	seed(free, "free", nil)
	t.Cleanup(func() {
		probeMu.Lock()
		delete(probeCache, proven)
		delete(probeCache, paid)
		delete(probeCache, free)
		probeMu.Unlock()
	})

	argon := Models["gemini-4-argon"]

	// 1. selama pemegang rollout tersedia, dia yang dipakai.
	got, ok := pickCookieAccountFor(argon, nil)
	if !ok || got == nil || got.ID != proven {
		t.Fatalf("pemegang rollout harus menang; dapat %+v ok=%v", got, ok)
	}

	// 2. pemegang rollout disaring keluar (mis. cooldown 5 menit) -> akun
	//    berbayar tanpa flag rollout dipakai, bukan menyerah jadi 502.
	got, ok = pickCookieAccountFor(argon, map[int64]bool{proven: true})
	if !ok || got == nil || got.ID != paid {
		t.Fatalf("tingkat kedua harus akun berbayar tanpa rollout; dapat %+v ok=%v", got, ok)
	}

	// 3. akun free TIDAK boleh dipilih untuk Argon, bahkan sebagai usaha
	//    terakhir. Terukur 2026-11-05: akun free meminta Argon/3.8 dan Google
	//    menurunkannya ke 3.5 Flash-Lite / 3.6 Flash - jawaban 200 dengan model
	//    salah yang mustahil dibedakan klien. Operator mengoreksi: Argon, 3.1 Pro,
	//    3.7, 3.8 semuanya khusus akun pro. Lebih baik tidak memilih sama sekali.
	if got, ok := pickCookieAccountFor(argon, map[int64]bool{proven: true, paid: true}); ok {
		t.Fatalf("akun free tidak boleh jadi kandidat untuk Argon; dapat %+v", got)
	}
}

// TestPaidGateCoversAllPaidModels: koreksi operator 2026-11-05. Gerbang pro
// tadinya HANYA hexArgon4, jadi 3.1 Pro dan 3.8/3.7 Flash tidak digatek - akun
// free dipilih lalu diturunkan Google ke 3.6 Flash. Keempat model ini khusus akun
// pro, dan pemilih tidak boleh menawarkan akun free untuk mereka sekalipun.
func TestPaidGateCoversAllPaidModels(t *testing.T) {
	paidModels := []string{
		"gemini-4-argon",
		"gemini-3.1-pro",
		"gemini-3.8-flash",
		"gemini-3.7-flash",
	}
	for _, name := range paidModels {
		m := Models[name]
		if !modelRequiresPaidAccount(m) {
			t.Errorf("%s harus dikenali sebagai model khusus akun pro", name)
		}
		pro := AccountCapability{Alive: true, Tier: "pro"}
		if !pro.supportsModelPaidOnly(m) {
			t.Errorf("akun pro harus lolos gerbang berbayar untuk %s", name)
		}
		free := AccountCapability{Alive: true, Tier: "free"}
		if free.supportsModelPaidOnly(m) {
			t.Errorf("akun free tidak boleh lolos gerbang berbayar untuk %s", name)
		}
	}

	// Model yang memang bisa dilayani free TIDAK boleh ikut digatek.
	for _, name := range []string{"gemini-3.6-flash", "gemini-3.5-flash-lite", "gemini-3.6-flash-thinking"} {
		if modelRequiresPaidAccount(Models[name]) {
			t.Errorf("%s seharusnya bisa dilayani akun free", name)
		}
	}
}

// TestSupportsModelPaidOnlyArgon memastikan gerbang tingkat kedua.
func TestSupportsModelPaidOnlyArgon(t *testing.T) {
	argon := Models["gemini-4-argon"]
	paidNoRollout := AccountCapability{Alive: true, Tier: "pro", Rollouts: []string{"SomeOtherRollout:2"}}
	if paidNoRollout.supportsModel(argon) {
		t.Fatal("tanpa rollout Argon4p0 supportsModel harus tetap menolak")
	}
	if !paidNoRollout.supportsModelPaidOnly(argon) {
		t.Fatal("akun berbayar harus lolos di tingkat kedua untuk Argon")
	}

	flash38 := Models["gemini-3.8-flash"]
	if paidNoRollout.supportsModelPaidOnly(flash38) {
		t.Fatal("3.8 Flash TIDAK boleh lolos tingkat kedua hanya karena pro - wajib punya rollout Flash3p8")
	}
}
