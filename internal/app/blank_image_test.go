package app

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

func TestLooksBlankDetectsPlaceholder(t *testing.T) {
	// rata: satu warna
	flat := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			flat.Set(x, y, color.RGBA{204, 229, 241, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, flat, nil); err != nil {
		t.Fatal(err)
	}
	if !looksBlank(buf.Bytes()) {
		t.Fatal("gambar rata harus dianggap kosong")
	}

	// berderau: gambar nyata
	noisy := image.NewRGBA(image.Rect(0, 0, 64, 64))
	seed := uint32(12345)
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			seed = seed*1664525 + 1013904223
			v := uint8(seed >> 24)
			noisy.Set(x, y, color.RGBA{v, uint8(int(v) / 2), uint8(255 - int(v)), 255})
		}
	}
	buf.Reset()
	if err := jpeg.Encode(&buf, noisy, nil); err != nil {
		t.Fatal(err)
	}
	if looksBlank(buf.Bytes()) {
		t.Fatal("gambar berderau tidak boleh dianggap kosong")
	}
}

func TestLooksBlankReadsRealPlaceholder(t *testing.T) {
	p := filepath.Join("..", "..", "data", "media", "2026-10-30", "d46aebf01f1dafa2.jpg")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Skip("placeholder sample tidak ada")
	}
	if !looksBlank(data) {
		t.Fatal("sample placeholder 30 Okt harus terdeteksi kosong")
	}
}
