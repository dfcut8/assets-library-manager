package imageinspect

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func BenchmarkInspectorPNG(b *testing.B) {
	source := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	for y := range 512 {
		for x := range 512 {
			source.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x), G: uint8(y), B: uint8(x + y), A: 0xff,
			})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		b.Fatal(err)
	}
	data := encoded.Bytes()
	inspector := New()
	limits := testLimits(".png")
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, err := inspector.Inspect(context.Background(), bytes.NewReader(data), limits); err != nil {
			b.Fatal(err)
		}
	}
}
