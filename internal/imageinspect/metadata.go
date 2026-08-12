package imageinspect

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/color"
	"io"
	"math"
	"strings"
)

const maxEXIFBytes = 1 << 20

type encodedMetadata struct {
	orientation int
	hasAlpha    bool
	animated    bool
	frameCount  int
	webP        webPMetadata
}

type webPMetadata struct {
	canvasWidth  int
	canvasHeight int
	hasAlpha     bool
	animated     bool
	frameCount   int
	background   color.NRGBA
	firstFrame   webPFrame
	exifOffset   int64
	exifLength   int64
}

type webPFrame struct {
	x             int
	y             int
	width         int
	height        int
	blend         bool
	payloadOffset int64
	payloadLength int64
	hasAlphaChunk bool
}

func detectFormat(header []byte) (format, mimeType, extension string, err error) {
	switch {
	case len(header) >= 8 && bytes.Equal(header[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "png", "image/png", ".png", nil
	case len(header) >= 3 && header[0] == 0xff && header[1] == 0xd8 && header[2] == 0xff:
		return "jpeg", "image/jpeg", ".jpg", nil
	case len(header) >= 6 && (bytes.Equal(header[:6], []byte("GIF87a")) || bytes.Equal(header[:6], []byte("GIF89a"))):
		return "gif", "image/gif", ".gif", nil
	case len(header) >= 12 && bytes.Equal(header[:4], []byte("RIFF")) && bytes.Equal(header[8:12], []byte("WEBP")):
		return "webp", "image/webp", ".webp", nil
	default:
		return "", "", "", ErrUnsupportedFormat
	}
}

func extensionMatches(format, extension string) bool {
	if extension == "" {
		return true
	}
	switch format {
	case "png":
		return strings.EqualFold(extension, ".png")
	case "jpeg":
		return strings.EqualFold(extension, ".jpg") || strings.EqualFold(extension, ".jpeg")
	case "gif":
		return strings.EqualFold(extension, ".gif")
	case "webp":
		return strings.EqualFold(extension, ".webp")
	default:
		return false
	}
}

func readEncodedMetadata(reader io.ReadSeeker, size int64, format string) (encodedMetadata, error) {
	metadata := encodedMetadata{orientation: 1, frameCount: 1}
	switch format {
	case "jpeg":
		orientation, err := readJPEGOrientation(reader, size)
		if err != nil {
			return encodedMetadata{}, err
		}
		metadata.orientation = orientation
	case "png":
		hasAlpha, orientation, err := readPNGMetadata(reader, size)
		if err != nil {
			return encodedMetadata{}, err
		}
		metadata.hasAlpha = hasAlpha
		metadata.orientation = orientation
	case "gif":
		frames, hasAlpha, err := readGIFMetadata(reader, size)
		if err != nil {
			return encodedMetadata{}, err
		}
		metadata.frameCount = frames
		metadata.animated = frames > 1
		metadata.hasAlpha = hasAlpha
	case "webp":
		webP, err := readWebPMetadata(reader, size)
		if err != nil {
			return encodedMetadata{}, err
		}
		metadata.webP = webP
		metadata.hasAlpha = webP.hasAlpha
		metadata.animated = webP.animated
		metadata.frameCount = max(1, webP.frameCount)
		if webP.exifLength > 0 && webP.exifLength <= maxEXIFBytes {
			exif := make([]byte, webP.exifLength)
			if _, err := reader.Seek(webP.exifOffset, io.SeekStart); err != nil {
				return encodedMetadata{}, fmt.Errorf("seeking webp exif: %w", err)
			}
			if _, err := io.ReadFull(reader, exif); err != nil {
				return encodedMetadata{}, fmt.Errorf("reading webp exif: %w", err)
			}
			if orientation, ok := parseTIFFOrientation(exif); ok {
				metadata.orientation = orientation
			}
		}
	default:
		return encodedMetadata{}, ErrUnsupportedFormat
	}

	return metadata, nil
}

func readJPEGOrientation(reader io.ReadSeeker, size int64) (int, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return 1, fmt.Errorf("seeking jpeg metadata: %w", err)
	}
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil || header[0] != 0xff || header[1] != 0xd8 {
		return 1, fmt.Errorf("reading jpeg metadata: %w", ErrInvalidMetadata)
	}
	for {
		position, err := reader.Seek(0, io.SeekCurrent)
		if err != nil {
			return 1, fmt.Errorf("locating jpeg segment: %w", err)
		}
		if position >= size {
			return 1, nil
		}
		markerPrefix, err := readOneByte(reader)
		if err != nil {
			return 1, fmt.Errorf("reading jpeg marker: %w", err)
		}
		if markerPrefix != 0xff {
			return 1, fmt.Errorf("reading jpeg marker: %w", ErrInvalidMetadata)
		}
		marker, err := readOneByte(reader)
		if err != nil {
			return 1, fmt.Errorf("reading jpeg marker: %w", err)
		}
		for marker == 0xff {
			marker, err = readOneByte(reader)
			if err != nil {
				return 1, fmt.Errorf("reading jpeg marker: %w", err)
			}
		}
		if marker == 0xda || marker == 0xd9 {
			return 1, nil
		}
		if marker == 0x01 || marker >= 0xd0 && marker <= 0xd8 {
			continue
		}
		lengthBytes := make([]byte, 2)
		if _, err := io.ReadFull(reader, lengthBytes); err != nil {
			return 1, fmt.Errorf("reading jpeg segment length: %w", err)
		}
		segmentLength := int64(binary.BigEndian.Uint16(lengthBytes))
		if segmentLength < 2 {
			return 1, fmt.Errorf("reading jpeg segment length: %w", ErrInvalidMetadata)
		}
		payloadLength := segmentLength - 2
		payloadStart, err := reader.Seek(0, io.SeekCurrent)
		if err != nil || payloadLength > size-payloadStart {
			return 1, fmt.Errorf("reading jpeg segment: %w", ErrInvalidMetadata)
		}
		if marker == 0xe1 && payloadLength <= maxEXIFBytes {
			payload := make([]byte, payloadLength)
			if _, err := io.ReadFull(reader, payload); err != nil {
				return 1, fmt.Errorf("reading jpeg exif: %w", err)
			}
			if orientation, ok := parseTIFFOrientation(payload); ok {
				return orientation, nil
			}
			continue
		}
		if _, err := reader.Seek(payloadLength, io.SeekCurrent); err != nil {
			return 1, fmt.Errorf("skipping jpeg segment: %w", err)
		}
	}
}

func readPNGMetadata(reader io.ReadSeeker, size int64) (bool, int, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return false, 1, fmt.Errorf("seeking png metadata: %w", err)
	}
	header := make([]byte, 33)
	if _, err := io.ReadFull(reader, header); err != nil {
		return false, 1, fmt.Errorf("reading png header: %w", err)
	}
	if !bytes.Equal(header[:8], []byte("\x89PNG\r\n\x1a\n")) ||
		binary.BigEndian.Uint32(header[8:12]) != 13 || !bytes.Equal(header[12:16], []byte("IHDR")) {
		return false, 1, fmt.Errorf("reading png header: %w", ErrInvalidMetadata)
	}
	colorType := header[25]
	hasAlpha := colorType == 4 || colorType == 6
	orientation := 1
	for {
		position, err := reader.Seek(0, io.SeekCurrent)
		if err != nil {
			return false, 1, fmt.Errorf("locating png chunk: %w", err)
		}
		if position == size {
			return hasAlpha, orientation, nil
		}
		chunkHeader := make([]byte, 8)
		if _, err := io.ReadFull(reader, chunkHeader); err != nil {
			return false, 1, fmt.Errorf("reading png chunk: %w", err)
		}
		chunkLength := int64(binary.BigEndian.Uint32(chunkHeader[:4]))
		chunkType := string(chunkHeader[4:8])
		dataStart, err := reader.Seek(0, io.SeekCurrent)
		if err != nil || chunkLength > size-dataStart-4 {
			return false, 1, fmt.Errorf("reading png chunk: %w", ErrInvalidMetadata)
		}
		if chunkType == "eXIf" && chunkLength <= maxEXIFBytes {
			payload := make([]byte, chunkLength)
			if _, err := io.ReadFull(reader, payload); err != nil {
				return false, 1, fmt.Errorf("reading png exif: %w", err)
			}
			if parsed, ok := parseTIFFOrientation(payload); ok {
				orientation = parsed
			}
		} else if _, err := reader.Seek(chunkLength, io.SeekCurrent); err != nil {
			return false, 1, fmt.Errorf("skipping png chunk: %w", err)
		}
		if _, err := reader.Seek(4, io.SeekCurrent); err != nil {
			return false, 1, fmt.Errorf("skipping png checksum: %w", err)
		}
		if chunkType == "IDAT" || chunkType == "IEND" {
			return hasAlpha, orientation, nil
		}
	}
}

