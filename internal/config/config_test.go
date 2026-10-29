package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func setup(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://localhost/bytedown")
	t.Setenv("API_KEY", strings.Repeat("a", 32))
	for _, k := range []string{"STORAGE_DIR", "LISTEN", "PUBLIC_VARIANTS", "CORS_ORIGINS", "PROFILES_JSON",
		"MAX_UPLOAD_BYTES", "MAX_PIXELS", "MAX_DIMENSION", "MAX_OUTPUT_COUNT", "MAX_OUTPUT_BYTES",
		"PROCESSING_SECONDS", "WORKER_CONCURRENCY"} {
		t.Setenv(k, "")
		// Empty explicitly configured values are errors for typed settings.
		t.Setenv(k, defaultEnv(k))
	}
	p, _ := json.Marshal(map[string]Profile{"web": DefaultProfile()})
	t.Setenv("PROFILES_JSON", string(p))
}

func defaultEnv(key string) string {
	return map[string]string{
		"STORAGE_DIR": "./data", "LISTEN": ":8080", "PUBLIC_VARIANTS": "false",
		"MAX_UPLOAD_BYTES": "20971520", "MAX_PIXELS": "40000000", "MAX_DIMENSION": "20000",
		"MAX_OUTPUT_COUNT": "17", "MAX_OUTPUT_BYTES": "209715200", "PROCESSING_SECONDS": "120",
		"WORKER_CONCURRENCY": "1",
	}[key]
}

func TestLoad(t *testing.T) {
	setup(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits != DefaultLimits() || c.WorkerConcurrency != 1 || c.PublicVariants || len(c.Profiles) != 1 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	t.Setenv("PUBLIC_VARIANTS", "true")
	t.Setenv("CORS_ORIGINS", "https://example.com,http://localhost:3000")
	t.Setenv("WORKER_CONCURRENCY", "3")
	t.Setenv("MAX_UPLOAD_BYTES", "1234")
	c, err = Load()
	if err != nil || len(c.Origins) != 2 || !c.PublicVariants || c.WorkerConcurrency != 3 || c.Limits.UploadBytes != 1234 {
		t.Fatalf("overrides: %+v, %v", c, err)
	}
}

func TestRejectInvalidEnvironment(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"DATABASE_URL", ""}, {"DATABASE_URL", "http://localhost/db"}, {"API_KEY", "short"},
		{"LISTEN", "8080"}, {"LISTEN", ":0"}, {"LISTEN", ":999999"}, {"STORAGE_DIR", ""},
		{"PUBLIC_VARIANTS", "maybe"}, {"CORS_ORIGINS", "*"}, {"CORS_ORIGINS", "https://site.test/path"},
		{"CORS_ORIGINS", "https://user@site.test"}, {"MAX_PIXELS", "-1"}, {"MAX_DIMENSION", "0"},
		{"MAX_UPLOAD_BYTES", "bad"}, {"MAX_OUTPUT_COUNT", "2"}, {"MAX_OUTPUT_BYTES", "0"},
		{"PROCESSING_SECONDS", "0"}, {"WORKER_CONCURRENCY", "0"},
		{"PROFILES_JSON", "{}"}, {"PROFILES_JSON", "null"}, {"PROFILES_JSON", `{"web":{"unknown":true}}`},
		{"PROFILES_JSON", `{} {}`},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			setup(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("expected invalid configuration to fail")
			}
		})
	}
}

func TestProfileValidation(t *testing.T) {
	for _, modify := range []func(*Profile){
		func(p *Profile) { p.Widths = []int{960, 480} },
		func(p *Profile) { p.Widths = []int{0} },
		func(p *Profile) { p.Formats = []string{"gif"} },
		func(p *Profile) { p.Formats = []string{"webp", "webp"} },
		func(p *Profile) { p.JPEGQuality = 0 },
		func(p *Profile) { p.AVIFQuality = 101 },
		func(p *Profile) { p.AVIFEffort = 10 },
		func(p *Profile) { p.WebPEffort = 7 },
	} {
		p := DefaultProfile()
		modify(&p)
		if p.Validate(DefaultLimits()) == nil {
			t.Fatalf("accepted invalid profile: %+v", p)
		}
	}
}
