package app

import (
	"strings"
	"testing"
)

// Canvas dikirim Google sebagai blok kode berpagar. Kalau fence ikut tersimpan,
// file .html diawali "```html" dan diakhiri "```", sehingga browser merender
// teks itu sebagai isi halaman, bukan dokumen.
func TestStripCodeFence(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"fence html",
			"```html\n<!DOCTYPE html><html><body>hi</body></html>\n```",
			"<!DOCTYPE html><html><body>hi</body></html>",
		},
		{
			"fence tanpa bahasa",
			"```\n<!DOCTYPE html><html></html>\n```",
			"<!DOCTYPE html><html></html>",
		},
		{
			"tanpa fence",
			"<!DOCTYPE html><html></html>",
			"<!DOCTYPE html><html></html>",
		},
		{
			"fence tanpa penutup",
			"```html\n<!DOCTYPE html><html></html>",
			"<!DOCTYPE html><html></html>",
		},
		{
			"fence huruf besar",
			"```HTML\n<!DOCTYPE html><html></html>\n```",
			"<!DOCTYPE html><html></html>",
		},
	}
	for _, tc := range cases {
		if got := stripCodeFence(tc.in); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// Dokumen canvas yang sudah bersih tidak boleh diubah lagi (idempoten),
// karena extractCanvasDoc bisa dipanggil pada payload yang sama berkali-kali.
func TestStripCodeFenceIdempotent(t *testing.T) {
	doc := "<!DOCTYPE html><html><body><p>ok</p></body></html>"
	once := stripCodeFence(doc)
	if twice := stripCodeFence(once); twice != once {
		t.Errorf("tidak idempoten: %q lalu %q", once, twice)
	}
}

// Canvas harus SELALU ditulis ke disk saat fitur itu menyala, walau kecil.
// Alasannya bukan ukuran: Chrome memblokir data: URL di iframe/tab baru, dan
// galeri hanya memuat isi data/media. Canvas inline karena itu tidak pernah
// bisa dibuka maupun muncul di galeri.
func TestCanvasAlwaysSavedToDisk(t *testing.T) {
	withSaveMediaToDisk(t, 1024) // ambang inline 1 MB
	t.Chdir(t.TempDir())

	// Dokumen kecil: jauh di bawah ambang, tapi tetap harus jadi URL.
	doc := []byte("<!DOCTYPE html><html><body><h1>hi</h1></body></html>")
	got := artifactLink(MediaArtifact{Mime: "text/html", Data: doc}, "http://test.local")
	if !strings.HasPrefix(got, "[canvas](http") {
		t.Errorf("canvas kecil harus tetap disimpan ke disk, got %q", got)
	}
	if strings.Contains(got, "data:") {
		t.Errorf("canvas tidak boleh jadi data URL (diblokir browser), got %q", got)
	}

	// Gambar kecil juga harus ke disk: base64 di dalam markdown membengkakkan
	// payload ke klien, dan galeri hanya membaca isi data/media — gambar inline
	// tidak pernah muncul di sana.
	img := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	g := artifactLink(MediaArtifact{Mime: "image/png", Data: img}, "http://test.local")
	if !strings.HasPrefix(g, "![image](http") {
		t.Errorf("gambar kecil harus jadi URL /files, got %q", g)
	}
	if strings.Contains(g, "data:") {
		t.Errorf("gambar tidak boleh inline saat simpan-ke-disk menyala, got %q", g)
	}
}
