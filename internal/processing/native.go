package processing

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/openbracketdevelopment/bytedown/internal/config"
)

var startOnce sync.Once
var startError error

func startup() error {
	startOnce.Do(func() {
		vips.LoggingSettings(func(string, vips.LogLevel, string) {}, vips.LogLevelError)
		startError = vips.Startup(&vips.Config{
			ConcurrencyLevel: 2, MaxCacheFiles: 0, MaxCacheMem: 0, MaxCacheSize: 0,
		})
	})
	if startError != nil {
		return ErrUnavailable
	}
	return nil
}

func normalized(path string, limits config.Limits) (*vips.ImageRef, Info, error) {
	data, info, err := readInput(path, limits)
	if err != nil {
		return nil, Info{}, err
	}
	if err := startup(); err != nil {
		return nil, Info{}, err
	}
	img, err := vips.LoadImageFromBuffer(data, vips.NewImportParams())
	if err != nil {
		return nil, Info{}, ErrInvalidImage
	}
	fail := func() (*vips.ImageRef, Info, error) {
		img.Close()
		return nil, Info{}, ErrInvalidImage
	}
	if img.Pages() > 1 {
		return fail()
	}
	profile, hasICC, profileErr := embeddedICC(data, info.Format)
	bands := img.Bands()
	if img.HasAlpha() {
		bands--
	}
	if profileErr != nil || (hasICC && (!validICC(profile, bands) || !img.HasProfile())) {
		return fail()
	}
	if img.HasProfile() {
		// libvips' built-in sRGB profile avoids filesystem-dependent fallback
		// profiles. An embedded profile must be usable; never guess CMYK.
		if !hasICC {
			return fail()
		}
		if err := img.TransformICCProfileWithFallback("srgb", ""); err != nil {
			return fail()
		}
	} else {
		switch img.Interpretation() {
		case vips.InterpretationSRGB, vips.InterpretationRGB, vips.InterpretationRGB16,
			vips.InterpretationBW, vips.InterpretationGrey16:
		default:
			return fail()
		}
	}
	target := vips.InterpretationSRGB
	if img.BandFormat() == vips.BandFormatUshort {
		target = vips.InterpretationRGB16
	}
	if err := img.ToColorSpace(target); err != nil {
		return fail()
	}
	if err := img.AutoRotate(); err != nil {
		return fail()
	}
	if _, err := img.Average(); err != nil {
		return fail()
	}
	info.Width, info.Height = img.Width(), img.Height()
	return img, info, nil
}

func nativeValidate(path string, limits config.Limits) (Info, error) {
	img, info, err := normalized(path, limits)
	if img != nil {
		img.Close()
	}
	return info, err
}

type plan struct {
	format string
	width  int
}

func outputPlan(info Info, p config.Profile, limit int) ([]plan, error) {
	plans := []plan{{info.Format, info.Width}}
	widths := append([]int(nil), p.Widths...)
	if p.IncludeSource {
		widths = append(widths, info.Width)
	}
	sort.Ints(widths)
	seen := map[plan]bool{plans[0]: true}
	for _, w := range widths {
		if w > info.Width {
			continue
		}
		for _, f := range p.Formats {
			item := plan{f, w}
			if !seen[item] {
				seen[item] = true
				plans = append(plans, item)
			}
		}
	}
	if len(plans) > limit {
		return nil, ErrLimit
	}
	return plans, nil
}

