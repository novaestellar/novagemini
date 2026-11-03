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

	// 3. akun free bukan lagi tersaring habis. Uji 2026-11-03: lima akun yang
	//    dilabeli tier=free semuanya melayani "4 Argon" saat diminta, jadi label
	//    tier tidak boleh membuang kandidat — dia cuma pilihan paling akhir,
	//    dan deteksi downgrade + cooldown yang memutuskan.
	got, ok = pickCookieAccountFor(argon, map[int64]bool{proven: true, paid: true})
	if !ok || got == nil || got.ID != free {
		t.Fatalf("akun free harus jadi cadangan terakhir, bukan dibuang; dapat %+v ok=%v", got, ok)
	}
}

func TestSupportsModelPaidOnlyArgon(t *testing.T) {
	argon := Models["gemini-4-argon"]
	paidNoRollout := AccountCapability{Alive: true, Tier: "pro", Rollouts: []string{"Flash3p8:2"}}
	if paidNoRollout.supportsModel(argon) {
		t.Fatal("tanpa rollout Argon4p0 supportsModel harus tetap menolak")
	}
	if !paidNoRollout.supportsModelPaidOnly(argon) {
		t.Fatal("akun berbayar harus lolos di tingkat kedua")
	}
	freeAcct := AccountCapability{Alive: true, Tier: "free"}
	if freeAcct.supportsModelPaidOnly(argon) {
		t.Fatal("akun free tidak boleh lolos di tingkat kedua")
	}
	if freeAcct.supportsModelPaidOnly(Models["gemini-3.6-flash"]) {
		t.Fatal("supportsModelPaidOnly hanya berlaku untuk Argon")
	}
	// tier "unknown" (belum diprobe) bukan bukti berbayar.
	if (AccountCapability{Alive: true}).supportsModelPaidOnly(argon) {
		t.Fatal("tier kosong tidak boleh dianggap berbayar")
	}
}
