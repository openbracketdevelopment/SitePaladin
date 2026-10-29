package processing

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/openbracketdevelopment/bytedown/internal/config"
)

func TestMain(m *testing.M) {
	if handled, err := HandleCommand(os.Args[1:]); handled {
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	// All fixtures, Go test temporary directories, and govips profile files stay
	// inside the project rather than using the host's shared temporary folder.
	scratch, err := os.MkdirTemp(".", ".processing-test-")
	if err != nil {
		panic(err)
	}
	abs, err := filepath.Abs(scratch)
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("TMPDIR", abs); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(scratch)
	os.Exit(code)
}

func fixture(t *testing.T, format string, alpha bool) (string, image.Image) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 32, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 32; x++ {
			a := uint8(255)
			if alpha && x < 16 {
				a = 80
			}
			img.SetNRGBA(x, y, color.NRGBA{uint8(x * 7), uint8(y * 13), 90, a})
		}
	}
	var data bytes.Buffer
	if format == "jpeg" {
		if err := jpeg.Encode(&data, img, &jpeg.Options{Quality: 95}); err != nil {
			t.Fatal(err)
		}
	} else if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return writeFixture(t, data.Bytes()), img
}

func writeFixture(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func smallProfile() config.Profile {
	p := config.DefaultProfile()
	p.Widths = []int{16, 32, 64}
	return p
}

func TestProbeAndValidation(t *testing.T) {
	if err := startup(); err != nil {
		t.Fatal(err)
	}
	path, _ := fixture(t, "png", false)
	data, _ := os.ReadFile(path)
	img, err := vips.LoadImageFromBuffer(data, vips.NewImportParams())
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	for _, format := range Formats() {
		if _, err := encode(img, format.ID, config.DefaultProfile()); err != nil {
			t.Fatalf("encode %s: %v", format.ID, err)
		}
	}
	if err := Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, format := range Formats() {
		if !format.Available || format.Encoder == "" || format.Options == nil {
			t.Fatalf("successful probe did not declare encoder capabilities: %+v", format)
		}
	}
	for _, format := range []string{"jpeg", "png"} {
		path, _ := fixture(t, format, true)
		got, err := Validate(context.Background(), path, config.DefaultLimits())
		if err != nil || got.Width != 32 || got.Height != 16 || got.Format != format || got.Bytes <= 0 {
			t.Fatalf("validate %s: %+v, %v", format, got, err)
		}
	}
}

func TestOutputs(t *testing.T) {
	for _, format := range []string{"jpeg", "png"} {
		t.Run(format, func(t *testing.T) {
			path, original := fixture(t, format, format == "png")
			dir := t.TempDir()
			outputs, err := Run(context.Background(), path, dir, smallProfile(), config.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if len(outputs) != 5 {
				t.Fatalf("got %d outputs, expected full original-format plus 2 widths * 2 formats", len(outputs))
			}
			seen := map[string]bool{}
			for _, output := range outputs {
				if len(output.ID) != 32 || filepath.Base(output.Filename) != output.Filename || output.Width > 32 {
					t.Fatalf("unsafe output: %+v", output)
				}
				key := output.Format + "-" + string(rune(output.Width))
				if seen[key] {
					t.Fatalf("duplicate output: %+v", output)
				}
				seen[key] = true
				data, err := os.ReadFile(filepath.Join(dir, output.Filename))
				if err != nil || int64(len(data)) != output.Bytes {
					t.Fatalf("output size: %v", err)
				}
				if err := startup(); err != nil {
					t.Fatal(err)
				}
				img, err := vips.LoadImageFromBuffer(data, vips.NewImportParams())
				if err != nil {
					t.Fatal(err)
				}
				if img.Width() != output.Width || img.Height() != output.Height || img.Width() != 2*img.Height() {
					t.Fatalf("unexpected dimensions: %+v", output)
				}
				if img.HasProfile() || img.Orientation() > 1 {
					t.Fatal("retained unnecessary metadata")
				}
				if format == "png" && !img.HasAlpha() {
					t.Fatalf("%s lost alpha", output.Format)
				}
				img.Close()
				if format == "png" && output.Format == "png" {
					decoded, err := png.Decode(bytes.NewReader(data))
					if err != nil {
						t.Fatal(err)
					}
					for y := 0; y < 16; y++ {
						for x := 0; x < 32; x++ {
							if original.At(x, y) != decoded.At(x, y) {
								t.Fatalf("PNG was not lossless at %d,%d: %v != %v", x, y, original.At(x, y), decoded.At(x, y))
							}
						}
					}
				}
			}
		})
	}
}

func TestOrientation(t *testing.T) {
	path, _ := fixture(t, "jpeg", false)
	data, _ := os.ReadFile(path)
	// Little-endian TIFF IFD with orientation 6 (rotate 90 degrees).
	exif := []byte{'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0,
		0x12, 0x01, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0}
	marker := []byte{0xff, 0xe1, 0, byte(len(exif) + 2)}
	marker = append(marker, exif...)
	data = append(append(append([]byte{}, data[:2]...), marker...), data[2:]...)
	path = writeFixture(t, data)
	info, err := Validate(context.Background(), path, config.DefaultLimits())
	if err != nil || info.Width != 16 || info.Height != 32 {
		t.Fatalf("orientation: %+v %v", info, err)
	}
	p := smallProfile()
	p.Widths = []int{8, 16}
	p.Formats = []string{"png"}
	dir := t.TempDir()
	outputs, err := Run(context.Background(), path, dir, p, config.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range outputs {
		if o.Height != o.Width*2 || o.Width > 16 {
			t.Fatalf("incorrect oriented output: %+v", o)
		}
		if o.Format == "png" && o.Width == 16 {
			original, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := os.ReadFile(filepath.Join(dir, o.Filename))
			if err != nil {
				t.Fatal(err)
			}
			got, err := png.Decode(bytes.NewReader(encoded))
			if err != nil {
				t.Fatal(err)
			}
			maxDifference := 0.0
			for y := 2; y < 30; y += 4 {
				for x := 2; x < 14; x += 4 {
					want := color.NRGBAModel.Convert(original.At(y, 15-x)).(color.NRGBA)
					actual := color.NRGBAModel.Convert(got.At(x, y)).(color.NRGBA)
					for i, value := range []uint8{actual.R, actual.G, actual.B} {
						expected := []uint8{want.R, want.G, want.B}[i]
						difference := math.Abs(float64(value) - float64(expected))
						maxDifference = math.Max(maxDifference, difference)
						if difference > 12 {
							t.Fatalf("clockwise orientation pixel mismatch: got %v, want %v", actual, want)
						}
					}
				}
			}
			t.Logf("orientation 6 verified against independently rotated decoded pixels; maximum JPEG decoder difference %.0f", maxDifference)
		}
	}
}

func TestRetryFilenames(t *testing.T) {
	path, _ := fixture(t, "png", true)
	first, err := Run(context.Background(), path, t.TempDir(), smallProfile(), config.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), path, t.TempDir(), smallProfile(), config.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatal("retry changed output count")
	}
	for i := range first {
		if first[i].Filename != second[i].Filename {
			t.Fatalf("retry changed output basename: %q != %q", first[i].Filename, second[i].Filename)
		}
	}
}

func TestAtomicOutputVisibility(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	if _, err := rand.New(rand.NewSource(42)).Read(img.Pix); err != nil {
		t.Fatal(err)
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var input bytes.Buffer
	if err := png.Encode(&input, img); err != nil {
		t.Fatal(err)
	}
	path, dir := writeFixture(t, input.Bytes()), t.TempDir()
	p := config.DefaultProfile()
	p.Widths, p.Formats = []int{128, 256}, []string{"png"}
	type result struct {
		outputs []Output
		err     error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		outputs, err := Run(ctx, path, dir, p, config.DefaultLimits())
		done <- result{outputs, err}
	}()
	seen := map[string]bool{}
	check := func() {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || seen[entry.Name()] {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := png.Decode(bytes.NewReader(data)); err != nil {
				t.Fatalf("final filename became visible before complete PNG: %s: %v", entry.Name(), err)
			}
			seen[entry.Name()] = true
		}
	}
	ticker := time.NewTicker(100 * time.Microsecond)
	defer ticker.Stop()
	for {
		select {
		case result := <-done:
			if result.err != nil {
				t.Fatal(result.err)
			}
			check()
			if len(seen) != len(result.outputs) || len(seen) != 3 {
				t.Fatalf("observed %d complete outputs; expected %d", len(seen), len(result.outputs))
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 3 {
				t.Fatalf("temporary files were not cleaned up: %v", entries)
			}
			return
		case <-ticker.C:
			check()
		}
	}
}

func pngChunk(typ string, body []byte) []byte {
	out := make([]byte, len(body)+12)
	binary.BigEndian.PutUint32(out[:4], uint32(len(body)))
	copy(out[4:8], typ)
	copy(out[8:], body)
	binary.BigEndian.PutUint32(out[len(out)-4:], crc32.ChecksumIEEE(out[4:len(out)-4]))
	return out
}

func insertChunk(data, chunk []byte) []byte {
	return append(append(append([]byte{}, data[:33]...), chunk...), data[33:]...)
}

func TestInvalidInputs(t *testing.T) {
	pngPath, _ := fixture(t, "png", false)
	pngData, _ := os.ReadFile(pngPath)
	jpegPath, _ := fixture(t, "jpeg", false)
	jpegData, _ := os.ReadFile(jpegPath)
	corrupt := append([]byte(nil), pngData...)
	corrupt[45] ^= 0x80
	cases := map[string][]byte{
		"unsupported": []byte("GIF89a"), "svg": []byte("<svg/>"), "empty": {},
		"png truncated": pngData[:len(pngData)-8], "jpeg truncated": jpegData[:len(jpegData)-4],
		"png CRC": corrupt, "APNG": insertChunk(pngData, pngChunk("acTL", make([]byte, 8))),
		"APNG frame":     insertChunk(pngData, pngChunk("fcTL", make([]byte, 26))),
		"non-sRGB gamma": insertChunk(pngData, pngChunk("gAMA", []byte{0, 1, 0x86, 0xa0})),
		"HDR":            insertChunk(pngData, pngChunk("cICP", []byte{9, 16, 0, 1})),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Validate(context.Background(), writeFixture(t, data), config.DefaultLimits())
			if !errors.Is(err, ErrInvalidImage) {
				t.Fatalf("expected invalid image, got %v", err)
			}
		})
	}
	// acTL inside text data is not animation.
	data := insertChunk(pngData, pngChunk("tEXt", []byte("comment\x00acTL fdAT fcTL")))
	if _, err := Validate(context.Background(), writeFixture(t, data), config.DefaultLimits()); err != nil {
		t.Fatalf("false APNG match: %v", err)
	}
}

func TestLimitsAndCancellation(t *testing.T) {
	path, _ := fixture(t, "png", true)
	for _, change := range []func(*config.Limits){
		func(l *config.Limits) { l.UploadBytes = 10 },
		func(l *config.Limits) { l.Pixels = 10 },
		func(l *config.Limits) { l.Dimension = 10 },
	} {
		l := config.DefaultLimits()
		change(&l)
		if _, err := Validate(context.Background(), path, l); !errors.Is(err, ErrLimit) {
			t.Fatalf("expected input limit, got %v", err)
		}
	}
	l := config.DefaultLimits()
	l.OutputBytes = 10
	dir := t.TempDir()
	if _, err := Run(context.Background(), path, dir, smallProfile(), l); !errors.Is(err, ErrLimit) {
		t.Fatalf("expected output size limit, got %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("failed attempt left outputs")
	}
	baseline, err := Run(context.Background(), path, t.TempDir(), smallProfile(), config.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	l.OutputBytes = baseline[0].Bytes
	if _, err := Run(context.Background(), path, dir, smallProfile(), l); !errors.Is(err, ErrLimit) {
		t.Fatalf("expected aggregate limit after first complete output, got %v", err)
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("aggregate-limit failure left complete or temporary outputs")
	}
	l = config.DefaultLimits()
	l.OutputCount = 1
	if _, err := Run(context.Background(), path, dir, smallProfile(), l); err == nil {
		t.Fatal("expected output count limit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Validate(ctx, path, config.DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := Probe(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	if _, err := Run(context.Background(), path, filepath.Join(dir, "missing"), smallProfile(), config.DefaultLimits()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected unavailable storage, got %v", err)
	}
}

func TestUnavailableEncoder(t *testing.T) {
	// libheif loads its AV1 encoder from plugins. An empty plugin search path
	// proves startup does actual encoding rather than only testing file loaders.
	t.Setenv("LIBHEIF_PLUGIN_PATH", t.TempDir())
	if err := Probe(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing required AV1 plugin: expected unavailable, got %v", err)
	}
	for _, format := range Formats() {
		if format.Available {
			t.Fatalf("failed probe left stale availability: %+v", format)
		}
	}
}

func TestSixteenBitPNG(t *testing.T) {
	img := image.NewNRGBA64(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetNRGBA64(x, y, color.NRGBA64{uint16(x*1000 + 1), uint16(y*1000 + 7), 12003, 32001})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	p := config.DefaultProfile()
	p.Widths, p.Formats = []int{4}, []string{"png"}
	dir := t.TempDir()
	outputs, err := Run(context.Background(), writeFixture(t, buf.Bytes()), dir, p, config.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, outputs[0].Filename))
	got, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			if img.At(x, y) != got.At(x, y) {
				t.Fatalf("16-bit PNG changed: %v != %v", img.At(x, y), got.At(x, y))
			}
		}
	}
}

func TestColorProfiles(t *testing.T) {
	if err := startup(); err != nil {
		t.Fatal(err)
	}
	path, _ := fixture(t, "png", true)
	data, _ := os.ReadFile(path)
	profilePath, err := vips.GetSRGBV2MicroICCProfilePath()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	addICC := func(profile []byte) string {
		var buf bytes.Buffer
		z := zlib.NewWriter(&buf)
		_, _ = z.Write(profile)
		_ = z.Close()
		body := append([]byte("generated\x00\x00"), buf.Bytes()...)
		return writeFixture(t, insertChunk(data, pngChunk("iCCP", body)))
	}
	if _, err := Validate(context.Background(), addICC(profile), config.DefaultLimits()); err != nil {
		t.Fatalf("valid embedded sRGB: %v", err)
	}
	bad := make([]byte, 128)
	copy(bad[36:], "acsp")
	if _, err := Validate(context.Background(), addICC(bad), config.DefaultLimits()); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("invalid ICC not rejected: %v", err)
	}
	// Generate a real four-component JPEG without an ICC profile.
	pixels := bytes.Repeat([]byte{20, 40, 60, 10}, 16*16)
	img, err := vips.NewImageFromMemory(pixels, 16, 16, 4, vips.BandFormatUchar, vips.InterpretationCMYK)
	if err != nil {
		t.Fatal(err)
	}
	jpg, _, err := img.ExportJpeg(&vips.JpegExportParams{Quality: 90, StripMetadata: true})
	img.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(context.Background(), writeFixture(t, jpg), config.DefaultLimits()); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("unprofiled CMYK not rejected: %v", err)
	}
}

