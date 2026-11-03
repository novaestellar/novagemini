package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Penolakan generik Google datang sebagai teks jawaban biasa di jalur stream.
// Dulu teks itu diteruskan apa adanya, jadi klien menerima pesan error sebagai
// konten model. Sekarang potongan awal ditahan dan Settle memutuskannya.
func TestSettleRejectsCannedRefusal(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newSSEWriter(context.Background(), rec, "id", 1, "gemini-3.8-flash")

	s.SendContent("I seem to be encountering an error. Can I try something else for you?")
	if strings.Contains(rec.Body.String(), `"content"`) {
		t.Fatalf("isi penolakan terlanjur dikirim sebagai konten: %s", rec.Body.String())
	}
	if err := s.Settle("I seem to be encountering an error. Can I try something else for you?"); err == nil {
		t.Fatal("penolakan generik tidak dikenali")
	}
}

// Jawaban pendek yang sah harus tetap terkirim saat Settle dipanggil.
func TestSettleFlushesShortAnswer(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newSSEWriter(context.Background(), rec, "id", 1, "gemini-3.6-flash")

	s.SendContent("OK")
	if err := s.Settle("OK"); err != nil {
		t.Fatalf("jawaban sah ditolak: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"content":"OK"`) {
		t.Fatalf("jawaban pendek tidak terkirim: %s", rec.Body.String())
	}
}

// Jawaban panjang harus mengalir, bukan ditahan sampai akhir: buffer dilepas
// begitu melewati heldContentLimit.
func TestStreamReleasesAfterLimit(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newSSEWriter(context.Background(), rec, "id", 1, "gemini-4-argon")

	long := strings.Repeat("a", heldContentLimit+10)
	s.SendContent(long)
	if !strings.Contains(rec.Body.String(), `"content"`) {
		t.Fatal("jawaban panjang tertahan di buffer")
	}
}

// ResponseWriter tanpa Flusher tidak boleh panik.
func TestSSEWriterWithoutFlusher(t *testing.T) {
	var w http.ResponseWriter = &noFlushWriter{}
	s := newSSEWriter(context.Background(), w, "id", 1, "m")
	s.SendContent("hello")
	if err := s.Settle("hello"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

type noFlushWriter struct{ hdr http.Header }

func (n *noFlushWriter) Header() http.Header {
	if n.hdr == nil {
		n.hdr = http.Header{}
	}
	return n.hdr
}
func (n *noFlushWriter) Write(b []byte) (int, error) { return len(b), nil }
func (n *noFlushWriter) WriteHeader(int)             {}
