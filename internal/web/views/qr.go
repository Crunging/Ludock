package views

import (
	"strconv"
	"strings"

	"github.com/boombuler/barcode/qr"
)

// qrQuiet is the blank border, in modules, that scanners need.
const qrQuiet = 4

// qrPath draws content as a QR code: an SVG path of its dark modules and the
// side length of the square including the border.
func qrPath(content string) (string, int, error) {
	code, err := qr.Encode(content, qr.M, qr.Auto)
	if err != nil {
		return "", 0, err
	}
	size := code.Bounds().Dx()
	dark := func(x, y int) bool { r, _, _, _ := code.At(x, y).RGBA(); return r == 0 }
	var path strings.Builder
	for y := range size {
		for x := 0; x < size; {
			if !dark(x, y) {
				x++
				continue
			}
			run := 1
			for x+run < size && dark(x+run, y) {
				run++
			}
			width := strconv.Itoa(run)
			path.WriteString("M" + strconv.Itoa(x+qrQuiet) + " " + strconv.Itoa(y+qrQuiet) + "h" + width + "v1h-" + width + "z")
			x += run
		}
	}
	return path.String(), size + 2*qrQuiet, nil
}
