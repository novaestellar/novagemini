package app

import "testing"

// Papan skor harus memilih akun yang jelas lebih cepat, dan tidak mengganggu
// round-robin ketika keduanya belum punya sampel.
func TestFasterAccountPrefersLowerLatency(t *testing.T) {
	latMu.Lock()
	latBoard = map[int64]*latWindow{}
	latMu.Unlock()

	if fasterAccount(1, 2) || fasterAccount(2, 1) {
		t.Fatal("akun tanpa sampel tidak boleh saling mengalahkan")
	}

	noteLatency(1, 200_000) // 200s
	noteLatency(2, 50_000)  //  50s

	if !fasterAccount(2, 1) {
		t.Fatal("akun 50s harus dianggap lebih cepat daripada 200s")
	}
	if fasterAccount(1, 2) {
		t.Fatal("akun 200s tidak boleh menang atas 50s")
	}

	// Ambang 25%: 100s vs 110s terlalu dekat, harus seri (round-robin tetap).
	noteLatency(3, 100_000)
	noteLatency(4, 110_000)
	if fasterAccount(3, 4) {
		t.Fatal("selisih 10% seharusnya tidak cukup untuk mengganti pilihan")
	}

	// Akun tanpa sampel memakai rata-rata papan, bukan 0 (yang akan selalu
	// menang) dan bukan pula +inf (yang tidak akan pernah dipilih).
	// Papan saat ini: 200, 50, 100, 110 -> rata-rata 115 detik.
	// 100s vs 115s hanya beda 15%, di bawah ambang 25% -> tidak mengganti.
	if fasterAccount(3, 5) {
		t.Fatal("selisih 15% terhadap rata-rata papan seharusnya belum cukup")
	}
	// Akun yang benar-benar cepat (40s) harus menang atas akun tanpa sampel.
	noteLatency(6, 40_000)
	if !fasterAccount(6, 5) {
		t.Fatal("akun 40s harus menang atas akun tanpa sampel (rata-rata papan)")
	}
}

func TestLatencyWindowExpires(t *testing.T) {
	latMu.Lock()
	latBoard = map[int64]*latWindow{}
	latMu.Unlock()

	noteLatency(9, 42_000)
	if _, ok := accountLatency(9); !ok {
		t.Fatal("sampel baru harus terbaca")
	}

	// Paksa jendela kedaluwarsa.
	latMu.Lock()
	latBoard[9].since -= latWindowSeconds + 1
	latMu.Unlock()

	if _, ok := accountLatency(9); ok {
		t.Fatal("sampel lebih tua dari 1 jam harus diabaikan")
	}
}
