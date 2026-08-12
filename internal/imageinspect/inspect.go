package imageinspect

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"

	"golang.org/x/image/webp"
)

// Inspect validates and decodes one untrusted image under explicit limits.
func (Inspector) Inspect(
	ctx context.Context,
	reader io.ReadSeeker,
	limits Limits,
) (inspection Inspection, returnErr error) {
	defer func() {
		if recover() != nil {
			inspection = Inspection{}
			returnErr = fmt.Errorf("inspecting image: %w", ErrDecoderPanic)
		}
	}()
	if reader == nil {
		return Inspection{}, errors.New("inspecting image: reader is nil")
	}
	if err := validateLimits(limits); err != nil {
		return Inspection{}, err
	}
	if err := ctx.Err(); err != nil {
		return Inspection{}, fmt.Errorf("inspecting image: %w", err)
	}
	size, err := sourceSize(reader)
	if err != nil {
		return Inspection{}, err
	}
	if size > limits.MaxSourceBytes {
		return Inspection{}, ErrSourceLimit
	}

	header := make([]byte, 12)
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return Inspection{}, fmt.Errorf("seeking image header: %w", err)
	}
	if _, err := io.ReadFull(&contextReader{ctx: ctx, reader: reader}, header); err != nil {
		return Inspection{}, fmt.Errorf("reading image header: %w", ErrUnsupportedFormat)
	}
	format, mimeType, _, err := detectFormat(header)
	if err != nil {
		return Inspection{}, err
	}
	if !extensionMatches(format, limits.ExpectedExtension) {
		return Inspection{}, ErrFormatMismatch
	}
	metadata, err := readEncodedMetadata(&contextReadSeeker{ctx: ctx, reader: reader}, size, format)
	if err != nil {
		return Inspection{}, fmt.Errorf("reading %s metadata: %w", format, err)
	}

	config, err := decodeConfig(ctx, reader, format, metadata)
	if err != nil {
		return Inspection{}, fmt.Errorf("decoding %s configuration: %w", format, err)
	}
	if err := checkedPixels(config.Width, config.Height, limits.MaxImagePixels); err != nil {
		return Inspection{}, err
	}
	decoded, err := decodeImage(ctx, reader, format, metadata)
	if err != nil {
		return Inspection{}, fmt.Errorf("decoding %s image: %w", format, err)
	}
	encodedBounds := decoded.Bounds()
	if encodedBounds.Dx() != config.Width || encodedBounds.Dy() != config.Height {
		return Inspection{}, fmt.Errorf("validating decoded dimensions: %w", ErrInvalidMetadata)
	}
	if err := ctx.Err(); err != nil {
		return Inspection{}, fmt.Errorf("inspecting decoded image: %w", err)
	}

	display := applyOrientation(decoded, metadata.orientation)
	hasTransparency, colors, err := inspectPixels(ctx, display)
	if err != nil {
		return Inspection{}, err
	}
	thumbnail, err := makeThumbnail(display, limits.ThumbnailMaxDimension)
	if err != nil {
		return Inspection{}, err
	}
	analysis, err := makeAnalysisRendition(
		ctx,
		display,
		hasTransparency,
		limits.AnalysisMaxDimension,
		limits.MaxAnalysisBytes,
	)
	if err != nil {
		return Inspection{}, err
	}
	displayBounds := display.Bounds()
	displayWidth := displayBounds.Dx()
	displayHeight := displayBounds.Dy()

	return Inspection{
		Format: format, MIMEType: mimeType, FileSizeBytes: size,
		EncodedWidth: config.Width, EncodedHeight: config.Height,
		DisplayWidth: displayWidth, DisplayHeight: displayHeight,
		AspectRatio: float64(displayWidth) / float64(displayHeight),
		Orientation: metadata.orientation, OrientationClass: orientationClass(displayWidth, displayHeight),
		HasAlpha: metadata.hasAlpha, HasTransparency: hasTransparency,
		EncodedAnimated: metadata.animated, EncodedFrameCount: metadata.frameCount,
		DominantColors: colors, Thumbnail: thumbnail,
		Analysis: Rendition{
			MIMEType: analysis.MIMEType, Extension: analysis.Extension,
			Width: analysis.Width, Height: analysis.Height, Data: analysis.Data,
		},
	}, nil
}

