package postprocess

import "testing"

// TestDetectImageFormat covers each shape we sniff plus a few
// rejection paths. Magic-byte fixtures are minimal — only enough
// bytes to satisfy each branch's length guard.
func TestDetectImageFormat(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
		ok   bool
	}{
		{
			name: "jpeg",
			data: []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'},
			want: "jpg", ok: true,
		},
		{
			name: "png",
			data: []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D},
			want: "png", ok: true,
		},
		{
			name: "heic_brand_heic",
			// 4-byte length, then "ftyp", then "heic" brand
			data: []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c'},
			want: "heic", ok: true,
		},
		{
			name: "heic_brand_mif1",
			data: []byte{0x00, 0x00, 0x00, 0x20, 'f', 't', 'y', 'p', 'm', 'i', 'f', '1'},
			want: "heic", ok: true,
		},
		{
			name: "gif87a",
			data: []byte{'G', 'I', 'F', '8', '7', 'a', 0x00, 0x00},
			want: "gif", ok: true,
		},
		{
			name: "gif89a",
			data: []byte{'G', 'I', 'F', '8', '9', 'a', 0x00, 0x00},
			want: "gif", ok: true,
		},
		{
			name: "webp_rejected", // RIFF....WEBP — Vision handles it but we don't claim to
			data: []byte{'R', 'I', 'F', 'F', 0x00, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P'},
			want: "", ok: false,
		},
		{
			name: "tiff_rejected",
			data: []byte{0x49, 0x49, 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00},
			want: "", ok: false,
		},
		{
			name: "empty",
			data: []byte{},
			want: "", ok: false,
		},
		{
			name: "short_jpeg_prefix", // 2 bytes — fails len>=3 guard
			data: []byte{0xFF, 0xD8},
			want: "", ok: false,
		},
		{
			name: "garbage",
			data: []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C},
			want: "", ok: false,
		},
		{
			name: "almost_png_one_bit_off", // changed one byte of PNG signature
			data: []byte{0x89, 0x50, 0x4E, 0x46, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D},
			want: "", ok: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := detectImageFormat(c.data)
			if got != c.want || ok != c.ok {
				t.Errorf("detectImageFormat(%s) = (%q, %v), want (%q, %v)",
					c.name, got, ok, c.want, c.ok)
			}
		})
	}
}
