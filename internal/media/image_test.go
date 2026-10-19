package media

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// claimSize rewrites the IHDR dimensions of a PNG without allocating pixels.
func claimSize(data []byte, w, h uint32) []byte {
	out := bytes.Clone(data)
	binary.BigEndian.PutUint32(out[16:], w)
	binary.BigEndian.PutUint32(out[20:], h)
	binary.BigEndian.PutUint32(out[29:], crc32.ChecksumIEEE(out[12:29]))
	return out
}

func TestNewAcceptsPNGAndJPEG(t *testing.T) {
	img, err := New(PNG, b64(pngBytes(t, 3, 2)))
	if err != nil || img.MIME() != PNG || img.Width() != 3 || img.Height() != 2 || img.Size() == 0 {
		t.Fatalf("png: %+v %v", img, err)
	}
	if img, err = New(JPEG, b64(jpegBytes(t))); err != nil || img.MIME() != JPEG {
		t.Fatalf("jpeg: %+v %v", img, err)
	}
	if !strings.HasPrefix(img.DataURL(), "data:image/jpeg;base64,") {
		t.Fatalf("data URL %q", img.DataURL())
	}
}

func TestNewRejects(t *testing.T) {
	good := pngBytes(t, 4, 4)
	tests := map[string]struct{ mime, data string }{
		"unsupported type": {"image/gif", b64(good)},
		"bad base64":       {PNG, "not base64!"},
		"mime mismatch":    {JPEG, b64(good)},
		"not an image":     {PNG, b64([]byte("hello"))},
		"truncated":        {PNG, b64(good[:len(good)-20])},
		"too many pixels":  {PNG, b64(claimSize(good, 4000, 4001))},
		"zero dimension":   {PNG, b64(claimSize(good, 0, 4))},
		"encoded too long": {PNG, strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxImageBytes)+8)},
	}
	for name, tt := range tests {
		if _, err := New(tt.mime, tt.data); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNewExactLimits(t *testing.T) {
	// Bytes after IEND are ignored by the decoder, so padding sets the size.
	padded := func(n int) string { return b64(append(pngBytes(t, 4, 4), make([]byte, n)...)) }
	base := len(pngBytes(t, 4, 4))
	if _, err := New(PNG, padded(MaxImageBytes-base)); err != nil {
		t.Errorf("exact byte limit: %v", err)
	}
	if _, err := New(PNG, padded(MaxImageBytes-base+1)); err == nil {
		t.Error("one byte over the limit accepted")
	}
	if _, err := New(PNG, b64(pngBytes(t, 4000, 4000))); err != nil {
		t.Errorf("exact pixel limit: %v", err)
	}
}

func TestJSONRoundTripRevalidates(t *testing.T) {
	img, err := New(PNG, b64(pngBytes(t, 2, 2)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(img)
	if err != nil {
		t.Fatal(err)
	}
	var back Image
	if err := json.Unmarshal(b, &back); err != nil || back.DataURL() != img.DataURL() {
		t.Fatalf("round trip: %v", err)
	}
	for _, bad := range []string{`{"mimeType":"image/png","data":"AAAA"}`, `[]`} {
		if json.Unmarshal([]byte(bad), &back) == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestCheckSet(t *testing.T) {
	img, err := New(PNG, b64(pngBytes(t, 2, 2)))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckSet([]Image{img, img, img, img}); err != nil {
		t.Errorf("four images: %v", err)
	}
	if CheckSet([]Image{img, img, img, img, img}) == nil {
		t.Error("five images accepted")
	}
	big := Image{mime: PNG, data: make([]byte, MaxImageBytes)}
	if CheckSet([]Image{big, big}) != nil {
		t.Error("8 MiB total rejected")
	}
	if CheckSet([]Image{big, big, img}) == nil {
		t.Error("over 8 MiB total accepted")
	}
}
