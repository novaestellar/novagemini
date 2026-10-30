package app

import "testing"

// Regresi: WARP mati membuat probe menulis alive=0/tier="" untuk semua akun,
// dan aturan "pro dulu" ikut mati tanpa gejala. Hasil probe yang gagal karena
// jalur keluar tidak boleh menghapus pengetahuan sebelumnya.
func TestMergeProbeKeepsDataOnTransportError(t *testing.T) {
	prev := AccountCapability{
		AccountID: 6, Tier: "pro", Alive: true,
		Caps:       []string{"CANVAS", "IMAGEN_4", "VIDEO_GENERATION"},
		Rollouts:   []string{"Argon4p0:2"},
		QuotaImage: 3, QuotaMusic: 2, QuotaThink: 5, QuotaVideo: 1,
		CheckedAt: 1000,
	}
	cur := AccountCapability{
		AccountID: 6, Alive: false, Tier: "",
		Err:       `Get "https://gemini.google.com/app": proxyconnect tcp: dial tcp 127.0.0.1:40000: connect: connection refused`,
		CheckedAt: 2000,
	}

	got := mergeProbeWithPrevious(cur, prev)
	if !got.Alive || got.Tier != "pro" {
		t.Fatalf("hasil probe transport menimpa data lama: alive=%v tier=%q", got.Alive, got.Tier)
	}
	if len(got.Caps) != 3 || len(got.Rollouts) != 1 {
		t.Fatalf("caps/rollouts hilang: %v %v", got.Caps, got.Rollouts)
	}
	if got.QuotaImage != 3 || got.QuotaMusic != 2 || got.QuotaThink != 5 || got.QuotaVideo != 1 {
		t.Fatalf("kuota hilang: %+v", got)
	}
	if got.Err == "" || got.CheckedAt != 2000 {
		t.Errorf("err/checked_at tidak diperbarui: %q %d", got.Err, got.CheckedAt)
	}
}

// Sesi mati tetap alive=false (itu benar), tapi tier terakhir dipertahankan
// supaya routing langsung pintar begitu cookie diperbarui.
func TestMergeProbeKeepsTierOnDeadSession(t *testing.T) {
	prev := AccountCapability{
		AccountID: 1, Tier: "free", Alive: true,
		Caps: []string{"CANVAS"}, Rollouts: []string{"Flash3p6PaidV2Rollout:3"},
	}
	cur := AccountCapability{
		AccountID: 1, Alive: false,
		Err:       "session dead: no SNlM0e in page (cookie expired or not signed in)",
		CheckedAt: 3000,
	}

	got := mergeProbeWithPrevious(cur, prev)
	if got.Alive {
		t.Error("sesi mati seharusnya alive=false")
	}
	if got.Tier != "free" || len(got.Caps) != 1 || len(got.Rollouts) != 1 {
		t.Fatalf("tier/caps terakhir terhapus: %+v", got)
	}
}

// Tanpa hasil sebelumnya (akun baru), hasil apa adanya dipakai.
func TestMergeProbeWithoutPrevious(t *testing.T) {
	cur := AccountCapability{AccountID: 9, Alive: true, Tier: "pro", Caps: []string{"CANVAS"}}
	got := mergeProbeWithPrevious(cur, AccountCapability{})
	if got.Tier != "pro" || len(got.Caps) != 1 {
		t.Fatalf("hasil baru dirusak: %+v", got)
	}
}