func readGIFMetadata(reader io.ReadSeeker, size int64) (int, bool, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return 0, false, fmt.Errorf("seeking gif metadata: %w", err)
	}
	header := make([]byte, 13)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, false, fmt.Errorf("reading gif header: %w", err)
	}
	if !bytes.Equal(header[:6], []byte("GIF87a")) && !bytes.Equal(header[:6], []byte("GIF89a")) {
		return 0, false, fmt.Errorf("reading gif header: %w", ErrInvalidMetadata)
	}
	if header[10]&0x80 != 0 {
		tableSize := int64(3 * (1 << ((header[10] & 0x07) + 1)))
		if err := skipBounded(reader, tableSize, size); err != nil {
			return 0, false, fmt.Errorf("skipping gif global palette: %w", err)
		}
	}
	frames := 0
	hasAlpha := false
	for {
		blockType, err := readOneByte(reader)
		if err != nil {
			return 0, false, fmt.Errorf("reading gif block: %w", err)
		}
		switch blockType {
		case 0x3b:
			if frames == 0 {
				return 0, false, fmt.Errorf("reading gif trailer: %w", ErrInvalidMetadata)
			}
			return frames, hasAlpha, nil
		case 0x2c:
			descriptor := make([]byte, 9)
			if _, err := io.ReadFull(reader, descriptor); err != nil {
				return 0, false, fmt.Errorf("reading gif image descriptor: %w", err)
			}
			if descriptor[8]&0x80 != 0 {
				tableSize := int64(3 * (1 << ((descriptor[8] & 0x07) + 1)))
				if err := skipBounded(reader, tableSize, size); err != nil {
					return 0, false, fmt.Errorf("skipping gif local palette: %w", err)
				}
			}
			if _, err := readOneByte(reader); err != nil {
				return 0, false, fmt.Errorf("reading gif lzw size: %w", err)
			}
			if err := skipGIFSubBlocks(reader, size); err != nil {
				return 0, false, err
			}
			frames++
		case 0x21:
			label, err := readOneByte(reader)
			if err != nil {
				return 0, false, fmt.Errorf("reading gif extension: %w", err)
			}
			if label == 0xf9 {
				graphicControl := make([]byte, 6)
				if _, err := io.ReadFull(reader, graphicControl); err != nil {
					return 0, false, fmt.Errorf("reading gif graphic control: %w", err)
				}
				if graphicControl[0] != 4 || graphicControl[5] != 0 {
					return 0, false, fmt.Errorf("reading gif graphic control: %w", ErrInvalidMetadata)
				}
				hasAlpha = hasAlpha || graphicControl[1]&0x01 != 0
			} else if err := skipGIFSubBlocks(reader, size); err != nil {
				return 0, false, err
			}
		default:
			return 0, false, fmt.Errorf("reading gif block: %w", ErrInvalidMetadata)
		}
	}
}

