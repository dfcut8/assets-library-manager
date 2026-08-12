package imageinspect

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"slices"
)

const (
	maxPaletteSamples         = 1 << 20
	effectivelyTransparentMax = 16
)

func applyOrientation(source image.Image, orientation int) *image.NRGBA {
	bounds := source.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	destinationWidth := width
	destinationHeight := height
	if orientation >= 5 && orientation <= 8 {
		destinationWidth, destinationHeight = height, width
	}
	destination := image.NewNRGBA(image.Rect(0, 0, destinationWidth, destinationHeight))
	for y := range height {
		for x := range width {
			destinationX, destinationY := orientedPoint(x, y, width, height, orientation)
			destination.SetNRGBA(
				destinationX,
				destinationY,
				pixelNRGBA(source, bounds.Min.X+x, bounds.Min.Y+y),
			)
		}
	}

	return destination
}

func orientedPoint(x, y, width, height, orientation int) (int, int) {
	switch orientation {
	case 2:
		return width - 1 - x, y
	case 3:
		return width - 1 - x, height - 1 - y
	case 4:
		return x, height - 1 - y
	case 5:
		return y, x
	case 6:
		return height - 1 - y, x
	case 7:
		return height - 1 - y, width - 1 - x
	case 8:
		return y, width - 1 - x
	default:
		return x, y
	}
}

func inspectPixels(ctx context.Context, source *image.NRGBA) (bool, []DominantColor, error) {
	bounds := source.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	totalPixels := int64(width) * int64(height)
	stride := max(int64(1), (totalPixels+maxPaletteSamples-1)/maxPaletteSamples)
	histogram := make(map[uint16]uint64, min(int(totalPixels/stride), 1<<15))
	hasTransparency := false
	var linearIndex int64
	for y := range height {
		if err := ctx.Err(); err != nil {
			return false, nil, fmt.Errorf("sampling image pixels: %w", err)
		}
		rowOffset := source.PixOffset(bounds.Min.X, bounds.Min.Y+y)
		for x := range width {
			pixelOffset := rowOffset + x*4
			red := source.Pix[pixelOffset]
			green := source.Pix[pixelOffset+1]
			blue := source.Pix[pixelOffset+2]
			alpha := source.Pix[pixelOffset+3]
			if alpha < 0xff {
				hasTransparency = true
			}
			if linearIndex%stride == 0 && alpha > effectivelyTransparentMax {
				key := uint16(red>>3)<<10 | uint16(green>>3)<<5 | uint16(blue>>3)
				histogram[key]++
			}
			linearIndex++
		}
	}

	type colorCount struct {
		key   uint16
		count uint64
	}
	counts := make([]colorCount, 0, len(histogram))
	for key, count := range histogram {
		counts = append(counts, colorCount{key: key, count: count})
	}
	slices.SortFunc(counts, func(left, right colorCount) int {
		if left.count != right.count {
			if left.count > right.count {
				return -1
			}
			return 1
		}
		return int(left.key) - int(right.key)
	})
	if len(counts) > 5 {
		counts = counts[:5]
	}
	colors := make([]DominantColor, 0, len(counts))
	for _, count := range counts {
		red := expandFiveBits(uint8(count.key >> 10))
		green := expandFiveBits(uint8(count.key >> 5 & 0x1f))
		blue := expandFiveBits(uint8(count.key & 0x1f))
		colors = append(colors, DominantColor{
			Hex: fmt.Sprintf("#%02X%02X%02X", red, green, blue), Samples: count.count,
		})
	}

	return hasTransparency, colors, nil
}

// pixelNRGBA keeps the configured high-pixel inspection path out of allocating color-model interfaces.
func pixelNRGBA(source image.Image, x, y int) color.NRGBA {
	switch typed := source.(type) {
	case *image.NRGBA:
		return typed.NRGBAAt(x, y)
	case *image.NRGBA64:
		pixel := typed.NRGBA64At(x, y)

		return color.NRGBA{R: uint8(pixel.R >> 8), G: uint8(pixel.G >> 8), B: uint8(pixel.B >> 8), A: uint8(pixel.A >> 8)}
	case *image.RGBA:
		pixel := typed.RGBAAt(x, y)

		return unpremultiplyRGBA(pixel.R, pixel.G, pixel.B, pixel.A)
	case *image.RGBA64:
		pixel := typed.RGBA64At(x, y)

		return unpremultiplyRGBA64(pixel.R, pixel.G, pixel.B, pixel.A)
	case *image.YCbCr:
		pixel := typed.YCbCrAt(x, y)
		red, green, blue := color.YCbCrToRGB(pixel.Y, pixel.Cb, pixel.Cr)

		return color.NRGBA{R: red, G: green, B: blue, A: 0xff}
	case *image.Gray:
		value := typed.GrayAt(x, y).Y

		return color.NRGBA{R: value, G: value, B: value, A: 0xff}
	case *image.Gray16:
		value := uint8(typed.Gray16At(x, y).Y >> 8)

		return color.NRGBA{R: value, G: value, B: value, A: 0xff}
	default:
		return colorToNRGBA(source.At(x, y))
	}
}

func unpremultiplyRGBA(red, green, blue, alpha uint8) color.NRGBA {
	if alpha == 0 {
		return color.NRGBA{}
	}
	if alpha == 0xff {
		return color.NRGBA{R: red, G: green, B: blue, A: alpha}
	}

	return color.NRGBA{
		R: uint8(min(uint32(0xff), uint32(red)*0xff/uint32(alpha))),
		G: uint8(min(uint32(0xff), uint32(green)*0xff/uint32(alpha))),
		B: uint8(min(uint32(0xff), uint32(blue)*0xff/uint32(alpha))),
		A: alpha,
	}
}

func unpremultiplyRGBA64(red, green, blue, alpha uint16) color.NRGBA {
	if alpha == 0 {
		return color.NRGBA{}
	}
	if alpha == 0xffff {
		return color.NRGBA{R: uint8(red >> 8), G: uint8(green >> 8), B: uint8(blue >> 8), A: 0xff}
	}

	return color.NRGBA{
		R: uint8(min(uint64(0xffff), uint64(red)*0xffff/uint64(alpha)) >> 8),
		G: uint8(min(uint64(0xffff), uint64(green)*0xffff/uint64(alpha)) >> 8),
		B: uint8(min(uint64(0xffff), uint64(blue)*0xffff/uint64(alpha)) >> 8),
		A: uint8(alpha >> 8),
	}
}

func colorToNRGBA(pixel color.Color) color.NRGBA {
	red, green, blue, alpha := pixel.RGBA()
	return unpremultiplyRGBA64(uint16(red), uint16(green), uint16(blue), uint16(alpha))
}

func expandFiveBits(value uint8) uint8 {
	value &= 0x1f

	return value<<3 | value>>2
}

func orientationClass(width, height int) string {
	switch {
	case width == height:
		return "square"
	case width > height:
		return "landscape"
	default:
		return "portrait"
	}
}
