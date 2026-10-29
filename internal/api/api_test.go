package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openbracketdevelopment/bytedown/internal/config"
	"github.com/openbracketdevelopment/bytedown/internal/database"
	"github.com/openbracketdevelopment/bytedown/internal/storage"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	files, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(config.Config{
		APIKey: strings.Repeat("x", 32), WorkerConcurrency: 1,
		Profiles: map[string]config.Profile{"web": config.DefaultProfile()}, Limits: config.DefaultLimits(),
	}, nil, files)
}

func TestAuthentication(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/v1/capabilities", "/v1/images", "/v1/jobs/123", "/v1/images/123/original", "/media/123"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%s: got %d", path, w.Code)
		}
	}
	for _, authorization := range []string{"Bearer " + s.Config.APIKey, s.Config.APIKey, "******", "Basic " + s.Config.APIKey} {
		r := httptest.NewRequest("GET", "/v1/capabilities", nil)
		r.Header.Set("Authorization", authorization)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		want := 401
		if authorization == "Bearer "+s.Config.APIKey {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("got %d want %d", w.Code, want)
		}
		if strings.Contains(w.Body.String(), s.Config.APIKey) {
			t.Fatal("response contains credential")
		}
	}
}

func TestLivenessAndCORS(t *testing.T) {
	s := testServer(t)
	request := httptest.NewRequest("GET", "/health/live", nil)
	request.Header.Set("Origin", "https://example.test")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, request)
	if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("liveness/CORS defaults")
	}
	s.Config.Origins = []string{"https://example.test"}
	for _, origin := range []string{"https://example.test", "https://evil.test"} {
		r := httptest.NewRequest("OPTIONS", "/v1/images", nil)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if origin == s.Config.Origins[0] {
			if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("allowed preflight")
			}
		} else if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("unapproved origin allowed")
		}
	}
}

func TestStreamingUploadLimit(t *testing.T) {
	s := testServer(t)
	s.Config.Limits.UploadBytes = 10
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "../../ignored.png")
	_, _ = part.Write(bytes.Repeat([]byte{1}, 11))
	_ = writer.Close()
	r := httptest.NewRequest(http.MethodPost, "/v1/images", &body)
	r.ContentLength = -1
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+s.Config.APIKey)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
}

func TestWidthPlanning(t *testing.T) {
	p := config.DefaultProfile()
	for width, want := range map[int]int{100: 3, 480: 3, 800: 5, 1920: 9, 2500: 11} {
		if got := plannedOutputs(width, p, "jpeg"); got != want {
			t.Fatalf("%d: %d != %d", width, got, want)
		}
	}
	p.Widths = []int{1000}
	p.Formats = []string{"jpeg", "webp"}
	if got := plannedOutputs(1000, p, "jpeg"); got != 2 {
		t.Fatalf("mandatory original-format copy must be deduplicated: %d", got)
	}
}

func TestHonestManifest(t *testing.T) {
	s := testServer(t)
	im := database.Image{ID: storage.ID(), Width: 100, Height: 50, Bytes: 100,
		Outputs: []database.Asset{{ID: storage.ID(), Width: 100, Height: 50, Bytes: 120, Filename: "/private/path"},
			{ID: storage.ID(), Width: 50, Height: 25, Bytes: 60}}}
	m := s.manifest(im)
	out := m["outputs"].([]map[string]any)
	if out[0]["smaller"] != false || out[0]["percent_size_difference"] != float64(20) || out[0]["includes_resizing"] != false {
		t.Fatalf("incorrect full-size reporting: %v", out[0])
	}

	if out[1]["smaller"] != true || out[1]["includes_resizing"] != true {
		t.Fatal("resize must be labeled")
	}
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), "/private/path") {
		t.Fatal("manifest leaked storage path")
	}
}

func TestInvalidUploadsNeverReachDatabase(t *testing.T) {
	var valid bytes.Buffer
	if err := png.Encode(&valid, image.NewNRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		data        []byte
		pixels      int64
		outputCount int
	}{
		{"unsupported", []byte("GIF89a unsupported"), 40_000_000, 17},
		{"truncated", valid.Bytes()[:valid.Len()-8], 40_000_000, 17},
		{"pixels", valid.Bytes(), 64, 17},
		{"outputs", valid.Bytes(), 40_000_000, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			s.Config.Limits.Pixels = tc.pixels
			s.Config.Limits.OutputCount = tc.outputCount
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, _ := writer.CreateFormFile("file", "misleading.jpg")
			_, _ = part.Write(tc.data)
			_ = writer.Close()
			r := httptest.NewRequest("POST", "/v1/images", &body)
			r.Header.Set("Content-Type", writer.FormDataContentType())
			r.Header.Set("Authorization", "Bearer "+s.Config.APIKey)
			w := httptest.NewRecorder()
			// A nil database ensures rejected input cannot create a record.
			s.Handler().ServeHTTP(w, r)
			if w.Code != 422 {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), s.Files.Dir) {
				t.Fatal("error exposed filesystem path")
			}
		})
	}
}