func validateLimits(limits Limits) error {
	if limits.MaxSourceBytes < 1 || limits.MaxImagePixels < 1 ||
		limits.ThumbnailMaxDimension < 1 || limits.ThumbnailMaxDimension > 320 ||
		limits.AnalysisMaxDimension < 1 || limits.MaxAnalysisBytes < 1 {
		return errors.New("inspecting image: limits are invalid")
	}
	if limits.ExpectedExtension != "" && limits.ExpectedExtension[0] != '.' {
		return errors.New("inspecting image: expected extension must begin with a dot")
	}

	return nil
}

func sourceSize(reader io.Seeker) (int64, error) {
	size, err := reader.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("measuring image source: %w", err)
	}
	if size < 1 {
		return 0, fmt.Errorf("measuring image source: %w", ErrUnsupportedFormat)
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("rewinding image source: %w", err)
	}

	return size, nil
}

func decodeConfig(
	ctx context.Context,
	reader io.ReadSeeker,
	format string,
	metadata encodedMetadata,
) (image.Config, error) {
	if format == "webp" && metadata.animated {
		return image.Config{
			ColorModel: color.NRGBAModel,
			Width:      metadata.webP.canvasWidth, Height: metadata.webP.canvasHeight,
		}, nil
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return image.Config{}, err
	}
	contextual := &contextReader{ctx: ctx, reader: reader}
	switch format {
	case "png":
		return png.DecodeConfig(contextual)
	case "jpeg":
		return jpeg.DecodeConfig(contextual)
	case "gif":
		return gif.DecodeConfig(contextual)
	case "webp":
		return webp.DecodeConfig(contextual)
	default:
		return image.Config{}, ErrUnsupportedFormat
	}
}

func decodeImage(
	ctx context.Context,
	reader io.ReadSeeker,
	format string,
	metadata encodedMetadata,
) (image.Image, error) {
	if format == "webp" && metadata.animated {
		return decodeAnimatedWebPFirstFrame(ctx, reader, metadata.webP)
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	contextual := &contextReader{ctx: ctx, reader: reader}
	switch format {
	case "png":
		return png.Decode(contextual)
	case "jpeg":
		return jpeg.Decode(contextual)
	case "gif":
		return gif.Decode(contextual)
	case "webp":
		return webp.Decode(contextual)
	default:
		return nil, ErrUnsupportedFormat
	}
}

func decodeAnimatedWebPFirstFrame(
	ctx context.Context,
	reader io.ReadSeeker,
	metadata webPMetadata,
) (image.Image, error) {
	frame := metadata.firstFrame
	if frame.payloadLength < 1 || frame.payloadLength > math.MaxUint32-22 {
		return nil, ErrInvalidMetadata
	}
	header := make([]byte, 30)
	copy(header[:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(22+frame.payloadLength))
	copy(header[8:12], "WEBP")
	copy(header[12:16], "VP8X")
	binary.LittleEndian.PutUint32(header[16:20], 10)
	if frame.hasAlphaChunk {
		header[20] = 1 << 4
	}
	writeUint24(header[24:27], uint32(frame.width-1))
	writeUint24(header[27:30], uint32(frame.height-1))
	if _, err := reader.Seek(frame.payloadOffset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking animated webp frame: %w", err)
	}
	frameReader := io.MultiReader(
		bytes.NewReader(header),
		io.LimitReader(&contextReader{ctx: ctx, reader: reader}, frame.payloadLength),
	)
	decoded, err := webp.Decode(frameReader)
	if err != nil {
		return nil, err
	}
	if decoded.Bounds().Dx() != frame.width || decoded.Bounds().Dy() != frame.height {
		return nil, ErrInvalidMetadata
	}

	canvas := image.NewNRGBA(image.Rect(0, 0, metadata.canvasWidth, metadata.canvasHeight))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: metadata.background}, image.Point{}, draw.Src)
	destination := image.Rect(frame.x, frame.y, frame.x+frame.width, frame.y+frame.height)
	operator := draw.Src
	if frame.blend {
		operator = draw.Over
	}
	draw.Draw(canvas, destination, decoded, decoded.Bounds().Min, operator)

	return canvas, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

type contextReadSeeker struct {
	ctx    context.Context
	reader io.ReadSeeker
}

func (reader *contextReadSeeker) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}

	return reader.reader.Read(buffer)
}

func (reader *contextReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}

	return reader.reader.Seek(offset, whence)
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}

	return reader.reader.Read(buffer)
}

func writeUint24(destination []byte, value uint32) {
	destination[0] = byte(value)
	destination[1] = byte(value >> 8)
	destination[2] = byte(value >> 16)
}