func readWebPMetadata(reader io.ReadSeeker, size int64) (webPMetadata, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return webPMetadata{}, fmt.Errorf("seeking webp metadata: %w", err)
	}
	header := make([]byte, 12)
	if _, err := io.ReadFull(reader, header); err != nil {
		return webPMetadata{}, fmt.Errorf("reading webp header: %w", err)
	}
	if !bytes.Equal(header[:4], []byte("RIFF")) || !bytes.Equal(header[8:12], []byte("WEBP")) {
		return webPMetadata{}, fmt.Errorf("reading webp header: %w", ErrInvalidMetadata)
	}
	declaredSize := int64(binary.LittleEndian.Uint32(header[4:8])) + 8
	if declaredSize != size {
		return webPMetadata{}, fmt.Errorf("reading webp size: %w", ErrInvalidMetadata)
	}

	metadata := webPMetadata{background: color.NRGBA{}}
	seenVP8X := false
	seenANIM := false
	for offset := int64(12); offset < size; {
		if size-offset < 8 {
			return webPMetadata{}, fmt.Errorf("reading webp chunk header: %w", ErrInvalidMetadata)
		}
		if _, err := reader.Seek(offset, io.SeekStart); err != nil {
			return webPMetadata{}, fmt.Errorf("seeking webp chunk: %w", err)
		}
		chunkHeader := make([]byte, 8)
		if _, err := io.ReadFull(reader, chunkHeader); err != nil {
			return webPMetadata{}, fmt.Errorf("reading webp chunk header: %w", err)
		}
		chunkType := string(chunkHeader[:4])
		chunkLength := int64(binary.LittleEndian.Uint32(chunkHeader[4:8]))
		dataOffset := offset + 8
		paddedLength := chunkLength + chunkLength%2
		if chunkLength > size-dataOffset || paddedLength > size-dataOffset {
			return webPMetadata{}, fmt.Errorf("reading webp chunk %q: %w", chunkType, ErrInvalidMetadata)
		}
		switch chunkType {
		case "VP8X":
			if seenVP8X || chunkLength != 10 {
				return webPMetadata{}, fmt.Errorf("reading webp extended header: %w", ErrInvalidMetadata)
			}
			seenVP8X = true
			data := make([]byte, 10)
			if _, err := io.ReadFull(reader, data); err != nil {
				return webPMetadata{}, fmt.Errorf("reading webp extended header: %w", err)
			}
			metadata.hasAlpha = data[0]&(1<<4) != 0
			metadata.animated = data[0]&(1<<1) != 0
			metadata.canvasWidth = int(readUint24(data[4:7])) + 1
			metadata.canvasHeight = int(readUint24(data[7:10])) + 1
		case "ANIM":
			if chunkLength != 6 || seenANIM {
				return webPMetadata{}, fmt.Errorf("reading webp animation header: %w", ErrInvalidMetadata)
			}
			seenANIM = true
			data := make([]byte, 6)
			if _, err := io.ReadFull(reader, data); err != nil {
				return webPMetadata{}, fmt.Errorf("reading webp animation header: %w", err)
			}
			metadata.background = color.NRGBA{R: data[2], G: data[1], B: data[0], A: data[3]}
		case "ANMF":
			frame, err := readWebPFrame(reader, dataOffset, chunkLength, metadata)
			if err != nil {
				return webPMetadata{}, err
			}
			metadata.frameCount++
			if metadata.frameCount == 1 {
				metadata.firstFrame = frame
			}
			metadata.hasAlpha = metadata.hasAlpha || frame.hasAlphaChunk
		case "EXIF":
			if metadata.exifLength == 0 {
				metadata.exifOffset = dataOffset
				metadata.exifLength = chunkLength
			}
		case "ALPH":
			metadata.hasAlpha = true
		case "VP8L":
			hasAlpha, err := readVP8LAlphaFlag(reader, chunkLength)
			if err != nil {
				return webPMetadata{}, fmt.Errorf("reading webp lossless header: %w", err)
			}
			metadata.hasAlpha = metadata.hasAlpha || hasAlpha
		}
		offset = dataOffset + paddedLength
	}
	if metadata.animated {
		if !seenVP8X || !seenANIM || metadata.frameCount < 1 ||
			metadata.canvasWidth < 1 || metadata.canvasHeight < 1 {
			return webPMetadata{}, fmt.Errorf("reading webp animation: %w", ErrInvalidMetadata)
		}
	} else if metadata.frameCount != 0 {
		return webPMetadata{}, fmt.Errorf("reading webp animation flag: %w", ErrInvalidMetadata)
	}

	return metadata, nil
}

