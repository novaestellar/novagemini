package app

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHeartbeatCoversSilentGap mengunci bug versi pertama perbaikan denyut:
// denyut dulu baru menyala di dalam start(), sedangkan start() hanya berjalan
// ketika delta pertama tiba. Jeda DI DEPAN token pertama - jeda yang justru
// memicu 503 dari proxy - jadi sama sekali tidak tertutup. Di sini writer
// dibuat, lalu tidak ada satu delta pun selama beberapa interval.
func TestHeartbeatCoversSilentGap(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 40 * time.Millisecond
	defer func() { heartbeatInterval = old }()

	rec := httptest.NewRecorder()
	s := newSSEWriter(context.Background(), rec, "id-silent", 1, "gemini-3.6-flash")

	time.Sleep(150 * time.Millisecond) // >3 interval, nol byte dari hulu
	s.Close()

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("header belum terkirim selama jeda: Content-Type=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("chunk role tidak dikirim selama jeda: %q", body)
	}
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("keepalive tidak dikirim selama jeda: %q", body)
	}

	n := rec.Body.Len()
	time.Sleep(120 * time.Millisecond)
	if rec.Body.Len() != n {
		t.Fatalf("masih menulis setelah Close: %d -> %d", n, rec.Body.Len())
	}
}

// TestFastStreamHasNoKeepalive: stream normal - delta tiba sebelum interval
// pertama - tidak boleh disisipi komentar keepalive, dan tetap ditutup [DONE].
func TestFastStreamHasNoKeepalive(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 5 * time.Second
	defer func() { heartbeatInterval = old }()

	rec := httptest.NewRecorder()
	s := newSSEWriter(context.Background(), rec, "id-fast", 1, "gemini-4-argon")
	defer s.Close()

	s.SendContent("OK")
	if err := s.Settle("OK"); err != nil {
		t.Fatalf("jawaban sah ditolak: %v", err)
	}
	s.Finish("stop", nil)

	body := rec.Body.String()
	if strings.Contains(body, ": keepalive") {
		t.Fatalf("keepalive muncul di stream cepat: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("[DONE] tidak ada: %q", body)
	}
}
