package imageinspect

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"testing"

	"golang.org/x/image/webp"
)

const losslessWebPBase64 = "UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZlgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAXFOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8yZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wOK3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA=="

func TestInspectorInspectPNGTransparencyPaletteAndDerivatives(t *testing.T) {
	t.Parallel()

	source := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	for x := range 4 {
		source.SetNRGBA(x, 0, color.NRGBA{R: 0xff, A: 0xff})
	}
	source.SetNRGBA(0, 1, color.NRGBA{B: 0xff, A: 0xff})
	source.SetNRGBA(1, 1, color.NRGBA{B: 0xff, A: 0xff})
	source.SetNRGBA(2, 1, color.NRGBA{G: 0xff, A: 0x00})
	source.SetNRGBA(3, 1, color.NRGBA{G: 0xff, A: 0x00})
	data := encodePNG(t, source)

	inspection, err := New().Inspect(context.Background(), bytes.NewReader(data), testLimits(".PNG"))
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if inspection.Format != "png" || inspection.MIMEType != "image/png" ||
		inspection.DisplayWidth != 4 || inspection.DisplayHeight != 2 ||
		!inspection.HasAlpha || !inspection.HasTransparency || inspection.EncodedAnimated ||
		inspection.EncodedFrameCount != 1 || inspection.OrientationClass != "landscape" {
		t.Fatalf("Inspect() = %+v", inspection)
	}
	if len(inspection.DominantColors) != 2 || inspection.DominantColors[0].Hex != "#FF0000" ||
		inspection.DominantColors[0].Samples != 4 || inspection.DominantColors[1].Hex != "#0000FF" {
		t.Fatalf("dominant colors = %+v", inspection.DominantColors)
	}
	if inspection.Thumbnail.Width != 4 || inspection.Thumbnail.Height != 2 ||
		inspection.Thumbnail.MIMEType != "image/png" {
		t.Fatalf("thumbnail = %+v", inspection.Thumbnail)
	}
	if _, err := png.Decode(bytes.NewReader(inspection.Thumbnail.Data)); err != nil {
		t.Fatalf("decoding thumbnail: %v", err)
	}
	if inspection.Analysis.MIMEType != "image/png" || int64(len(inspection.Analysis.Data)) > testLimits(".png").MaxAnalysisBytes {
		t.Fatalf("analysis rendition = %+v", inspection.Analysis)
	}
}

func TestInspectorInspectIndexedPNGTransparencyWithoutAlphaChannel(t *testing.T) {
	t.Parallel()

	palette := color.Palette{
		color.NRGBA{R: 0xff, A: 0xff},
		color.NRGBA{G: 0xff, A: 0x00},
	}
	source := image.NewPaletted(image.Rect(0, 0, 2, 1), palette)
	source.Pix = []byte{0, 1}
	data := encodePNG(t, source)

	inspection, err := New().Inspect(context.Background(), bytes.NewReader(data), testLimits(".png"))
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if inspection.HasAlpha || !inspection.HasTransparency {
		t.Fatalf("Inspect() alpha metadata = (%t, %t), want (false, true)",
			inspection.HasAlpha, inspection.HasTransparency)
	}
}

func TestInspectorInspectJPEGAppliesEXIFOrientation(t *testing.T) {
	t.Parallel()

	source := image.NewNRGBA(image.Rect(0, 0, 2, 3))
	for y := range 3 {
		for x := range 2 {
			source.SetNRGBA(x, y, color.NRGBA{R: uint8(40 + x*80), G: uint8(30 + y*60), B: 20, A: 0xff})
		}
	}
	data := injectJPEGEXIFOrientation(t, encodeJPEG(t, source), 6)
	inspection, err := New().Inspect(context.Background(), bytes.NewReader(data), testLimits(".jpeg"))
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if inspection.Format != "jpeg" || inspection.Orientation != 6 ||
		inspection.EncodedWidth != 2 || inspection.EncodedHeight != 3 ||
		inspection.DisplayWidth != 3 || inspection.DisplayHeight != 2 ||
		inspection.HasAlpha || inspection.HasTransparency || inspection.Analysis.MIMEType != "image/jpeg" {
		t.Fatalf("Inspect() = %+v", inspection)
	}
}