func TestICCConversionPixelProof(t *testing.T) {
	if err := startup(); err != nil {
		t.Fatal(err)
	}
	profilePath, err := vips.GetSRGBV2MicroICCProfilePath()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	// Generate a linear-light RGB profile from the fixture's sRGB primaries.
	// Replacing each TRC by gamma 1 makes ignoring this profile conspicuous:
	// a linear sample of 128 must become approximately 188 in encoded sRGB.
	tags := int(binary.BigEndian.Uint32(profile[128:132]))
	for i := 0; i < tags; i++ {
		tag := profile[132+i*12 : 144+i*12]
		switch string(tag[:4]) {
		case "rTRC", "gTRC", "bTRC":
			offset := int(binary.BigEndian.Uint32(tag[4:8]))
			binary.BigEndian.PutUint32(tag[8:12], 14)
			curve := profile[offset : offset+14]
			clear(curve)
			copy(curve, "curv")
			binary.BigEndian.PutUint32(curve[8:12], 1)
			binary.BigEndian.PutUint16(curve[12:14], 256)
		}
	}
	clear(profile[84:100])
	if !validICC(profile, 3) {
		t.Fatal("generated linear RGB profile is invalid")
	}
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	sample := color.NRGBA{64, 128, 192, 111}
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.SetNRGBA(x, y, sample)
		}
	}
	var pngData, compressed bytes.Buffer
	if err := png.Encode(&pngData, img); err != nil {
		t.Fatal(err)
	}
	z := zlib.NewWriter(&compressed)
	_, _ = z.Write(profile)
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	input := insertChunk(pngData.Bytes(), pngChunk("iCCP", append([]byte("linear\x00\x00"), compressed.Bytes()...)))
	p := config.DefaultProfile()
	p.Widths, p.Formats = nil, []string{"png"}
	dir := t.TempDir()
	outputs, err := Run(context.Background(), writeFixture(t, input), dir, p, config.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, outputs[0].Filename))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	toSRGB := func(value uint8) uint8 {
		v := float64(value) / 255
		if v <= 0.0031308 {
			v *= 12.92
		} else {
			v = 1.055*math.Pow(v, 1/2.4) - 0.055
		}
		return uint8(math.Round(v * 255))
	}
	expected := color.NRGBA{toSRGB(sample.R), toSRGB(sample.G), toSRGB(sample.B), sample.A}
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			got := color.NRGBAModel.Convert(decoded.At(x, y)).(color.NRGBA)
			for i, value := range []uint8{got.R, got.G, got.B} {
				want := []uint8{expected.R, expected.G, expected.B}[i]
				if math.Abs(float64(value)-float64(want)) > 3 {
					t.Fatalf("ICC conversion pixel (%d,%d): got %v, independently expected %v", x, y, got, expected)
				}
			}
			if got.A != expected.A {
				t.Fatalf("ICC conversion changed alpha: %d != %d", got.A, expected.A)
			}
		}
	}
	t.Logf("linear input %v converted to sRGB %v; expected %v (RGB tolerance 3, alpha exact)",
		sample, color.NRGBAModel.Convert(decoded.At(0, 0)), expected)
}

func TestCommandProtocol(t *testing.T) {
	for _, args := range [][]string{{}, {"api"}, {"worker"}} {
		if handled, err := HandleCommand(args); handled || err != nil {
			t.Fatalf("claimed noninternal command: %v", args)
		}
	}
	for _, args := range [][]string{{"internal-validate"}, {"internal-process"}, {"internal-probe", "extra"}} {
		if handled, err := HandleCommand(args); !handled || err == nil {
			t.Fatalf("bad protocol accepted: %v", args)
		}
	}
	for _, format := range Formats() {
		if !strings.HasPrefix(format.MIME, "image/") || !strings.HasPrefix(format.Extension, ".") {
			t.Fatalf("incomplete format registration: %+v", format)
		}
	}
}
