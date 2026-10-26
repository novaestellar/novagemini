package app

import (
	"encoding/json"
	"testing"
)

// parseModelHeader mengurai header x-goog-ext-525001261-jspb jadi array.
// Bentuknya:
//
//	[1,null,null,null,"<hex>",null,null,0,[4,5,6,8],null,null,1,null,null,<mode>,<think>,"<uuid>"]
//
// jadi mode ada di indeks 14 dan think di indeks 15.
func parseModelHeader(t *testing.T, header string) []json.RawMessage {
	t.Helper()
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(header), &arr); err != nil {
		t.Fatalf("header bukan JSON array: %v (%s)", err, header)
	}
	if len(arr) < 16 {
		t.Fatalf("header terlalu pendek (%d elemen): %s", len(arr), header)
	}
	return arr
}

func headerInt(t *testing.T, arr []json.RawMessage, idx int) int {
	t.Helper()
	var v int
	if err := json.Unmarshal(arr[idx], &v); err != nil {
		t.Fatalf("elemen %d bukan angka: %v (%s)", idx, err, arr[idx])
	}
	return v
}

const (
	thinkPlain    = 1
	thinkExtended = 2
)

// Angka ThinkLevel BUKAN urutan naik: low=1, high=2, medium=4.
// Perbandingan lama `thinkLevel >= thinkLevelHigh` membuat medium (4 >= 2)
// ikut dikirim sebagai extended thinking, dan Google membalas dengan model
// lain (3.5 Flash-Lite) -> deteksi downgrade -> 502.
func TestBuildModelHeaderArgonThinkLevel(t *testing.T) {
	uuid := "11111111-1111-1111-1111-111111111111"
	cases := []struct {
		name      string
		thinkLvl  int
		wantThink int
	}{
		{"low", thinkLevelLow, thinkPlain},
		{"medium", thinkLevelMedium, thinkPlain}, // 4 bukan extended
		{"high", thinkLevelHigh, thinkExtended},  // hanya high yang extended
		{"default", 0, thinkPlain},
	}
	for _, tc := range cases {
		// Produksi selalu mengirim thinkingNormal (=1) sebagai nilai dasar
		// (lihat gemini.go:653 dan conversation.go:329), bukan 0.
		h := buildModelHeader(hexArgon4, 8, thinkingNormal, tc.thinkLvl, uuid)
		arr := parseModelHeader(t, h)
		if got := headerInt(t, arr, 15); got != tc.wantThink {
			t.Errorf("Argon %s (ThinkLevel=%d): think=%d want %d", tc.name, tc.thinkLvl, got, tc.wantThink)
		}
	}
}

// Mode Argon harus tetap 8 (itu satu-satunya nilai yang benar-benar memilih
// Argon; hex diabaikan server). Jangan sampai perubahan di atas menggesernya.
func TestBuildModelHeaderArgonKeepsMode8(t *testing.T) {
	h := buildModelHeader(hexArgon4, 8, 0, thinkLevelLow, "11111111-1111-1111-1111-111111111111")
	arr := parseModelHeader(t, h)
	if got := headerInt(t, arr, 14); got != 8 {
		t.Errorf("Argon mode=%d want 8", got)
	}
}

// Non-Argon tidak boleh ikut menerjemahkan ThinkLevel jadi extended thinking:
// terjemahan itu khusus Argon, model lain memakai think apa adanya.
func TestBuildModelHeaderNonArgonKeepsThink(t *testing.T) {
	h := buildModelHeader(hexFlash38, 1, 0, thinkLevelHigh, "11111111-1111-1111-1111-111111111111")
	arr := parseModelHeader(t, h)
	if got := headerInt(t, arr, 15); got != 0 {
		t.Errorf("non-Argon think=%d want 0 (ThinkLevel hanya untuk Argon)", got)
	}
}
