package processing

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"

	"github.com/openbracketdevelopment/bytedown/internal/config"
)

func readInput(path string, limits config.Limits) ([]byte, Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, Info{}, ErrInvalidImage
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil, Info{}, ErrInvalidImage
	}
	if stat.Size() > limits.UploadBytes {
		return nil, Info{}, ErrLimit
	}
	data, err := io.ReadAll(io.LimitReader(f, limits.UploadBytes+1))
	if err != nil {
		return nil, Info{}, ErrInvalidImage
	}
	if int64(len(data)) > limits.UploadBytes {
		return nil, Info{}, ErrLimit
	}
	var cfg image.Config
	var format string
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")):
		format = "png"
		if err := checkPNG(data); err != nil {
			return nil, Info{}, err
		}
		cfg, err = png.DecodeConfig(bytes.NewReader(data))
	case len(data) >= 4 && data[0] == 0xff && data[1] == 0xd8:
		format = "jpeg"
		if data[len(data)-2] != 0xff || data[len(data)-1] != 0xd9 {
			return nil, Info{}, ErrInvalidImage
		}
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(data))
	default:
		return nil, Info{}, ErrInvalidImage
	}
	if err != nil {
		return nil, Info{}, ErrInvalidImage
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > limits.Dimension || cfg.Height > limits.Dimension ||
		int64(cfg.Width) > limits.Pixels/int64(cfg.Height) {
		return nil, Info{}, ErrLimit
	}
	// Fully decode after dimensions have been bounded: header-only checks miss
	// truncated entropy streams and PNG zlib failures tolerated by some loaders.
	if format == "png" {
		_, err = png.Decode(bytes.NewReader(data))
	} else {
		_, err = jpeg.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return nil, Info{}, ErrInvalidImage
	}
	return data, Info{cfg.Width, cfg.Height, format, int64(len(data))}, nil
}

// Parse real chunks and CRCs, not byte substrings (which can occur in IDAT).
func checkPNG(data []byte) error {
	pos := 8
	hasICC, nonSRGB := false, false
	for pos < len(data) {
		if len(data)-pos < 12 {
			return ErrInvalidImage
		}
		length := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
		if length > int64(len(data)-pos-12) {
			return ErrInvalidImage
		}
		end := pos + 8 + int(length)
		typ := string(data[pos+4 : pos+8])
		if crc32.ChecksumIEEE(data[pos+4:end]) != binary.BigEndian.Uint32(data[end:end+4]) {
			return ErrInvalidImage
		}
		switch typ {
		case "acTL", "fcTL", "fdAT", "cICP", "mDCV", "cLLI":
			return ErrInvalidImage
		case "iCCP":
			hasICC = true
		case "gAMA":
			if length != 4 {
				return ErrInvalidImage
			}
			nonSRGB = nonSRGB || binary.BigEndian.Uint32(data[pos+8:end]) != 45455
		case "cHRM":
			standard := []uint32{31270, 32900, 64000, 33000, 30000, 60000, 15000, 6000}
			if length != 32 {
				return ErrInvalidImage
			}
			for i, value := range standard {
				nonSRGB = nonSRGB || binary.BigEndian.Uint32(data[pos+8+i*4:pos+12+i*4]) != value
			}
		case "IEND":
			if length != 0 || end+4 != len(data) || (nonSRGB && !hasICC) {
				return ErrInvalidImage
			}
			return nil
		}
		pos = end + 4
	}
	return ErrInvalidImage
}