func TestInspectorInspectAnimatedGIFCountsFrames(t *testing.T) {
	t.Parallel()

	palette := color.Palette{color.NRGBA{A: 0}, color.NRGBA{R: 0xff, A: 0xff}}
	first := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	second := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	first.Pix = []byte{1, 1, 0, 0}
	second.Pix = []byte{0, 0, 1, 1}
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &gif.GIF{
		Image: []*image.Paletted{first, second}, Delay: []int{10, 10}, LoopCount: 0,
		Config: image.Config{ColorModel: palette, Width: 2, Height: 2},
	}); err != nil {
		t.Fatal(err)
	}
	inspection, err := New().Inspect(context.Background(), bytes.NewReader(encoded.Bytes()), testLimits(".gif"))
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !inspection.EncodedAnimated || inspection.EncodedFrameCount != 2 ||
		!inspection.HasAlpha || !inspection.HasTransparency {
		t.Fatalf("Inspect() = %+v", inspection)
	}
}

func TestInspectorInspectStaticAndAnimatedWebP(t *testing.T) {
	t.Parallel()

	static := decodeLosslessWebP(t)
	staticInspection, err := New().Inspect(context.Background(), bytes.NewReader(static), testLimits(".webp"))
	if err != nil {
		t.Fatalf("Inspect(static) error = %v", err)
	}
	if staticInspection.Format != "webp" || staticInspection.EncodedAnimated ||
		staticInspection.EncodedFrameCount != 1 || staticInspection.HasAlpha {
		t.Fatalf("Inspect(static) = %+v", staticInspection)
	}

	animated := makeAnimatedWebP(t, static, 2)
	animatedInspection, err := New().Inspect(context.Background(), bytes.NewReader(animated), testLimits(".webp"))
	if err != nil {
		t.Fatalf("Inspect(animated) error = %v", err)
	}
	if !animatedInspection.EncodedAnimated || animatedInspection.EncodedFrameCount != 2 ||
		animatedInspection.DisplayWidth != staticInspection.DisplayWidth ||
		animatedInspection.DisplayHeight != staticInspection.DisplayHeight {
		t.Fatalf("Inspect(animated) = %+v", animatedInspection)
	}
}

func TestInspectorRejectsFormatSourcePixelAndRenditionLimits(t *testing.T) {
	t.Parallel()

	data := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 4, 2)))
	tests := []struct {
		name   string
		limits Limits
		want   error
	}{
		{name: "extension mismatch", limits: testLimits(".jpg"), want: ErrFormatMismatch},
		{name: "source bytes", limits: func() Limits { value := testLimits(".png"); value.MaxSourceBytes = int64(len(data) - 1); return value }(), want: ErrSourceLimit},
		{name: "pixels", limits: func() Limits { value := testLimits(".png"); value.MaxImagePixels = 7; return value }(), want: ErrPixelLimit},
		{name: "rendition bytes", limits: func() Limits { value := testLimits(".png"); value.MaxAnalysisBytes = 1; return value }(), want: ErrRenditionLimit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := New().Inspect(context.Background(), bytes.NewReader(data), test.limits)
			if !errors.Is(err, test.want) {
				t.Fatalf("Inspect() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestInspectorContainsDecoderPanic(t *testing.T) {
	t.Parallel()

	_, err := New().Inspect(context.Background(), panicReadSeeker{}, testLimits(".png"))
	if !errors.Is(err, ErrDecoderPanic) {
		t.Fatalf("Inspect() error = %v", err)
	}
}

func TestInspectorHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New().Inspect(ctx, bytes.NewReader(encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 1, 1)))), testLimits(".png"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Inspect() error = %v", err)
	}
}