func nativeRun(input, dir string, profile config.Profile, limits config.Limits) (outputs []Output, err error) {
	source, info, err := normalized(input, limits)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	plans, err := outputPlan(info, profile, limits.OutputCount)
	if err != nil {
		return nil, err
	}
	// Explicit JPEG variants cannot preserve transparency.
	if source.HasAlpha() {
		for _, item := range plans {
			if item.format == "jpeg" {
				return nil, ErrInvalidImage
			}
		}
	}
	var created []string
	defer func() {
		if err != nil {
			for _, name := range created {
				_ = os.Remove(name)
			}
			outputs = nil
		}
	}()
	var total int64
	for _, item := range plans {
		start := time.Now()
		img, copyErr := source.Copy()
		if copyErr != nil {
			return nil, ErrUnavailable
		}
		if item.width < info.Width {
			if resizeErr := img.Resize(float64(item.width)/float64(info.Width), vips.KernelLanczos3); resizeErr != nil {
				img.Close()
				return nil, ErrInvalidImage
			}
		}
		data, encodeErr := encode(img, item.format, profile)
		width, height := img.Width(), img.Height()
		img.Close()
		if encodeErr != nil {
			return nil, ErrUnavailable
		}
		if int64(len(data)) > limits.OutputBytes-total {
			return nil, ErrLimit
		}
		total += int64(len(data))
		id, idErr := randomID()
		if idErr != nil {
			return nil, ErrUnavailable
		}
		ext := "." + item.format
		if item.format == "jpeg" {
			ext = ".jpg"
		}
		// Attempt directories are private and unique; stable basenames let the
		// publisher derive the same asset identity after a lease retry.
		name := fmt.Sprintf("%s-%d%s", item.format, item.width, ext)
		path := filepath.Join(dir, name)
		partial := filepath.Join(dir, "."+id+".part")
		f, openErr := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if openErr != nil {
			return nil, ErrUnavailable
		}
		created = append(created, partial)
		_, writeErr := f.Write(data)
		syncErr := f.Sync()
		closeErr := f.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return nil, ErrUnavailable
		}
		// The attempt directory belongs exclusively to this child. Never replace
		// an existing output, and expose its final name only after fsync/close.
		if _, statErr := os.Lstat(path); statErr == nil || !errors.Is(statErr, os.ErrNotExist) {
			return nil, ErrUnavailable
		}
		if err := os.Rename(partial, path); err != nil {
			return nil, ErrUnavailable
		}
		created = append(created, path)
		outputs = append(outputs, Output{id, item.format, name, width, height, int64(len(data)), time.Since(start).Milliseconds()})
	}
	return outputs, nil
}

func encode(img *vips.ImageRef, format string, profile config.Profile) ([]byte, error) {
	var data []byte
	var err error
	switch format {
	case "jpeg":
		p := vips.NewJpegExportParams()
		p.Quality, p.StripMetadata, p.OptimizeCoding = profile.JPEGQuality, true, true
		data, _, err = img.ExportJpeg(p)
	case "png":
		p := vips.NewPngExportParams()
		p.StripMetadata, p.Compression, p.Palette = true, 6, false
		if img.BandFormat() == vips.BandFormatUshort {
			p.Bitdepth = 16
		} else {
			p.Bitdepth = 8
		}
		data, _, err = img.ExportPng(p)
	case "webp":
		p := vips.NewWebpExportParams()
		p.Quality, p.ReductionEffort, p.StripMetadata = profile.WebPQuality, profile.WebPEffort, true
		data, _, err = img.ExportWebp(p)
	case "avif":
		p := vips.NewAvifExportParams()
		p.Quality, p.Effort, p.StripMetadata = profile.AVIFQuality, profile.AVIFEffort, true
		p.Bitdepth = 8
		data, _, err = img.ExportAvif(p)
	default:
		return nil, ErrUnavailable
	}
	return data, err
}

func nativeProbe() error {
	if err := startup(); err != nil {
		return err
	}
	fixture := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			fixture.SetNRGBA(x, y, color.NRGBA{uint8(x * 16), uint8(y * 16), 100, 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, fixture); err != nil {
		return ErrUnavailable
	}
	img, err := vips.LoadImageFromBuffer(buffer.Bytes(), vips.NewImportParams())
	if err != nil {
		return ErrUnavailable
	}
	defer img.Close()
	for _, format := range Formats() {
		encoded, err := encode(img, format.ID, config.DefaultProfile())
		if err != nil {
			return ErrUnavailable
		}
		decoded, err := vips.LoadImageFromBuffer(encoded, vips.NewImportParams())
		if err != nil {
			return ErrUnavailable
		}
		_, evalErr := decoded.Average()
		ok := decoded.Width() == 16 && decoded.Height() == 16
		decoded.Close()
		if evalErr != nil || !ok {
			return ErrUnavailable
		}
	}
	return nil
}
