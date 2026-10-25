package app

import (
	"testing"
	"time"
)

// resetMediaJobs mengganti peta job media dengan yang kosong untuk test, lalu
// membersihkannya lagi saat selesai. Test di paket ini berjalan berurutan, jadi
// aman.
func resetMediaJobs(t *testing.T) {
	t.Helper()
	clear := func() {
		mediaJobsMu.Lock()
		mediaJobs = map[string]*mediaJob{}
		mediaJobsMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// seedMediaJob menyisipkan satu job langsung (tanpa goroutine / tanpa network)
// dengan kunci dedup yang dihitung seperti jalur asli.
func seedMediaJob(t *testing.T, model, prompt, status string, images []pendingUpload, idemKey string) *mediaJob {
	t.Helper()
	j := &mediaJob{
		ID:        "media_" + randHex(16),
		Object:    "media.job",
		Model:     model,
		Status:    status,
		CreatedAt: time.Now().Unix(),
		Prompt:    prompt,
		dedupKey:  mediaDedupKey(model, prompt, images, idemKey),
	}
	putMediaJob(j)
	return j
}

func mediaJobCount() int {
	mediaJobsMu.Lock()
	defer mediaJobsMu.Unlock()
	return len(mediaJobs)
}

// (a) kunci identik + job masih in-flight -> job yang sama, jumlah tidak naik.
func TestDedupSameKeyInFlight(t *testing.T) {
	resetMediaJobs(t)

	for _, status := range []string{"queued", "in_progress"} {
		resetMediaJobs(t)
		existing := seedMediaJob(t, "gemini-image", "a cat", status, nil, "")
		before := mediaJobCount()

		got, deduped := findOrCreateMediaJob("gemini-image", "a cat", nil, "")
		if !deduped {
			t.Fatalf("status=%s: job %s in-flight harus di-dedup", status, status)
		}
		if got.ID != existing.ID {
			t.Fatalf("status=%s: harus pakai job lama %s, dapat %s", status, existing.ID, got.ID)
		}
		if after := mediaJobCount(); after != before {
			t.Fatalf("status=%s: jumlah job berubah before=%d after=%d", status, before, after)
		}
	}
}

// (b) kunci identik tapi job sudah terminal -> job baru dengan id berbeda.
func TestDedupTerminalCreatesNew(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		resetMediaJobs(t)
		existing := seedMediaJob(t, "gemini-image", "a cat", status, nil, "")

		got, deduped := findOrCreateMediaJob("gemini-image", "a cat", nil, "")
		if deduped {
			t.Fatalf("status=%s: job terminal tidak boleh di-dedup", status)
		}
		if got.ID == existing.ID {
			t.Fatalf("status=%s: harus job baru, bukan %s yang sudah selesai", status, existing.ID)
		}
		if mediaJobCount() != 2 {
			t.Fatalf("status=%s: harus ada 2 job, dapat %d", status, mediaJobCount())
		}
	}
}

// (c) model berbeda, prompt sama -> kunci berbeda, job baru.
func TestDedupDifferentModel(t *testing.T) {
	resetMediaJobs(t)
	seedMediaJob(t, "gemini-image", "a cat", "in_progress", nil, "")

	if mediaDedupKey("gemini-image", "a cat", nil, "") == mediaDedupKey("gemini-music", "a cat", nil, "") {
		t.Fatal("model berbeda harus menghasilkan kunci berbeda")
	}
	got, deduped := findOrCreateMediaJob("gemini-music", "a cat", nil, "")
	if deduped {
		t.Fatal("model berbeda tidak boleh di-dedup")
	}
	if got.Model != "gemini-music" {
		t.Fatalf("job baru harus model gemini-music, dapat %s", got.Model)
	}
}

// (d) prompt berbeda -> job baru.
func TestDedupDifferentPrompt(t *testing.T) {
	resetMediaJobs(t)
	seedMediaJob(t, "gemini-image", "a cat", "in_progress", nil, "")

	_, deduped := findOrCreateMediaJob("gemini-image", "a dog", nil, "")
	if deduped {
		t.Fatal("prompt berbeda tidak boleh di-dedup")
	}
}

// (e) jalur header Idempotency-Key: dua panggilan dengan header sama -> satu job.
func TestDedupIdempotencyKey(t *testing.T) {
	resetMediaJobs(t)

	first, d1 := findOrCreateMediaJob("gemini-image", "prompt one", nil, "abc-123")
	if d1 {
		t.Fatal("panggilan pertama harus fresh")
	}
	// prompt sengaja beda: idempotency key mengalahkan hash konten.
	second, d2 := findOrCreateMediaJob("gemini-image", "totally different", nil, "abc-123")
	if !d2 {
		t.Fatal("panggilan kedua dengan Idempotency-Key sama harus di-dedup")
	}
	if second.ID != first.ID {
		t.Fatalf("harus job sama: %s vs %s", first.ID, second.ID)
	}
	if mediaJobCount() != 1 {
		t.Fatalf("harus 1 job, dapat %d", mediaJobCount())
	}

	// Idempotency-Key yang sama pada model berbeda tetap dua job.
	if _, dd := findOrCreateMediaJob("gemini-music", "prompt one", nil, "abc-123"); dd {
		t.Fatal("Idempotency-Key sama pada model berbeda tidak boleh di-dedup")
	}
}

// gambar yang berbeda -> kunci berbeda; gambar identik -> kunci sama.
func TestDedupDifferentImages(t *testing.T) {
	resetMediaJobs(t)
	imgA := []pendingUpload{{Data: []byte("AAAA"), Name: "a.png", Mime: "image/png", Kind: 1}}
	imgB := []pendingUpload{{Data: []byte("BBBB"), Name: "b.png", Mime: "image/png", Kind: 1}}

	seedMediaJob(t, "gemini-image", "same", "in_progress", imgA, "")

	if _, deduped := findOrCreateMediaJob("gemini-image", "same", imgB, ""); deduped {
		t.Fatal("gambar berbeda tidak boleh di-dedup")
	}
	if _, deduped := findOrCreateMediaJob("gemini-image", "same", imgA, ""); !deduped {
		t.Fatal("gambar identik harus di-dedup")
	}
}
