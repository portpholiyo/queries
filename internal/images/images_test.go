package images

import (
	"bytes"
	"errors"
	"testing"
)

func TestCheckFormats(t *testing.T) {
	bmp := make([]byte, 40)
	bmp[0], bmp[1], bmp[14] = 'B', 'M', 40
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"png", []byte("\x89PNG\r\n\x1a\n0000"), "image/png"},
		{"gif", []byte("GIF89a0000000"), "image/gif"},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00WEBP"), 'V', 'P', '8', ' '), "image/webp"},
		{"bmp", bmp, "image/bmp"},
		{"tiff", []byte("II*\x00\x08\x00\x00\x00"), "image/tiff"},
		{"avif", []byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1miaf"), "image/avif"},
		{"heic", []byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic"), "image/heic"},
		{"jpeg", []byte("\xff\xd8\xff\xe0\x00\x04ab\xff\xda\x00\x02zz"), "image/jpeg"},
	}
	for _, c := range cases {
		_, info, err := Check(c.data)
		if err != nil || info.MIME != c.mime {
			t.Errorf("%s: got %q, %v; want %q", c.name, info.MIME, err, c.mime)
		}
	}
}

func TestCheckRejects(t *testing.T) {
	bad := map[string][]byte{
		"empty":  {},
		"text":   []byte("hello world, not an image"),
		"svg":    []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"html":   []byte("<html><script>alert(1)</script></html>"),
		"pdf":    []byte("%PDF-1.7 ..."),
		"exe":    []byte("MZ\x90\x00\x03"),
		"bm-txt": []byte("BM is not a bitmap, just text that starts with BM......"),
		"trunc":  []byte("\xff\xd8\xff\xe0\x00"),
	}
	for name, data := range bad {
		if _, _, err := Check(data); !errors.Is(err, ErrNotImage) {
			t.Errorf("%s: expected ErrNotImage, got %v", name, err)
		}
	}
}

func TestJPEGStripsExifButKeepsOtherSegments(t *testing.T) {
	src := []byte("\xff\xd8" +
		"\xff\xe0\x00\x04JF" + // APP0 keep
		"\xff\xe1\x00\x0aExifGPS!" + // APP1 drop
		"\xff\xed\x00\x04ps" + // APP13 drop
		"\xff\xe2\x00\x04IC" + // APP2 (ICC) keep
		"\xff\xda\x00\x02\x01\x02\x03\xff\xd9")
	out, info, err := Check(src)
	if err != nil || info.MIME != "image/jpeg" {
		t.Fatal(err, info)
	}
	if bytes.Contains(out, []byte("GPS")) || bytes.Contains(out, []byte("ps")) {
		t.Error("metadata segments were not removed")
	}
	want := []byte("\xff\xd8\xff\xe0\x00\x04JF\xff\xe2\x00\x04IC\xff\xda\x00\x02\x01\x02\x03\xff\xd9")
	if !bytes.Equal(out, want) {
		t.Errorf("unexpected output %q", out)
	}
}
