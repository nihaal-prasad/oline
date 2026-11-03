package tui

import (
	"bytes"
	"image"
	"strings"
	"testing"

	"github.com/mattn/go-sixel"

	"github.com/nihaal-prasad/oline/assets"
)

func TestDA1HasSixel(t *testing.T) {
	cases := []struct {
		resp string
		want bool
	}{
		{"\x1b[?62;4;22c", true}, // VT420 + Sixel(4)
		{"\x1b[?64;4c", true},    // Sixel present
		{"\x1b[?62;22c", false},  // no Sixel capability
		{"\x1b[?1;2c", false},    // VT100-ish, no Sixel
		{"garbage", false},       // no DA1 at all
		{"\x1b[?4c", true},       // just Sixel
	}
	for _, c := range cases {
		if got := da1HasSixel(c.resp); got != c.want {
			t.Errorf("da1HasSixel(%q) = %v, want %v", c.resp, got, c.want)
		}
	}
}

func TestSixelEncodeLogo(t *testing.T) {
	img, _, err := image.Decode(bytes.NewReader(assets.LogoPNG))
	if err != nil {
		t.Fatalf("embedded logo.png did not decode: %v", err)
	}
	scaled := scaleToWidth(img, sixelWidthPx)
	if scaled.Bounds().Dx() != sixelWidthPx || scaled.Bounds().Dy() <= 0 {
		t.Fatalf("scaled bounds wrong: %v", scaled.Bounds())
	}
	var buf bytes.Buffer
	if err := sixel.NewEncoder(&buf).Encode(scaled); err != nil {
		t.Fatalf("sixel encode failed: %v", err)
	}
	out := buf.String()
	// A Sixel payload is introduced by DCS 'q' (ESC P ... q) and ends with ST (ESC \).
	if !strings.Contains(out, "\x1bP") || !strings.Contains(out, "q") || !strings.HasSuffix(out, "\x1b\\") {
		t.Errorf("sixel output missing DCS/ST framing (len=%d)", len(out))
	}
	if len(out) < 1000 {
		t.Errorf("sixel output suspiciously small: %d bytes", len(out))
	}
}
