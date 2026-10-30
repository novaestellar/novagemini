package app

import (
	"sync"
	"time"
)

// Papan skor latensi per akun.
//
// Kenapa ada: pemilihan akun sebelumnya hanya bertanya "akun mana yang
// mendukung model ini", bukan "akun mana yang sedang cepat". Padahal data
// menunjukkan selisihnya besar — pada model yang sama, satu akun bisa p50
// 98 detik sementara akun lain 202 detik pada jam yang sama, dan yang paling
// lambat bukan selalu yang paling sibuk.
//
// Yang disimpan hanya rata-rata bergerak 1 jam: jumlah milidetik dan cacah
// sampel. Tidak ada riwayat panjang, tidak ada tabel baru, dan angkanya hilang
// saat proses berhenti (boleh — ini petunjuk pemilihan, bukan data laporan).

const (
	latWindowSeconds = 3600
	// latDelta: akun baru dianggap lebih cepat kalau rata-ratanya minimal
	// 25% lebih rendah. Ambang ini mencegah dua akun yang praktis sama
	// saling mengalahkan terus-menerus (yang merusak round-robin).
	latDelta = 1.25
)

type latWindow struct {
	ms    int64
	n     int
	since int64
}

var (
	latMu    sync.Mutex
	latBoard = map[int64]*latWindow{}
)

// noteLatency mencatat satu hasil sukses untuk sebuah akun.
func noteLatency(accountID, ms int64) {
	if accountID <= 0 || ms <= 0 {
		return
	}
	now := time.Now().Unix()
	latMu.Lock()
	w := latBoard[accountID]
	if w == nil || now-w.since > latWindowSeconds {
		w = &latWindow{since: now}
	}
	w.ms += ms
	w.n++
	latBoard[accountID] = w
	latMu.Unlock()
}

// accountLatency mengembalikan rata-rata 1 jam untuk satu akun.
func accountLatency(accountID int64) (float64, bool) {
	now := time.Now().Unix()
	latMu.Lock()
	defer latMu.Unlock()
	w := latBoard[accountID]
	if w == nil || w.n == 0 || now-w.since > latWindowSeconds {
		return 0, false
	}
	return float64(w.ms) / float64(w.n), true
}

// latencyBoardAverage dipakai sebagai nilai netral untuk akun yang belum punya
// sampel: akun baru tidak otomatis dianggap cepat, juga tidak dihukum.
func latencyBoardAverage() float64 {
	latMu.Lock()
	defer latMu.Unlock()
	var sum float64
	var n int
	for _, w := range latBoard {
		if w.n > 0 && time.Now().Unix()-w.since <= latWindowSeconds {
			sum += float64(w.ms) / float64(w.n)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// fasterAccount menjawab apakah a cukup lebih cepat daripada b untuk dipilih.
// Kalau keduanya belum punya sampel, kembalikan false supaya urutan SQL
// (round-robin) tetap berlaku.
func fasterAccount(a, b int64) bool {
	sa, oka := accountLatency(a)
	sb, okb := accountLatency(b)
	if !oka && !okb {
		return false
	}
	if avg := latencyBoardAverage(); avg > 0 {
		if !oka {
			sa = avg
		}
		if !okb {
			sb = avg
		}
	}
	if sa <= 0 || sb <= 0 {
		return false
	}
	return sa*latDelta < sb
}

// LatRow adalah satu baris papan skor untuk panel.
type LatRow struct {
	ID    int64   `json:"id"`
	Label string  `json:"label"`
	AvgMs float64 `json:"avg_ms"`
	N     int     `json:"n"`
}

// latencyRows mengembalikan akun yang punya sampel, tercepat lebih dulu.
// Dipakai /admin/api/scoreboard supaya papan skor bisa dilihat, bukan cuma
// bekerja diam-diam.
func latencyRows() []LatRow {
	now := time.Now().Unix()
	latMu.Lock()
	snapshot := make(map[int64]LatRow, len(latBoard))
	for id, w := range latBoard {
		if w.n == 0 || now-w.since > latWindowSeconds {
			continue
		}
		snapshot[id] = LatRow{ID: id, AvgMs: float64(w.ms) / float64(w.n), N: w.n}
	}
	latMu.Unlock()

	out := make([]LatRow, 0, len(snapshot))
	for _, a := range accountList() {
		if row, ok := snapshot[a.ID]; ok {
			row.Label = a.Label
			out = append(out, row)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].AvgMs < out[j-1].AvgMs; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
