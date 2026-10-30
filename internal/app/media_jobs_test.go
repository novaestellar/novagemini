package app

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withSaveMediaToDisk 在测试期间强制开启磁盘落盘,并把内联阈值降到 1 KB,
// 这样测试可以用小字节数区分「内联」与「落盘」两类产物。用完恢复原值。
func withSaveMediaToDisk(t *testing.T, inlineKB int) {
	t.Helper()
	rtMu.Lock()
	old := rtVal
	rtVal.SaveMediaToDisk = true
	rtVal.MediaInlineKB = inlineKB
	rtMu.Unlock()
	t.Cleanup(func() {
		rtMu.Lock()
		rtVal = old
		rtMu.Unlock()
	})
}

// countFiles 递归统计 dir 下的文件数(不含目录)。
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return n
}

// TestDeriveMediaJobArtifactsNoDuplicateSave 锁死「一个产物只写一次盘」这个不变量。
//
// 合成一份结果:一个小的内联产物 + 一个大的落盘产物,用 appendArtifactMarkdownWithBase
// 渲染成 markdown(与 callGemini 生产路径完全一致),然后走 runMediaJob 使用的
// deriveMediaJobArtifacts 派生结构化列表,断言:
//
//	a) 派生列表数量正确;
//	b) 大产物 URL 非空、Base64 为空;
//	c) 小产物 URL 为空、Base64 非空;
//	d) 磁盘上文件数 == 大产物数量(即每个大产物只落一份,不是两份)。
func TestDeriveMediaJobArtifactsNoDuplicateSave(t *testing.T) {
	// 产物写到相对路径 ./data/media,切到临时目录隔离,避免污染真实数据。
	tmp := t.TempDir()
	t.Chdir(tmp)
	withSaveMediaToDisk(t, 1) // 阈值 1 KB

	small := MediaArtifact{Mime: "image/png", Data: bytesRepeat(0x41, 512)}  // < 1 KB -> 内联
	large := MediaArtifact{Mime: "video/mp4", Data: bytesRepeat(0x42, 4096)} // >= 1 KB -> 落盘
	src := []MediaArtifact{small, large}

	baseURL := "http://test.local"
	content := appendArtifactMarkdownWithBase("", src, baseURL)

	arts := deriveMediaJobArtifacts(content, src)

	if len(arts) != 2 {
		t.Fatalf("(a) 派生列表数量=%d want 2; content=%q", len(arts), content)
	}

	// 顺序应与 src 一致:先小后大。
	if arts[0].URL == "" || arts[0].Base64 != "" {
		t.Errorf("(c) small image: URL=%q Base64.len=%d (want URL non-empty, Base64 empty)",
			arts[0].URL, len(arts[0].Base64))
	}
	if !strings.Contains(arts[0].URL, "/files/") {
		t.Errorf("(c) small image URL must point at /files/: %q", arts[0].URL)
	}
	if arts[0].Mime != "image/png" || arts[0].Bytes != len(small.Data) {
		t.Errorf("(c) small image Mime/Bytes=%s/%d want image/png/%d",
			arts[0].Mime, arts[0].Bytes, len(small.Data))
	}

	if arts[1].URL == "" || arts[1].Base64 != "" {
		t.Errorf("(b) 大产物 video: URL=%q Base64.len=%d (want URL non-empty, Base64 empty)",
			arts[1].URL, len(arts[1].Base64))
	}
	if !strings.Contains(arts[1].URL, "/files/") {
		t.Errorf("大产物 URL 应指向 /files/, got %q", arts[1].URL)
	}
	if arts[1].Mime != "video/mp4" || arts[1].Bytes != len(large.Data) {
		t.Errorf("大产物 Mime/Bytes=%s/%d want video/mp4/%d", arts[1].Mime, arts[1].Bytes, len(large.Data))
	}

	// data(内存原始字节)必须仍被填上,供 GET /v1/media/{id}/content 使用。
	if len(arts[1].data) != len(large.Data) {
		t.Errorf("大产物 data 长度=%d want %d", len(arts[1].data), len(large.Data))
	}

	// (d) 磁盘上应恰好一个大产物文件。旧 bug 会写两份。
	mediaRoot := filepath.Join(tmp, "data", "media")
	got := countFiles(t, mediaRoot)
	if got != 2 {
		t.Fatalf("(d) each artifact must be written exactly once: files=%d want 2; dir=%s", got, mediaRoot)
	}

	// 双保险: cari file .mp4 secara eksplisit (direktori sekarang juga berisi
	// gambar kecil yang ikut disimpan).
	var found string
	_ = filepath.WalkDir(mediaRoot, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".mp4") {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Errorf("tidak ada file .mp4 di %s", mediaRoot)
	} else if b, err := os.ReadFile(found); err != nil {
		t.Fatalf("读取落盘文件: %v", err)
	} else if len(b) != len(large.Data) {
		t.Errorf("落盘文件字节=%d want %d", len(b), len(large.Data))
	}
}

