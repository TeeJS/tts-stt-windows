// gen-icon writes assets/icon.ico: a dark rounded tile with three green waveform bars.
// Run once from the repo root when the icon needs regenerating:
//
//	go run ./tools/gen-icon
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
)

func main() {
	const s = 32
	img := image.NewRGBA(image.Rect(0, 0, s, s))
	bg := color.RGBA{0x13, 0x1f, 0x2c, 0xff}     // open-quake surface color
	accent := color.RGBA{0x7c, 0xff, 0xb2, 0xff} // open-quake accent green
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			// rounded corners: skip pixels outside a radius-6 corner arc
			cx, cy := cornerDist(x, y, s, 6)
			if cx*cx+cy*cy > 36 {
				continue
			}
			img.Set(x, y, bg)
		}
	}
	// three vertical bars, middle tallest -- a minimal waveform glyph
	bars := []struct{ x, h int }{{8, 10}, {14, 20}, {20, 14}}
	for _, b := range bars {
		top := (s - b.h) / 2
		for y := top; y < top+b.h; y++ {
			for x := b.x; x < b.x+4; x++ {
				img.Set(x, y, accent)
			}
		}
	}

	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		log.Fatal(err)
	}
	// ICO container with one PNG-compressed entry (supported since Vista).
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, uint16(0))            // reserved
	binary.Write(&ico, binary.LittleEndian, uint16(1))            // type: icon
	binary.Write(&ico, binary.LittleEndian, uint16(1))            // count
	ico.WriteByte(s)                                              // width
	ico.WriteByte(s)                                              // height
	ico.WriteByte(0)                                              // palette
	ico.WriteByte(0)                                              // reserved
	binary.Write(&ico, binary.LittleEndian, uint16(1))            // planes
	binary.Write(&ico, binary.LittleEndian, uint16(32))           // bpp
	binary.Write(&ico, binary.LittleEndian, uint32(pngBuf.Len())) // data size
	binary.Write(&ico, binary.LittleEndian, uint32(6+16))         // data offset
	ico.Write(pngBuf.Bytes())

	if err := os.MkdirAll("cmd/tts-sst", 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("cmd/tts-sst/icon.ico", ico.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote cmd/tts-sst/icon.ico (%d bytes)", ico.Len())
}

// cornerDist returns the pixel's offset into the nearest corner square of radius r,
// or (0,0) when the pixel is in the tile's straight-edged body.
func cornerDist(x, y, s, r int) (int, int) {
	cx, cy := 0, 0
	if x < r && y < r {
		cx, cy = r-1-x, r-1-y
	} else if x >= s-r && y < r {
		cx, cy = x-(s-r), r-1-y
	} else if x < r && y >= s-r {
		cx, cy = r-1-x, y-(s-r)
	} else if x >= s-r && y >= s-r {
		cx, cy = x-(s-r), y-(s-r)
	}
	return cx, cy
}
