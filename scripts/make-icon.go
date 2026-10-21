//go:build ignore

// Generates TaskView's shared application, installer and notification-area icon.
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
)

func main() {
	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	var images [][]byte
	for _, n := range sizes {
		im := image.NewNRGBA(image.Rect(0, 0, n, n))
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				u, v := float64(x)/float64(n), float64(y)/float64(n)
				if u < 0.04 || v < 0.04 || u > 0.96 || v > 0.96 {
					continue
				}
				c := color.NRGBA{R: 37, G: 99, B: 235, A: 255}
				for row := 0; row < 3; row++ {
					top := 0.23 + float64(row)*0.21
					if v >= top && v < top+0.10 && ((u >= 0.23 && u < 0.33) || (u >= 0.43 && u < 0.78)) {
						c = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
					}
				}
				im.SetNRGBA(x, y, c)
			}
		}
		var b bytes.Buffer
		if err := png.Encode(&b, im); err != nil {
			panic(err)
		}
		images = append(images, b.Bytes())
	}
	var ico bytes.Buffer
	for _, v := range []uint16{0, 1, uint16(len(sizes))} {
		_ = binary.Write(&ico, binary.LittleEndian, v)
	}
	offset := 6 + 16*len(sizes)
	for i, n := range sizes {
		width := byte(n)
		if n == 256 {
			width = 0
		}
		ico.Write([]byte{width, width, 0, 0})
		for _, v := range []uint16{1, 32} {
			_ = binary.Write(&ico, binary.LittleEndian, v)
		}
		_ = binary.Write(&ico, binary.LittleEndian, uint32(len(images[i])))
		_ = binary.Write(&ico, binary.LittleEndian, uint32(offset))
		offset += len(images[i])
	}
	for _, data := range images {
		ico.Write(data)
	}
	if err := os.WriteFile("resources/taskview.ico", ico.Bytes(), 0o644); err != nil {
		panic(err)
	}
}
