package imageinspect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	xdraw "golang.org/x/image/draw"
)

var jpegQualities = []int{90, 82, 74, 66, 58, 50, 42}

func makeThumbnail(source image.Image, maximum int) (Rendition, error) {
	width, height := scaledDimensions(source.Bounds().Dx(), source.Bounds().Dy(), maximum)
	resized := resize(source, width, height)
	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&encoded, resized); err != nil {
		return Rendition{}, fmt.Errorf("encoding thumbnail: %w", err)
	}

	return Rendition{
		MIMEType: "image/png", Extension: ".png", Width: width, Height: height,
		Data: bytes.Clone(encoded.Bytes()),
	}, nil
}

func makeAnalysisRendition(
	ctx context.Context,
	source image.Image,
	preserveTransparency bool,
	maximumDimension int,
	maximumBytes int64,
) (Rendition, error) {
	width, height := scaledDimensions(source.Bounds().Dx(), source.Bounds().Dy(), maximumDimension)
	for {
		if err := ctx.Err(); err != nil {
			return Rendition{}, fmt.Errorf("creating analysis rendition: %w", err)
		}
		resized := resize(source, width, height)
		if preserveTransparency {
			data, err := encodePNGBounded(resized, maximumBytes)
			if err == nil {
				return Rendition{
					MIMEType: "image/png", Extension: ".png", Width: width, Height: height, Data: data,
				}, nil
			}
			if !errors.Is(err, ErrRenditionLimit) {
				return Rendition{}, fmt.Errorf("encoding analysis png: %w", err)
			}
		} else {
			for _, quality := range jpegQualities {
				data, err := encodeJPEGBounded(resized, quality, maximumBytes)
				if err == nil {
					return Rendition{
						MIMEType: "image/jpeg", Extension: ".jpg", Width: width, Height: height, Data: data,
					}, nil
				}
				if !errors.Is(err, ErrRenditionLimit) {
					return Rendition{}, fmt.Errorf("encoding analysis jpeg: %w", err)
				}
			}
		}
		if width == 1 && height == 1 {
			return Rendition{}, ErrRenditionLimit
		}
		width, height = shrinkDimensions(width, height)
	}
}

func resize(source image.Image, width, height int) *image.NRGBA {
	destination := image.NewNRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(destination, destination.Bounds(), source, source.Bounds(), xdraw.Src, nil)

	return destination
}

func scaledDimensions(width, height, maximum int) (int, int) {
	if width <= maximum && height <= maximum {
		return width, height
	}
	if width >= height {
		return maximum, max(1, int(int64(height)*int64(maximum)/int64(width)))
	}

	return max(1, int(int64(width)*int64(maximum)/int64(height))), maximum
}

func shrinkDimensions(width, height int) (int, int) {
	if width >= height {
		return max(1, width*3/4), max(1, int(int64(height)*int64(max(1, width*3/4))/int64(width)))
	}
	newHeight := max(1, height*3/4)

	return max(1, int(int64(width)*int64(newHeight)/int64(height))), newHeight
}

func encodePNGBounded(source image.Image, maximum int64) ([]byte, error) {
	buffer := newLimitedBuffer(maximum)
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(buffer, source); err != nil {
		return nil, err
	}

	return bytes.Clone(buffer.Bytes()), nil
}

func encodeJPEGBounded(source image.Image, quality int, maximum int64) ([]byte, error) {
	buffer := newLimitedBuffer(maximum)
	if err := jpeg.Encode(buffer, source, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}

	return bytes.Clone(buffer.Bytes()), nil
}

type limitedBuffer struct {
	buffer  bytes.Buffer
	maximum int64
}

func newLimitedBuffer(maximum int64) *limitedBuffer {
	return &limitedBuffer{maximum: maximum}
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	remaining := buffer.maximum - int64(buffer.buffer.Len())
	if remaining <= 0 || int64(len(data)) > remaining {
		return 0, ErrRenditionLimit
	}

	return buffer.buffer.Write(data)
}

func (buffer *limitedBuffer) Bytes() []byte {
	return buffer.buffer.Bytes()
}

var _ io.Writer = (*limitedBuffer)(nil)
