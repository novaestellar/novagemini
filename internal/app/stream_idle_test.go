package app

import (
	"testing"
	"time"
)

// Heartbeat `[{"37":[n]}]` yang datang tiap ~60 detik selama model berpikir DULU
// ikut me-reset penjaga stall, sehingga penjaga itu tak pernah jenuh dan stream
// yang jawabannya sudah selesai tetap terbuka 200-621 detik. Terukur di mesin
// ini: 3.1 Pro membalas "OK" dengan TTFB 211 ms tapi total 621.423 ms.
func TestIsHeartbeatLine(t *testing.T) {
	heartbeats := []string{
		`[{"37":[1]}]`,
		`  [{"37":[42]}]  `,
		`[{"38":[7]}]`,
	}
	for _, h := range heartbeats {
		if !isHeartbeatLine(h) {
			t.Errorf("%q harus dikenali sebagai heartbeat", h)
		}
	}
	notHeartbeats := []string{
		`[["wrb.fr",null,"[[\"teks jawaban"]]]`,
		`[["e",4,null,null,1234]]`,
		`)]}'`,
		`177`,
		`[{"36":[1]}]`,
		"",
	}
	for _, n := range notHeartbeats {
		if isHeartbeatLine(n) {
			t.Errorf("%q TIDAK boleh dianggap heartbeat", n)
		}
	}
}

// Cutoff harus dihitung dari isi terakhir, bukan dari heartbeat: model yang
// berpikir lama (deltanya terus datang) tetap aman, tapi model yang sudah
// menjawab dan hanya mengirim heartbeat tidak menggantung.
func TestContentIdleAfterDefault(t *testing.T) {
	old := rtCfg()
	next := old
	rtMu.Lock()
	rtVal = next
	rtMu.Unlock()

	if d := contentIdleAfter(); d != 25*time.Second {
		t.Errorf("default harus 25 detik, dapat %s", d)
	}

	rtMu.Lock()
	rtVal.ContentIdleSec = 0
	rtMu.Unlock()
	if d := contentIdleAfter(); d != 25*time.Second {
		t.Errorf("nilai 0 harus jatuh ke default 25 detik, dapat %s", d)
	}

	rtMu.Lock()
	rtVal.ContentIdleSec = 90
	rtMu.Unlock()
	if d := contentIdleAfter(); d != 90*time.Second {
		t.Errorf("harus mengikuti konfigurasi (90 detik), dapat %s", d)
	}

	t.Cleanup(func() {
		rtMu.Lock()
		rtVal = old
		rtMu.Unlock()
	})
}

// Terminator tetap dikenali - jangan sampai penambahan heartbeat mematahkannya.
func TestIsStreamTerminatorStillWorks(t *testing.T) {
	if !isStreamTerminator(`[["e",4,null,null,1234]]`) {
		t.Error("frame penutup harus tetap dikenali")
	}
	if isStreamTerminator(`[["wrb.fr",null,"teks"]]`) {
		t.Error("frame isi bukan penutup")
	}
}
