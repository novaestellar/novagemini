package app

import "testing"

// supportsModel menyaring akun berdasarkan hasil probe. Yang PENTING untuk
// dipahami: sinyal dari probe (tier/rollouts) TIDAK bisa dipercaya sebagai
// "akun ini pasti bisa menyajikan model X".
//
// Bukti dari tabel requests: akun FREE (acc1) berkali-kali dilayani
// "3.1 Pro" dengan sukses. Sebaliknya akun PRO (nova-ff2) yang punya rollout
// Argon4p0 pun bisa dijawab "3.5 Flash-Lite" saat kuota/rollout-nya sedang
// habis. Artinya penyaringan di sini hanya untuk MENGHINDARI pilihan yang
// jelas mustahil (akun free untuk Argon), bukan jaminan kualitas.
//
// Konsekuensinya: jangan pernah menyaring akun free dari model teks biasa
// seperti 3.1 Pro. Kalau disaring, saat satu-satunya akun pro kena limit
// tidak ada akun cadangan sama sekali dan SEMUA permintaan 502 — itu regresi
// yang pernah terjadi.
func TestSupportsModelDoesNotGatePro31ByTier(t *testing.T) {
	pro := Models["gemini-3.1-pro"]
	proThink := Models["gemini-3.1-pro-thinking"]

	// Akun free harus tetap DILOLOSKAN: terbukti pernah melayani 3.1 Pro.
	freeAcct := AccountCapability{
		Alive:    true,
		Tier:     "free",
		Caps:     []string{"EXTENSIONS"},
		Rollouts: []string{"Fast3.2FreeRollout:3", "Flash3p6PaidV2Rollout:2"},
	}
	if !freeAcct.supportsModel(pro) {
		t.Error("akun free tidak boleh disaring dari 3.1 Pro (pernah sukses melayaninya)")
	}
	if !freeAcct.supportsModel(proThink) {
		t.Error("akun free tidak boleh disaring dari 3.1 Pro thinking")
	}

	paidAcct := AccountCapability{
		Alive:    true,
		Tier:     "pro",
		Caps:     []string{"CANVAS", "IMAGEN_4"},
		Rollouts: []string{"Argon4p0:2", "Flash3p8:1"},
	}
	if !paidAcct.supportsModel(pro) {
		t.Error("akun pro harus bisa menyajikan 3.1 Pro")
	}

	// Akun yang belum pernah diprobe tetap diloloskan: lebih baik mencoba
	// daripada menolak berdasarkan data yang tidak ada.
	if !(AccountCapability{}).supportsModel(pro) {
		t.Error("akun tanpa data probe harus tetap dicoba")
	}
}

// Argon adalah satu-satunya model yang memang WAJIB disaring: akun tanpa
// rollout Argon4p0 tidak punya jalur apa pun untuk menyajikannya.
func TestSupportsModelArgon(t *testing.T) {
	argon := Models["gemini-4-argon"]
	freeAcct := AccountCapability{
		Alive:    true,
		Tier:     "free",
		Rollouts: []string{"Fast3.2FreeRollout:3", "Flash3p6PaidV2Rollout:2"},
	}
	if freeAcct.supportsModel(argon) {
		t.Error("akun tanpa Argon4p0 tidak boleh dipilih untuk Argon")
	}
	paidAcct := AccountCapability{Alive: true, Tier: "pro", Rollouts: []string{"Argon4p0:2"}}
	if !paidAcct.supportsModel(argon) {
		t.Error("akun dengan Argon4p0 harus bisa menyajikan Argon")
	}
}

// 3.6 Flash: akun yang sudah punya Flash3p8/Flash3p7 TETAP boleh dipilih.
//
// Aturan lama menolaknya karena request 3.6 di sana dijawab 3.8 dan deteksi
// model menjawab "downgraded". Tapi 3.8 lebih tinggi dari 3.6 dan konteksnya
// tidak mengecil — itu kenaikan, bukan penurunan. Menolak akunnya membuat 3.6
// dan 3.6-thinking mati total di akun utama yang punya 3.8 (gejalanya "needs a
// signed-in PAID account" padahal akunnya pro). Deteksi sekarang membedakan
// arah: naik diterima, turun ke 3.5 Flash-Lite tetap ditolak.
func TestSupportsModelFlash36AcceptsNewerAccount(t *testing.T) {
	f36 := Models["gemini-3.6-flash"]

	older := AccountCapability{Alive: true, Rollouts: []string{"Flash3p6PaidV2Rollout:3"}}
	if !older.supportsModel(f36) {
		t.Error("akun dengan Flash3p6 harus bisa menyajikan 3.6")
	}

	newer := AccountCapability{Alive: true, Rollouts: []string{"Flash3p8:1", "Flash3p7:3"}}
	if !newer.supportsModel(f36) {
		t.Error("akun dengan Flash3p8/3p7 harus tetap bisa melayani 3.6 (Google menaikkan ke 3.8)")
	}

	// Yang tetap tidak boleh: akun tanpa satu pun rollout 3.6/3.7/3.8/Argon.
	none := AccountCapability{Alive: true, Rollouts: []string{"FlashLitePaidV2Rollout:3"}}
	if none.supportsModel(f36) {
		t.Error("akun tanpa rollout 3.6/3.7/3.8 seharusnya tidak dipilih")
	}
}