func readWebPFrame(
	reader io.ReadSeeker,
	dataOffset int64,
	chunkLength int64,
	metadata webPMetadata,
) (webPFrame, error) {
	if chunkLength < 16 {
		return webPFrame{}, fmt.Errorf("reading webp frame header: %w", ErrInvalidMetadata)
	}
	if _, err := reader.Seek(dataOffset, io.SeekStart); err != nil {
		return webPFrame{}, fmt.Errorf("seeking webp frame header: %w", err)
	}
	header := make([]byte, 16)
	if _, err := io.ReadFull(reader, header); err != nil {
		return webPFrame{}, fmt.Errorf("reading webp frame header: %w", err)
	}
	frame := webPFrame{
		x: int(readUint24(header[0:3])) * 2, y: int(readUint24(header[3:6])) * 2,
		width: int(readUint24(header[6:9])) + 1, height: int(readUint24(header[9:12])) + 1,
		blend: header[15]&(1<<1) == 0, payloadOffset: dataOffset + 16,
		payloadLength: chunkLength - 16,
	}
	if frame.width < 1 || frame.height < 1 || frame.x > metadata.canvasWidth-frame.width ||
		frame.y > metadata.canvasHeight-frame.height {
		return webPFrame{}, fmt.Errorf("reading webp frame bounds: %w", ErrInvalidMetadata)
	}
	hasImageChunk := false
	for offset := frame.payloadOffset; offset < frame.payloadOffset+frame.payloadLength; {
		if frame.payloadOffset+frame.payloadLength-offset < 8 {
			return webPFrame{}, fmt.Errorf("reading webp frame chunk: %w", ErrInvalidMetadata)
		}
		if _, err := reader.Seek(offset, io.SeekStart); err != nil {
			return webPFrame{}, fmt.Errorf("seeking webp frame chunk: %w", err)
		}
		chunkHeader := make([]byte, 8)
		if _, err := io.ReadFull(reader, chunkHeader); err != nil {
			return webPFrame{}, fmt.Errorf("reading webp frame chunk: %w", err)
		}
		chunkType := string(chunkHeader[:4])
		chunkLength := int64(binary.LittleEndian.Uint32(chunkHeader[4:8]))
		paddedLength := chunkLength + chunkLength%2
		dataStart := offset + 8
		if paddedLength > frame.payloadOffset+frame.payloadLength-dataStart {
			return webPFrame{}, fmt.Errorf("reading webp frame chunk %q: %w", chunkType, ErrInvalidMetadata)
		}
		switch chunkType {
		case "ALPH":
			if hasImageChunk || frame.hasAlphaChunk {
				return webPFrame{}, fmt.Errorf("reading webp alpha chunk: %w", ErrInvalidMetadata)
			}
			frame.hasAlphaChunk = true
		case "VP8 ":
			if hasImageChunk {
				return webPFrame{}, fmt.Errorf("reading webp frame image: %w", ErrInvalidMetadata)
			}
			hasImageChunk = true
		case "VP8L":
			if hasImageChunk || frame.hasAlphaChunk {
				return webPFrame{}, fmt.Errorf("reading webp lossless frame: %w", ErrInvalidMetadata)
			}
			hasAlpha, err := readVP8LAlphaFlag(reader, chunkLength)
			if err != nil {
				return webPFrame{}, fmt.Errorf("reading webp lossless frame: %w", err)
			}
			frame.hasAlphaChunk = hasAlpha
			hasImageChunk = true
		default:
			return webPFrame{}, fmt.Errorf("reading webp frame chunk %q: %w", chunkType, ErrInvalidMetadata)
		}
		offset = dataStart + paddedLength
	}
	if !hasImageChunk {
		return webPFrame{}, fmt.Errorf("reading webp frame image: %w", ErrInvalidMetadata)
	}

	return frame, nil
}