func testLimits(extension string) Limits {
	return Limits{
		MaxSourceBytes: 1 << 20, MaxImagePixels: 1 << 20,
		ThumbnailMaxDimension: 32, AnalysisMaxDimension: 64,
		MaxAnalysisBytes: 1 << 20, ExpectedExtension: extension,
	}
}

func encodePNG(t *testing.T, source image.Image) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}

	return encoded.Bytes()
}

func encodeJPEG(t *testing.T, source image.Image) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, source, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}

	return encoded.Bytes()
}

func injectJPEGEXIFOrientation(t *testing.T, encoded []byte, orientation uint16) []byte {
	t.Helper()
	if len(encoded) < 2 || encoded[0] != 0xff || encoded[1] != 0xd8 {
		t.Fatal("jpeg is missing start marker")
	}
	tiff := make([]byte, 26)
	copy(tiff[:2], "II")
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	binary.LittleEndian.PutUint16(tiff[8:10], 1)
	binary.LittleEndian.PutUint16(tiff[10:12], 0x0112)
	binary.LittleEndian.PutUint16(tiff[12:14], 3)
	binary.LittleEndian.PutUint32(tiff[14:18], 1)
	binary.LittleEndian.PutUint16(tiff[18:20], orientation)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segment := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:4], uint16(len(payload)+2))
	segment = append(segment, payload...)
	result := make([]byte, 0, len(encoded)+len(segment))
	result = append(result, encoded[:2]...)
	result = append(result, segment...)
	result = append(result, encoded[2:]...)

	return result
}

func decodeLosslessWebP(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(losslessWebPBase64)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func makeAnimatedWebP(t *testing.T, static []byte, frameCount int) []byte {
	t.Helper()
	config, err := webp.DecodeConfig(bytes.NewReader(static))
	if err != nil {
		t.Fatal(err)
	}
	if len(static) < 20 || string(static[:4]) != "RIFF" || string(static[8:12]) != "WEBP" {
		t.Fatal("invalid static webp fixture")
	}
	framePayload := static[12:]
	var body bytes.Buffer
	body.WriteString("WEBP")
	vp8x := make([]byte, 10)
	vp8x[0] = 1 << 1
	writeUint24(vp8x[4:7], uint32(config.Width-1))
	writeUint24(vp8x[7:10], uint32(config.Height-1))
	writeRIFFChunk(t, &body, "VP8X", vp8x)
	writeRIFFChunk(t, &body, "ANIM", make([]byte, 6))
	for range frameCount {
		frameHeader := make([]byte, 16)
		writeUint24(frameHeader[6:9], uint32(config.Width-1))
		writeUint24(frameHeader[9:12], uint32(config.Height-1))
		frameHeader[12] = 10
		frameData := append(frameHeader, framePayload...)
		writeRIFFChunk(t, &body, "ANMF", frameData)
	}
	var result bytes.Buffer
	result.WriteString("RIFF")
	if body.Len() > int(^uint32(0)) {
		t.Fatal("animated fixture too large")
	}
	if err := binary.Write(&result, binary.LittleEndian, uint32(body.Len())); err != nil {
		t.Fatal(err)
	}
	result.Write(body.Bytes())

	return result.Bytes()
}

func writeRIFFChunk(t *testing.T, destination *bytes.Buffer, name string, data []byte) {
	t.Helper()
	if len(name) != 4 {
		t.Fatal("riff chunk name must have four bytes")
	}
	destination.WriteString(name)
	if err := binary.Write(destination, binary.LittleEndian, uint32(len(data))); err != nil {
		t.Fatal(err)
	}
	destination.Write(data)
	if len(data)%2 != 0 {
		destination.WriteByte(0)
	}
}

type panicReadSeeker struct{}

func (panicReadSeeker) Read([]byte) (int, error) { panic("decoder panic") }
func (panicReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		return 100, nil
	}

	return offset, nil
}