// TestDeriveMediaJobArtifactsCanvasNoContent 验证 canvas 类结果(无产物)不产生
// 任何结构化 artefak:内容里没有链接 -> 列表为空,且不会 panic。
func TestDeriveMediaJobArtifactsCanvasNoContent(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	withSaveMediaToDisk(t, 1)

	// canvas:纯 HTML 文档,无产物。
	html := "<!doctype html><html><body>canvas</body></html>"
	if arts := deriveMediaJobArtifacts(html, nil); arts != nil {
		t.Errorf("无产物的 canvas 应返回 nil, got %+v", arts)
	}

	// 有产物但没有对应链接(异常/裁剪情形)-> 不产生条目。
	a := MediaArtifact{Mime: "audio/mpeg", Data: bytesRepeat(0x43, 32)}
	if arts := deriveMediaJobArtifacts("没有链接的正文", []MediaArtifact{a}); arts != nil {
		t.Errorf("正文无链接时应返回 nil, got %+v", arts)
	}
}

// TestDeriveMediaJobArtifactsCanvasFromLink melengkapi kasus di atas: canvas yang
// BERHASIL punya satu jejak saja — link [canvas](...) di akhir konten, karena
// dokumennya tidak lewat res.Artifacts. Sebelum ini link canvas tidak dikenali
// sama sekali, jadi job canvas selalu selesai dengan artifacts kosong dan galeri
// tidak pernah melihatnya sebagai artefak.
func TestDeriveMediaJobArtifactsCanvasFromLink(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	withSaveMediaToDisk(t, 1)

	// Siapkan file di media dir supaya ukurannya bisa diisi dari disk.
	day := time.Now().Format("2006-01-02")
	dir := filepath.Join(mediaDir(), day)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := []byte("<!doctype html><html><body>halo</body></html>")
	if err := os.WriteFile(filepath.Join(dir, "abc.html"), body, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	content := "Sedikit penjelasan dari model.\n\n[canvas](/files/" + day + "/abc.html)"
	arts := deriveMediaJobArtifacts(content, nil)
	if len(arts) != 1 {
		t.Fatalf("canvas harus menghasilkan 1 artefak, got %d: %+v", len(arts), arts)
	}
	a := arts[0]
	if a.Mime != "text/html" {
		t.Errorf("Mime=%q want text/html", a.Mime)
	}
	if !strings.Contains(a.URL, "/files/") || a.Base64 != "" {
		t.Errorf("URL=%q Base64.len=%d, want URL ke /files/ dan Base64 kosong", a.URL, len(a.Base64))
	}
	if a.Bytes != len(body) {
		t.Errorf("Bytes=%d want %d (dibaca dari disk)", a.Bytes, len(body))
	}
	if len(a.data) != 0 {
		t.Errorf("artefak hasil sintesis tidak boleh membawa byte di memori, got %d", len(a.data))
	}
}

// bytesRepeat 是 bytes.Repeat 的小包装,避免测试文件再多引一个 import。
func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
