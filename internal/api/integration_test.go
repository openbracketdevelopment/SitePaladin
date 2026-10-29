package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/openbracketdevelopment/bytedown/internal/database"
	"github.com/openbracketdevelopment/bytedown/internal/jobs"
	"github.com/openbracketdevelopment/bytedown/internal/processing"
	"github.com/openbracketdevelopment/bytedown/internal/storage"
)

func TestMain(m *testing.M) {
	if handled, err := processing.HandleCommand(os.Args[1:]); handled {
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestHTTPLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for HTTP/PostgreSQL integration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := "api_" + storage.ID()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := database.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	s.DB = db
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	worker := jobs.Worker{Config: s.Config, DB: db, Files: s.Files}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); _ = worker.Run(ctx) }()
	defer func() { cancel(); <-workerDone }()
	call := func(method, path string, body io.Reader, contentType, key string) *http.Response {
		t.Helper()
		r, err := http.NewRequest(method, server.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+s.Config.APIKey)
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	fixture := image.NewNRGBA(image.Rect(0, 0, 32, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 32; x++ {
			fixture.SetNRGBA(x, y, color.NRGBA{uint8(7 * x), uint8(13 * y), 100, uint8(8 * x)})
		}
	}
	for _, format := range []string{"png", "jpeg"} {
		t.Run(format, func(t *testing.T) {
			var source bytes.Buffer
			if format == "png" {
				err = png.Encode(&source, fixture)
			} else {
				err = jpeg.Encode(&source, fixture, &jpeg.Options{Quality: 90})
			}
			if err != nil {
				t.Fatal(err)
			}
			upload := func(data []byte, key string) (int, map[string]string) {
				t.Helper()
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				// Neither this misleading filename nor the default submitted
				// application/octet-stream type decides the decoded format.
				part, _ := writer.CreateFormFile("file", "../../wrong.gif")
				_, _ = part.Write(data)
				_ = writer.WriteField("profile", "web")
				_ = writer.Close()
				response := call("POST", "/v1/images", &body, writer.FormDataContentType(), key)
				defer response.Body.Close()
				result := map[string]string{}
				if response.StatusCode == 202 {
					if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
						t.Fatal(err)
					}
				}
				return response.StatusCode, result
			}
			key := storage.ID()
			status, accepted := upload(source.Bytes(), key)
			if status != 202 {
				t.Fatalf("upload: %d", status)
			}
			repeatedStatus, repeated := upload(source.Bytes(), key)
			if repeatedStatus != 202 || repeated["image_id"] != accepted["image_id"] || repeated["job_id"] != accepted["job_id"] {
				t.Fatal("idempotent replay changed identity")
			}
			var other bytes.Buffer
			if err := png.Encode(&other, image.NewNRGBA(image.Rect(0, 0, 8, 8))); err != nil {
				t.Fatal(err)
			}
			conflict, _ := upload(other.Bytes(), key)
			if conflict != 409 {
				t.Fatalf("conflicting replay: %d", conflict)
			}
			deadline := time.Now().Add(30 * time.Second)
			for {
				response := call("GET", accepted["status_url"], nil, "", "")
				var job struct {
					Status    string `json:"status"`
					Generated int    `json:"generated"`
					Planned   int    `json:"planned"`
				}
				err := json.NewDecoder(response.Body).Decode(&job)
				response.Body.Close()
				if err != nil || job.Status == "failed" {
					t.Fatalf("job failed: %v %+v", err, job)
				}
				if job.Status == "succeeded" {
					if job.Generated != 3 || job.Planned != 3 {
						t.Fatalf("counts: %+v", job)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("processing deadline")
				}
				time.Sleep(50 * time.Millisecond)
			}
			response := call("GET", "/v1/images/"+accepted["image_id"]+"/original", nil, "", "")
			original, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 || !bytes.Equal(original, source.Bytes()) {
				t.Fatal("original changed")
			}
			response = call("GET", "/v1/images/"+accepted["image_id"], nil, "", "")
			var manifest struct {
				Format  string `json:"format"`
				Outputs []struct {
					URL           string `json:"url"`
					Bytes         int64  `json:"bytes"`
					Width, Height int
				} `json:"outputs"`
			}
			err = json.NewDecoder(response.Body).Decode(&manifest)
			response.Body.Close()
			if err != nil || manifest.Format != format || len(manifest.Outputs) != 3 {
				t.Fatalf("manifest: %v %+v", err, manifest)
			}
			publicConfig := s.Config
			publicConfig.PublicVariants = true
			publicServer := httptest.NewServer(New(publicConfig, db, s.Files).Handler())
			defer publicServer.Close()
			for _, output := range manifest.Outputs {
				response = call("GET", output.URL, nil, "", "")
				b, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != 200 || int64(len(b)) != output.Bytes || output.Width != 32 || output.Height != 16 {
					t.Fatal("invalid downloaded output")
				}
				if response.Header.Get("Cache-Control") != "private, no-store" {
					t.Fatal("private variant cache policy")
				}
				publicResponse, err := http.Get(publicServer.URL + output.URL)
				if err != nil {
					t.Fatal(err)
				}
				publicResponse.Body.Close()
				if publicResponse.StatusCode != 200 || !strings.HasPrefix(publicResponse.Header.Get("Cache-Control"), "public,") {
					t.Fatal("intentional public variant access")
				}
			}
			response = call("DELETE", "/v1/images/"+accepted["image_id"], nil, "", "")
			response.Body.Close()
			if response.StatusCode != 204 {
				t.Fatal("delete failed")
			}
			response = call("GET", manifest.Outputs[0].URL, nil, "", "")
			response.Body.Close()
			if response.StatusCode != 404 {
				t.Fatal("deleted media still visible")
			}
			response = call("GET", accepted["status_url"], nil, "", "")
			response.Body.Close()
			if response.StatusCode != 404 {
				t.Fatal("deleted job still visible")
			}
		})
	}
}
