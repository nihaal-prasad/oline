package tui

import (
	"bufio"
	"bytes"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-sixel"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"golang.org/x/term"

	"github.com/nihaal-prasad/oline/assets"
)

// sixelWidthPx is the pixel width the banner is scaled to before Sixel encoding
// (height follows the image's aspect). Kept modest so it fits a typical window.
const sixelWidthPx = 760

// bgR/bgG/bgB is the background color the logo's near-black backdrop is snapped
// to, so the Sixel image blends into the app background. It matches theme.colBg
// (#0c0c0c, Windows Terminal's default dark background).
const (
	bgR = 0x0c
	bgG = 0x0c
	bgB = 0x0c
)

const splashPrompt = "Press Enter to Continue..."

// ShowSplash renders the oline banner crisply via Sixel on terminals that
// support it, then waits for the user to press Enter before the TUI starts. It
// returns true when it displayed the Sixel splash (so the caller can skip the
// block-art logo inside the wizard). On any terminal without Sixel — or when not
// attached to a real terminal — it does nothing and returns false, leaving the
// block-art logo to appear in the wizard instead.
func ShowSplash() bool {
	if !sixelSupported() {
		return false
	}
	decoded, _, err := image.Decode(bytes.NewReader(assets.LogoPNG))
	if err != nil {
		return false
	}
	img := scaleToWidth(decoded, sixelWidthPx)
	snapBackground(img) // match the app background so the banner blends in

	cols, _, _ := term.GetSize(int(os.Stdout.Fd()))
	// Logo width in cells (~10px per cell) and prompt width, each centered in the
	// terminal so the prompt sits centered directly below the logo.
	logoCols := img.Bounds().Dx() / 10
	logoPad := centerPad(cols, logoCols)
	promptPad := centerPad(cols, len(splashPrompt))

	var buf bytes.Buffer
	buf.WriteString("\x1b[2J\x1b[H\r\n") // clear screen, home
	if logoPad > 0 {
		buf.WriteString("\x1b[" + itoa(logoPad) + "C")
	}
	enc := sixel.NewEncoder(&buf)
	enc.Colors = 255 // a full palette; without this go-sixel degenerates to ~black
	enc.Dither = true
	if err := enc.Encode(img); err != nil {
		return false
	}
	buf.WriteString("\r\n\r\n")
	buf.WriteString(strings.Repeat(" ", promptPad) + splashPrompt)
	_, _ = os.Stdout.Write(buf.Bytes())

	// Cooked line read; consumes the Enter so the TUI starts clean.
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	_, _ = os.Stdout.WriteString("\x1b[2J\x1b[H")
	return true
}

// sixelSupported reports whether the terminal can render Sixel graphics.
// OLINE_SIXEL=1/0 forces it; otherwise it asks the terminal with a Primary
// Device Attributes (DA1) query and checks for capability 4 (Sixel).
func sixelSupported() bool {
	switch os.Getenv("OLINE_SIXEL") {
	case "1":
		return true
	case "0":
		return false
	}
	inFd := int(os.Stdin.Fd())
	if !term.IsTerminal(inFd) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return false
	}
	old, err := term.MakeRaw(inFd)
	if err != nil {
		return false
	}
	defer term.Restore(inFd, old)

	if _, err := os.Stdout.WriteString("\x1b[c"); err != nil {
		return false
	}
	ch := make(chan string, 1)
	go func() {
		var b []byte
		tmp := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(tmp)
			if n > 0 {
				b = append(b, tmp[0])
				if tmp[0] == 'c' || len(b) > 128 { // DA1 terminator
					break
				}
			}
			if err != nil {
				break
			}
		}
		ch <- string(b)
	}()
	select {
	case resp := <-ch:
		return da1HasSixel(resp)
	case <-time.After(250 * time.Millisecond):
		return false // no/slow response -> assume unsupported
	}
}

// da1HasSixel reports whether a DA1 response (e.g. "\x1b[?62;4;22c") lists
// capability 4 (Sixel) among its parameters.
func da1HasSixel(s string) bool {
	q := strings.IndexByte(s, '?')
	if q < 0 {
		return false
	}
	rest := s[q+1:]
	c := strings.IndexByte(rest, 'c')
	if c < 0 {
		return false
	}
	for _, p := range strings.Split(rest[:c], ";") {
		if p == "4" {
			return true
		}
	}
	return false
}

// scaleToWidth returns img resized to w pixels wide (aspect preserved).
func scaleToWidth(img image.Image, w int) *image.RGBA {
	b := img.Bounds()
	if b.Dx() == 0 {
		w = 1
	}
	h := b.Dy() * w / maxInt(b.Dx(), 1)
	dst := image.NewRGBA(image.Rect(0, 0, w, maxInt(h, 1)))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst
}

// snapBackground replaces the logo's near-black backdrop pixels with the app
// background color, so the Sixel image has no mismatched box around it.
func snapBackground(img *image.RGBA) {
	for i := 0; i+3 < len(img.Pix); i += 4 {
		r, g, b := int(img.Pix[i]), int(img.Pix[i+1]), int(img.Pix[i+2])
		if (r*299+g*587+b*114)/1000 < 20 {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2] = bgR, bgG, bgB
		}
	}
}

// centerPad returns the left padding to center a span of width inner in outer.
func centerPad(outer, inner int) int {
	if outer <= inner {
		return 0
	}
	return (outer - inner) / 2
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// itoa is a tiny base-10 formatter (avoids importing strconv here).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [12]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}
