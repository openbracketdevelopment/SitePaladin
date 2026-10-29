// Package processing isolates native image work in deadline-controlled children.
package processing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/openbracketdevelopment/bytedown/internal/config"
)

const PipelineVersion = "bytedown-v1"

var (
	ErrInvalidImage = errors.New("invalid or unsupported image")
	ErrLimit        = errors.New("image resource limit exceeded")
	ErrUnavailable  = errors.New("image processing unavailable")
)

type Info struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Format string `json:"format"`
	Bytes  int64  `json:"bytes"`
}

type Output struct {
	ID         string `json:"id"`
	Format     string `json:"format"`
	Filename   string `json:"filename"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
}

type FormatInfo struct {
	ID           string   `json:"id"`
	Extension    string   `json:"extension"`
	MIME         string   `json:"mime"`
	Transparency bool     `json:"transparency"`
	Encoder      string   `json:"encoder"`
	Available    bool     `json:"available"`
	Options      []string `json:"options"`
}

var probeSucceeded atomic.Bool

func Formats() []FormatInfo {
	available := probeSucceeded.Load()
	return []FormatInfo{
		{"jpeg", ".jpg", "image/jpeg", false, "jpegsave_buffer", available, []string{"jpeg_quality"}},
		{"png", ".png", "image/png", true, "pngsave_buffer", available, []string{}},
		{"webp", ".webp", "image/webp", true, "webpsave_buffer", available, []string{"webp_quality", "webp_effort"}},
		{"avif", ".avif", "image/avif", true, "heifsave_buffer", available, []string{"avif_quality", "avif_effort"}},
	}
}

type request struct {
	Input   string         `json:"input"`
	Dir     string         `json:"dir,omitempty"`
	Profile config.Profile `json:"profile"`
	Limits  config.Limits  `json:"limits"`
}

type response struct {
	Info    Info     `json:"info"`
	Outputs []Output `json:"outputs,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func Validate(ctx context.Context, path string, limits config.Limits) (Info, error) {
	if err := limits.Validate(); err != nil {
		return Info{}, err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return Info{}, ErrInvalidImage
	}
	r, err := execute(ctx, "internal-validate", request{Input: path, Limits: limits}, filepath.Dir(path))
	return r.Info, err
}

func Run(ctx context.Context, input, dir string, profile config.Profile, limits config.Limits) ([]Output, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := profile.Validate(limits); err != nil {
		return nil, err
	}
	input, err := filepath.Abs(input)
	if err != nil {
		return nil, ErrInvalidImage
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, ErrUnavailable
	}
	r, err := execute(ctx, "internal-process", request{input, dir, profile, limits}, dir)
	return r.Outputs, err
}

func Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		return ErrUnavailable
	}
	cmd := exec.CommandContext(ctx, exe, "internal-probe")
	// No image bytes or native diagnostics are exposed to callers or logs.
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		probeSucceeded.Store(false)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnavailable
	}
	probeSucceeded.Store(true)
	return nil
}

func execute(ctx context.Context, command string, req request, base string) (response, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.Limits.ProcessingSeconds)*time.Second)
	defer cancel()
	control, err := os.MkdirTemp(base, ".bytedown-control-")
	if err != nil {
		return response{}, ErrUnavailable
	}
	defer os.RemoveAll(control)
	input, result := filepath.Join(control, "request.json"), filepath.Join(control, "result.json")
	data, err := json.Marshal(req)
	if err != nil || os.WriteFile(input, data, 0600) != nil {
		return response{}, ErrUnavailable
	}
	exe, err := os.Executable()
	if err != nil {
		return response{}, ErrUnavailable
	}
	cmd := exec.CommandContext(ctx, exe, command, input, result)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return response{}, ctx.Err()
		}
		return response{}, ErrUnavailable
	}
	var r response
	if err := readJSON(result, &r); err != nil {
		return response{}, ErrUnavailable
	}
	switch r.Error {
	case "":
		return r, nil
	case "invalid_image":
		return response{}, ErrInvalidImage
	case "limit_exceeded":
		return response{}, ErrLimit
	default:
		return response{}, ErrUnavailable
	}
}

// HandleCommand must be called by main before loading service configuration.
// Both internal-validate and internal-process take REQUEST_JSON RESULT_JSON.
// Native errors are returned in the result JSON; protocol/probe errors are errors.
func HandleCommand(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if args[0] != "internal-validate" && args[0] != "internal-process" && args[0] != "internal-probe" {
		return false, nil
	}
	if args[0] == "internal-probe" {
		if len(args) != 1 {
			return true, ErrUnavailable
		}
		return true, nativeProbe()
	}
	if len(args) != 3 {
		return true, ErrUnavailable
	}
	var req request
	if err := readJSON(args[1], &req); err != nil {
		return true, ErrUnavailable
	}
	if err := req.Limits.Validate(); err != nil {
		return true, ErrLimit
	}
	var r response
	var err error
	if args[0] == "internal-validate" {
		r.Info, err = nativeValidate(req.Input, req.Limits)
	} else {
		if err = req.Profile.Validate(req.Limits); err == nil {
			r.Outputs, err = nativeRun(req.Input, req.Dir, req.Profile, req.Limits)
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidImage):
			r.Error = "invalid_image"
		case errors.Is(err, ErrLimit):
			r.Error = "limit_exceeded"
		default:
			r.Error = "unavailable"
		}
	}
	data, err := json.Marshal(r)
	if err != nil {
		return true, ErrUnavailable
	}
	// Results are created exclusively, never through an existing symlink.
	f, err := os.OpenFile(args[2], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return true, ErrUnavailable
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return true, ErrUnavailable
	}
	return true, nil
}

func readJSON(path string, value any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return ErrUnavailable
	}
	return nil
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
