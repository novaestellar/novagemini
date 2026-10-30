package app

import "testing"

// Kebijakan musik: kuota dipakai sebagai izin percobaan, BUKAN cap sebagai
// syarat mutlak.
//
// Pernah dicoba sebaliknya (mewajibkan cap MUSIC/LYRIA) dan itu salah: akun
// nova-ff2 tidak mencantumkan cap musik di probe, tapi di web akun yang sama
// musik tetap bisa dibuat. Probe bisa terpotong; memblokir akun yang sebenarnya
// mampu jauh lebih mahal daripada satu percobaan yang gagal cepat.
func TestSupportsModelMusicUsesQuotaNotCap(t *testing.T) {
	music := ModelConfig{HexID: hexFlash36, Mode: 1, Tool: toolMusic}

	noCapButQuota := AccountCapability{
		Alive: true, Tier: "pro", QuotaMusic: 4,
		Caps: []string{"CANVAS", "IMAGEN_4", "VIDEO_GENERATION"},
	}
	if !noCapButQuota.supportsModel(music) {
		t.Error("akun dengan kuota musik harus tetap dicoba walau cap tidak tercantum")
	}

	zeroQuota := AccountCapability{Alive: true, Tier: "pro", QuotaMusic: 0}
	if zeroQuota.supportsModel(music) {
		t.Error("kuota musik 0 berarti tidak ada izin percobaan")
	}

	// Akun yang probe-nya tidak hidup sengaja TIDAK disaring di sini: status akun
	// (enabled/disabled) yang menentukan, bukan data kapabilitas yang belum ada.
	// Menyaring di dua tempat membuat akun sehat bisa hilang dari pool.
	unknown := AccountCapability{Alive: false, QuotaMusic: 4}
	if !unknown.supportsModel(music) {
		t.Error("probe mati tidak boleh menyaring akun di gate kapabilitas")
	}
}
