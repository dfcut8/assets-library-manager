package imageinspect

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"testing"
)

func TestParseTIFFOrientationSupportsBothByteOrders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		order binary.ByteOrder
		mark  string
	}{
		{name: "little endian", order: binary.LittleEndian, mark: "II"},
		{name: "big endian", order: binary.BigEndian, mark: "MM"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			payload := make([]byte, 26)
			copy(payload[:2], test.mark)
			test.order.PutUint16(payload[2:4], 42)
			test.order.PutUint32(payload[4:8], 8)
			test.order.PutUint16(payload[8:10], 1)
			test.order.PutUint16(payload[10:12], 0x0112)
			test.order.PutUint16(payload[12:14], 3)
			test.order.PutUint32(payload[14:18], 1)
			test.order.PutUint16(payload[18:20], 8)
			orientation, ok := parseTIFFOrientation(payload)
			if !ok || orientation != 8 {
				t.Fatalf("parseTIFFOrientation() = %d, %v", orientation, ok)
			}
		})
	}
}

func TestApplyOrientationMapsEveryEXIFTransform(t *testing.T) {
	t.Parallel()

	source := imageWithDistinctCorners()
	tests := []struct {
		orientation int
		width       int
		height      int
		topLeft     uint8
	}{
		{orientation: 1, width: 2, height: 3, topLeft: 1},
		{orientation: 2, width: 2, height: 3, topLeft: 2},
		{orientation: 3, width: 2, height: 3, topLeft: 6},
		{orientation: 4, width: 2, height: 3, topLeft: 5},
		{orientation: 5, width: 3, height: 2, topLeft: 1},
		{orientation: 6, width: 3, height: 2, topLeft: 5},
		{orientation: 7, width: 3, height: 2, topLeft: 6},
		{orientation: 8, width: 3, height: 2, topLeft: 2},
	}
	for _, test := range tests {
		result := applyOrientation(source, test.orientation)
		if result.Bounds().Dx() != test.width || result.Bounds().Dy() != test.height || result.NRGBAAt(0, 0).R != test.topLeft {
			t.Errorf("orientation %d result bounds=%v top-left=%d", test.orientation, result.Bounds(), result.NRGBAAt(0, 0).R)
		}
	}
}

func TestPixelNRGBAMatchesStandardColorConversion(t *testing.T) {
	t.Parallel()

	nrgba := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	nrgba.SetNRGBA(0, 0, color.NRGBA{R: 240, G: 120, B: 60, A: 128})
	rgba := image.NewRGBA(image.Rect(0, 0, 1, 1))
	rgba.SetRGBA(0, 0, color.RGBA{R: 120, G: 60, B: 30, A: 128})
	nrgba64 := image.NewNRGBA64(image.Rect(0, 0, 1, 1))
	nrgba64.SetNRGBA64(0, 0, color.NRGBA64{R: 0xf0f0, G: 0x7878, B: 0x3c3c, A: 0x8080})
	rgba64 := image.NewRGBA64(image.Rect(0, 0, 1, 1))
	rgba64.SetRGBA64(0, 0, color.RGBA64{R: 0x7878, G: 0x3c3c, B: 0x1e1e, A: 0x8080})
	ycbcr := image.NewYCbCr(image.Rect(0, 0, 1, 1), image.YCbCrSubsampleRatio444)
	ycbcr.Y[0], ycbcr.Cb[0], ycbcr.Cr[0] = 100, 140, 120
	gray := image.NewGray(image.Rect(0, 0, 1, 1))
	gray.SetGray(0, 0, color.Gray{Y: 90})
	paletted := image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{
		color.NRGBA{R: 40, G: 80, B: 120, A: 160},
	})

	for _, source := range []image.Image{nrgba, rgba, nrgba64, rgba64, ycbcr, gray, paletted} {
		want := color.NRGBAModel.Convert(source.At(0, 0)).(color.NRGBA)
		if got := pixelNRGBA(source, 0, 0); got != want {
			t.Errorf("pixelNRGBA(%T) = %#v, want %#v", source, got, want)
		}
	}
}

func FuzzParseTIFFOrientationDoesNotPanic(f *testing.F) {
	f.Add([]byte("II*\x00\x08\x00\x00\x00"))
	f.Add([]byte("Exif\x00\x00MM\x00*\x00\x00\x00\x08"))
	f.Fuzz(func(t *testing.T, payload []byte) {
		orientation, ok := parseTIFFOrientation(payload)
		if ok && (orientation < 1 || orientation > 8) {
			t.Fatalf("orientation = %d", orientation)
		}
	})
}

func FuzzReadWebPMetadataDoesNotPanic(f *testing.F) {
	f.Add([]byte("RIFF\x04\x00\x00\x00WEBP"))
	f.Add([]byte("RIFF\x16\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"))
	f.Fuzz(func(_ *testing.T, payload []byte) {
		_, _ = readWebPMetadata(bytes.NewReader(payload), int64(len(payload)))
	})
}

func TestReadVP8LAlphaFlag(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		bits uint32
		want bool
	}{
		{name: "opaque", bits: 0, want: false},
		{name: "alpha used", bits: 1 << 28, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			header := make([]byte, 5)
			header[0] = 0x2f
			binary.LittleEndian.PutUint32(header[1:], test.bits)
			got, err := readVP8LAlphaFlag(bytes.NewReader(header), int64(len(header)))
			if err != nil || got != test.want {
				t.Fatalf("readVP8LAlphaFlag() = %v, %v", got, err)
			}
		})
	}
}

func imageWithDistinctCorners() *image.NRGBA {
	imageValue := image.NewNRGBA(image.Rect(0, 0, 2, 3))
	value := uint8(1)
	for y := range 3 {
		for x := range 2 {
			imageValue.SetNRGBA(x, y, color.NRGBA{R: value, A: 0xff})
			value++
		}
	}

	return imageValue
}
