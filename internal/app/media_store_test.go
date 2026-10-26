package app

import (
	"strings"
	"testing"
)

// Canvas menghasilkan dokumen HTML utuh. Sebelum ada cabang text/html di
// artifactExt, dokumen itu tersimpan sebagai .bin sehingga panel
// menyajikannya sebagai unduhan octet-stream dan canvas tidak pernah muncul
// sebagai artifact yang bisa dibuka.
func TestArtifactExtHTML(t *testing.T) {
	cases := map[string]string{
		"text/html":                ".html",
		"text/html; charset=utf-8": ".html",
		"TEXT/HTML":                ".html",
	}
	for mime, want := range cases {
		if got := artifactExt(mime); got != want {
			t.Errorf("artifactExt(%q)=%q want %q", mime, got, want)
		}
	}
	// 其它类型不能被这条新分支带偏。
	if got := artifactExt("image/png"); got != ".png" {
		t.Errorf("artifactExt(image/png)=%q want .png", got)
	}
	if got := artifactExt("application/pdf"); got != ".bin" {
		t.Errorf("artifactExt(application/pdf)=%q want .bin", got)
	}
}

// mimeFromExt 必须能反解 artifactExt 的结果,否则画廊列出来的 canvas
// 文件会被当成 application/octet-stream,iframe 也就无从渲染。
func TestMimeExtRoundTrip(t *testing.T) {
	for _, mime := range []string{"text/html", "image/png", "image/jpeg", "video/mp4", "audio/mpeg"} {
		ext := artifactExt(mime)
		if ext == ".bin" {
			continue
		}
		got := mimeFromExt("abc" + ext)
		if mime == "audio/mpeg" && got == "audio/wav" {
			t.Fatalf("unreachable")
		}
		if !strings.HasPrefix(got, strings.Split(mime, "/")[0]) {
			t.Errorf("%s -> %s -> %s, keluarga mime berubah", mime, ext, got)
		}
	}
}

// artifactLink 对 HTML 必须给 [canvas](url), bukan [media](url), supaya
// klien tahu itu halaman yang bisa dibuka dan bukan blob sembarang.
func TestArtifactLinkCanvas(t *testing.T) {
	withSaveMediaToDisk(t, 1)
	t.Chdir(t.TempDir())

	html := []byte("<html><body>" + strings.Repeat("x", 4096) + "</body></html>")
	got := artifactLink(MediaArtifact{Mime: "text/html", Data: html}, "http://test.local")
	if !strings.HasPrefix(got, "[canvas](") {
		t.Errorf("canvas link 应以 [canvas]( 开头, got %q", got)
	}
	if !strings.Contains(got, "/files/") || !strings.HasSuffix(got, ".html)") {
		t.Errorf("canvas link 应指向 /files/*.html, got %q", got)
	}
}