func readVP8LAlphaFlag(reader io.Reader, chunkLength int64) (bool, error) {
	if chunkLength < 5 {
		return false, ErrInvalidMetadata
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return false, err
	}
	if header[0] != 0x2f {
		return false, ErrInvalidMetadata
	}
	bits := binary.LittleEndian.Uint32(header[1:5])
	if bits>>29 != 0 {
		return false, ErrInvalidMetadata
	}

	return bits&(1<<28) != 0, nil
}

func parseTIFFOrientation(payload []byte) (int, bool) {
	if bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
		payload = payload[6:]
	}
	if len(payload) < 8 {
		return 1, false
	}
	var order binary.ByteOrder
	switch string(payload[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1, false
	}
	if order.Uint16(payload[2:4]) != 42 {
		return 1, false
	}
	ifdOffset := uint64(order.Uint32(payload[4:8]))
	if ifdOffset > uint64(len(payload)-2) {
		return 1, false
	}
	entryCount := uint64(order.Uint16(payload[ifdOffset : ifdOffset+2]))
	if entryCount > 4096 || entryCount > (uint64(len(payload))-ifdOffset-2)/12 {
		return 1, false
	}
	for index := uint64(0); index < entryCount; index++ {
		offset := ifdOffset + 2 + index*12
		entry := payload[offset : offset+12]
		if order.Uint16(entry[:2]) != 0x0112 {
			continue
		}
		if order.Uint16(entry[2:4]) != 3 || order.Uint32(entry[4:8]) != 1 {
			return 1, false
		}
		orientation := int(order.Uint16(entry[8:10]))
		if orientation < 1 || orientation > 8 {
			return 1, false
		}

		return orientation, true
	}

	return 1, false
}

func skipGIFSubBlocks(reader io.ReadSeeker, size int64) error {
	for {
		length, err := readOneByte(reader)
		if err != nil {
			return fmt.Errorf("reading gif sub-block: %w", err)
		}
		if length == 0 {
			return nil
		}
		if err := skipBounded(reader, int64(length), size); err != nil {
			return fmt.Errorf("skipping gif sub-block: %w", err)
		}
	}
}

func skipBounded(reader io.ReadSeeker, count, size int64) error {
	position, err := reader.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if count < 0 || count > size-position {
		return ErrInvalidMetadata
	}
	_, err = reader.Seek(count, io.SeekCurrent)

	return err
}

func readOneByte(reader io.Reader) (byte, error) {
	var buffer [1]byte
	_, err := io.ReadFull(reader, buffer[:])

	return buffer[0], err
}

func readUint24(data []byte) uint32 {
	return uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16
}

func checkedPixels(width, height int, maximum int64) error {
	if width < 1 || height < 1 || maximum < 1 {
		return ErrInvalidMetadata
	}
	if int64(width) > math.MaxInt64/int64(height) || int64(width)*int64(height) > maximum {
		return ErrPixelLimit
	}

	return nil
}
