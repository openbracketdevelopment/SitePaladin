// Package config loads and validates the operator-controlled service configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL       string             `json:"-"`
	StorageDir        string             `json:"-"`
	APIKey            string             `json:"-"`
	Listen            string             `json:"-"`
	PublicVariants    bool               `json:"public_variants"`
	Origins           []string           `json:"origins"`
	Profiles          map[string]Profile `json:"profiles"`
	Limits            Limits             `json:"limits"`
	WorkerConcurrency int                `json:"worker_concurrency"`
}

type Profile struct {
	Widths        []int    `json:"widths"`
	Formats       []string `json:"formats"`
	IncludeSource bool     `json:"include_source"`
	JPEGQuality   int      `json:"jpeg_quality"`
	WebPQuality   int      `json:"webp_quality"`
	WebPEffort    int      `json:"webp_effort"`
	AVIFQuality   int      `json:"avif_quality"`
	AVIFEffort    int      `json:"avif_effort"`
}

type Limits struct {
	UploadBytes       int64 `json:"upload_bytes"`
	Pixels            int64 `json:"pixels"`
	Dimension         int   `json:"dimension"`
	OutputCount       int   `json:"output_count"`
	OutputBytes       int64 `json:"output_bytes"`
	ProcessingSeconds int   `json:"processing_seconds"`
}

func DefaultProfile() Profile {
	return Profile{
		Widths: []int{480, 960, 1440, 1920}, Formats: []string{"webp", "avif"},
		IncludeSource: true, JPEGQuality: 85, WebPQuality: 80, WebPEffort: 4,
		AVIFQuality: 50, AVIFEffort: 4,
	}
}

func DefaultLimits() Limits {
	return Limits{20 << 20, 40_000_000, 20_000, 17, 200 << 20, 120}
}

// Validate also protects callers loading a previously snapshotted job.
func (l Limits) Validate() error {
	if l.UploadBytes <= 0 || l.UploadBytes > 1<<40 ||
		l.Pixels <= 0 || l.Pixels > 1_000_000_000 ||
		l.Dimension <= 0 || l.Dimension > 1_000_000 ||
		l.OutputCount <= 0 || l.OutputCount > 1000 ||
		l.OutputBytes <= 0 || l.OutputBytes > 1<<40 ||
		l.ProcessingSeconds <= 0 || l.ProcessingSeconds > 86400 {
		return errors.New("invalid resource limits")
	}
	return nil
}

func (p Profile) Validate(l Limits) error {
	if len(p.Widths) == 0 && !p.IncludeSource {
		return errors.New("profile has no output widths")
	}
	previous := 0
	for _, w := range p.Widths {
		if w <= previous || w > l.Dimension {
			return errors.New("profile widths must be increasing, positive, and within the dimension limit")
		}
		previous = w
	}
	seen := map[string]bool{}
	for _, f := range p.Formats {
		if (f != "jpeg" && f != "png" && f != "webp" && f != "avif") || seen[f] {
			return errors.New("profile contains unsupported or duplicate formats")
		}
		seen[f] = true
	}
	if len(p.Formats) == 0 || p.JPEGQuality < 1 || p.JPEGQuality > 100 ||
		p.WebPQuality < 1 || p.WebPQuality > 100 || p.WebPEffort < 0 || p.WebPEffort > 6 ||
		p.AVIFQuality < 1 || p.AVIFQuality > 100 || p.AVIFEffort < 0 || p.AVIFEffort > 9 {
		return errors.New("profile has invalid codec settings")
	}
	// One full-size optimized copy in the input format is always generated.
	widths := len(p.Widths)
	if p.IncludeSource {
		widths++
	}
	if 1+widths*len(p.Formats) > l.OutputCount {
		return errors.New("profile exceeds output count limit")
	}
	return nil
}

var profileName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func Load() (Config, error) {
	c := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"), APIKey: os.Getenv("API_KEY"),
		StorageDir: env("STORAGE_DIR", "./data"), Listen: env("LISTEN", ":8080"),
		Profiles: map[string]Profile{"web": DefaultProfile()}, Limits: DefaultLimits(), WorkerConcurrency: 1,
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return Config{}, errors.New("DATABASE_URL must be a PostgreSQL URL")
	}
	if len(c.APIKey) < 32 || strings.TrimSpace(c.APIKey) != c.APIKey {
		return Config{}, errors.New("API_KEY must contain at least 32 characters without surrounding whitespace")
	}
	if strings.TrimSpace(c.StorageDir) == "" || strings.IndexByte(c.StorageDir, 0) >= 0 {
		return Config{}, errors.New("invalid STORAGE_DIR")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return Config{}, errors.New("LISTEN must be a host:port address")
	}
	if _, port, _ := net.SplitHostPort(c.Listen); port == "" {
		return Config{}, errors.New("LISTEN requires a port")
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return Config{}, errors.New("LISTEN port must be between 1 and 65535")
	}
	if s, ok := os.LookupEnv("PUBLIC_VARIANTS"); ok {
		c.PublicVariants, err = strconv.ParseBool(s)
		if err != nil {
			return Config{}, errors.New("invalid PUBLIC_VARIANTS")
		}
	}
	if s := os.Getenv("CORS_ORIGINS"); s != "" {
		for _, raw := range strings.Split(s, ",") {
			origin := strings.TrimSpace(raw)
			u, err := url.Parse(origin)
			if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				return Config{}, errors.New("CORS_ORIGINS must contain explicit HTTP origins")
			}
			c.Origins = append(c.Origins, origin)
		}
	}
	ints := []struct {
		name string
		set  func(int64)
	}{
		{"MAX_UPLOAD_BYTES", func(n int64) { c.Limits.UploadBytes = n }},
		{"MAX_PIXELS", func(n int64) { c.Limits.Pixels = n }},
		{"MAX_DIMENSION", func(n int64) { c.Limits.Dimension = int(n) }},
		{"MAX_OUTPUT_COUNT", func(n int64) { c.Limits.OutputCount = int(n) }},
		{"MAX_OUTPUT_BYTES", func(n int64) { c.Limits.OutputBytes = n }},
		{"PROCESSING_SECONDS", func(n int64) { c.Limits.ProcessingSeconds = int(n) }},
		{"WORKER_CONCURRENCY", func(n int64) { c.WorkerConcurrency = int(n) }},
	}
	for _, setting := range ints {
		if s, ok := os.LookupEnv(setting.name); ok {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return Config{}, fmt.Errorf("invalid %s", setting.name)
			}
			setting.set(n)
		}
	}
	if err := c.Limits.Validate(); err != nil {
		return Config{}, err
	}
	if c.WorkerConcurrency < 1 || c.WorkerConcurrency > 256 {
		return Config{}, errors.New("WORKER_CONCURRENCY must be between 1 and 256")
	}
	if s, ok := os.LookupEnv("PROFILES_JSON"); ok {
		c.Profiles = nil
		dec := json.NewDecoder(strings.NewReader(s))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c.Profiles); err != nil {
			return Config{}, errors.New("invalid PROFILES_JSON")
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return Config{}, errors.New("invalid trailing PROFILES_JSON")
		}
	}
	if len(c.Profiles) == 0 || len(c.Profiles) > 64 {
		return Config{}, errors.New("between 1 and 64 profiles are required")
	}
	for name, p := range c.Profiles {
		if !profileName.MatchString(name) {
			return Config{}, errors.New("invalid profile name")
		}
		if err := p.Validate(c.Limits); err != nil {
			return Config{}, fmt.Errorf("invalid profile %s: %w", name, err)
		}
	}
	return c, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
