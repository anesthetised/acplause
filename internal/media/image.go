// Package media validates the images that travel through model history.
package media

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
)

// Bounds from ADR 0009. The set limits apply to the images of one prompt or
// one tool result.
const (
	MaxImageBytes = 4 << 20
	MaxPixels     = 16_000_000
	MaxImages     = 4
	MaxSetBytes   = 8 << 20
)

// Supported MIME types.
const (
	PNG  = "image/png"
	JPEG = "image/jpeg"
)

// Image is a validated PNG or JPEG. It is immutable: the byte slice is never
// exposed, and the zero value is not a valid image.
type Image struct {
	mime          string
	data          []byte
	width, height int
}

// New validates base64 data claimed to be of type mime. The encoded length is
// checked before anything is decoded, and the declared dimensions before the
// pixels are, so a hostile header cannot make the validator allocate its
// claimed area.
func New(mime, encoded string) (Image, error) {
	if mime != PNG && mime != JPEG {
		return Image{}, fmt.Errorf("unsupported image type %q: only %s and %s", mime, PNG, JPEG)
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > MaxImageBytes+2 {
		return Image{}, fmt.Errorf("image exceeds %d bytes", MaxImageBytes)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return Image{}, fmt.Errorf("image is not valid base64: %w", err)
	}
	if len(data) > MaxImageBytes {
		return Image{}, fmt.Errorf("image exceeds %d bytes", MaxImageBytes)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Image{}, fmt.Errorf("image is not a valid PNG or JPEG: %w", err)
	}
	if want := map[string]string{PNG: "png", JPEG: "jpeg"}[mime]; format != want {
		return Image{}, fmt.Errorf("image is %s, declared %s", format, mime)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > MaxPixels {
		return Image{}, fmt.Errorf("image of %dx%d exceeds %d pixels", cfg.Width, cfg.Height, MaxPixels)
	}
	if format == "png" {
		_, err = png.Decode(bytes.NewReader(data))
	} else {
		_, err = jpeg.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return Image{}, fmt.Errorf("image is corrupt: %w", err)
	}
	return Image{mime: mime, data: data, width: cfg.Width, height: cfg.Height}, nil
}

// MIME returns the image type.
func (i Image) MIME() string { return i.mime }

// Width returns the width in pixels.
func (i Image) Width() int { return i.width }

// Height returns the height in pixels.
func (i Image) Height() int { return i.height }

// Size returns the decoded byte length.
func (i Image) Size() int { return len(i.data) }

// DataURL returns the image as a data URL for Chat Completions image_url parts.
func (i Image) DataURL() string {
	return "data:" + i.mime + ";base64," + base64.StdEncoding.EncodeToString(i.data)
}

type imageJSON struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

// MarshalJSON encodes the image as it is persisted.
func (i Image) MarshalJSON() ([]byte, error) {
	return json.Marshal(imageJSON{MIMEType: i.mime, Data: base64.StdEncoding.EncodeToString(i.data)})
}

// UnmarshalJSON validates a persisted image again, so a damaged row fails to
// load instead of reaching the model.
func (i *Image) UnmarshalJSON(b []byte) error {
	var raw imageJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	img, err := New(raw.MIMEType, raw.Data)
	if err != nil {
		return err
	}
	*i = img
	return nil
}

// CheckSet applies the count and total size limits of one prompt or tool result.
func CheckSet(images []Image) error {
	if len(images) > MaxImages {
		return fmt.Errorf("%d images exceed the limit of %d", len(images), MaxImages)
	}
	total := 0
	for _, img := range images {
		total += img.Size()
	}
	if total > MaxSetBytes {
		return errors.New("images exceed the total size limit")
	}
	return nil
}
