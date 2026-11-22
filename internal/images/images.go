// Package images validates uploaded images by content (never by filename or
// client-declared type) and removes embedded metadata where it can do so safely.
package images

import (
	"bytes"
	"errors"
	"fmt"
)

// ErrNotImage is returned when data is not a supported image.
var ErrNotImage = errors.New("not a supported image")

// Info describes a validated image.
type Info struct {
	MIME string
	Ext  string
	// Inline is true when browsers can display the format directly.
	Inline bool
}

// Check sniffs data and returns the (possibly cleaned) bytes to store.
// SVG is deliberately unsupported: it can carry scripts.
func Check(data []byte) ([]byte, Info, error) {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return data, Info{"image/png", "png", true}, nil
	case len(data) > 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		clean, err := stripJPEG(data)
		if err != nil {
			return nil, Info{}, fmt.Errorf("%w: %v", ErrNotImage, err)
		}
		return clean, Info{"image/jpeg", "jpg", true}, nil
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return data, Info{"image/gif", "gif", true}, nil
	case len(data) > 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return data, Info{"image/webp", "webp", true}, nil
	case isBMP(data):
		return data, Info{"image/bmp", "bmp", true}, nil
	case bytes.HasPrefix(data, []byte("II*\x00")), bytes.HasPrefix(data, []byte("MM\x00*")):
		return data, Info{"image/tiff", "tiff", false}, nil
	}
	if mime, ext, ok := isoBMFF(data); ok {
		return data, Info{mime, ext, mime == "image/avif"}, nil
	}
	return nil, Info{}, ErrNotImage
}

func isBMP(d []byte) bool {
	if len(d) < 26 || d[0] != 'B' || d[1] != 'M' {
		return false
	}
	switch uint32(d[14]) | uint32(d[15])<<8 | uint32(d[16])<<16 | uint32(d[17])<<24 {
	case 12, 40, 52, 56, 64, 108, 124:
		return true
	}
	return false
}

// isoBMFF recognises AVIF and HEIC/HEIF by their "ftyp" brands.
func isoBMFF(d []byte) (mime, ext string, ok bool) {
	if len(d) < 16 || string(d[4:8]) != "ftyp" {
		return "", "", false
	}
	size := int(d[0])<<24 | int(d[1])<<16 | int(d[2])<<8 | int(d[3])
	if size < 16 || size > len(d) {
		size = min(len(d), 64)
	}
	brands := []string{string(d[8:12])}
	for i := 16; i+4 <= size; i += 4 {
		brands = append(brands, string(d[i:i+4]))
	}
	heic := false
	for _, b := range brands {
		switch b {
		case "avif", "avis":
			return "image/avif", "avif", true
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1":
			heic = true
		}
	}
	if heic {
		return "image/heic", "heic", true
	}
	return "", "", false
}

// stripJPEG removes EXIF/XMP (APP1) and Photoshop (APP13) segments, which can
// hold GPS position and other personal data. Image data is copied untouched.
func stripJPEG(d []byte) ([]byte, error) {
	out := make([]byte, 0, len(d))
	out = append(out, d[0], d[1]) // SOI
	i := 2
	for {
		if i+4 > len(d) {
			return nil, errors.New("truncated jpeg")
		}
		if d[i] != 0xFF {
			return nil, errors.New("bad jpeg marker")
		}
		m := d[i+1]
		if m == 0xFF { // fill byte
			i++
			continue
		}
		segLen := int(d[i+2])<<8 | int(d[i+3])
		if segLen < 2 || i+2+segLen > len(d) {
			return nil, errors.New("bad jpeg segment")
		}
		if m == 0xDA { // start of scan: the rest is entropy-coded data
			return append(out, d[i:]...), nil
		}
		if m != 0xE1 && m != 0xED {
			out = append(out, d[i:i+2+segLen]...)
		}
		i += 2 + segLen
	}
}
