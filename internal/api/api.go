package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openbracketdevelopment/bytedown/internal/config"
	"github.com/openbracketdevelopment/bytedown/internal/database"
	"github.com/openbracketdevelopment/bytedown/internal/processing"
	"github.com/openbracketdevelopment/bytedown/internal/storage"
)

const Version = "0.1.0"

type Server struct {
	Config   config.Config
	DB       *database.Store
	Files    *storage.Store
	slots    chan struct{}
	probeMu  sync.Mutex
	probeAt  time.Time
	probeErr error
}

func New(c config.Config, db *database.Store, files *storage.Store) *Server {
	return &Server{Config: c, DB: db, Files: files, slots: make(chan struct{}, c.WorkerConcurrency)}
}

func respond(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func failure(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func (s *Server) authorized(r *http.Request) bool {
	got := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	want := sha256.Sum256([]byte(s.Config.APIKey))
	return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]string{"status": "ok"})
	})
	m.HandleFunc("GET /health/ready", s.ready)
	m.HandleFunc("GET /v1/capabilities", s.capabilities)
	m.HandleFunc("POST /v1/images", s.upload)
	m.HandleFunc("GET /v1/images", s.list)
	m.HandleFunc("GET /v1/images/{id}", s.image)
	m.HandleFunc("GET /v1/jobs/{id}", s.job)
	m.HandleFunc("GET /v1/images/{id}/original", s.original)
	m.HandleFunc("DELETE /v1/images/{id}", s.delete)
	m.HandleFunc("GET /media/{id}", s.media)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		failure(w, 404, "not_found", "Resource not found.")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		origin := r.Header.Get("Origin")
		for _, allowed := range s.Config.Origins {
			if origin != "" && origin == allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				if r.Method == "OPTIONS" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				break
			}
		}
		public := r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" ||
			(s.Config.PublicVariants && strings.HasPrefix(r.URL.Path, "/media/"))
		if !public && !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			failure(w, 401, "unauthorized", "Valid server credentials are required.")
			return
		}
		m.ServeHTTP(w, r)
	})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if s.DB.Ping(ctx) != nil || s.Files.Ready() != nil || s.probe(ctx) != nil {
		failure(w, 503, "not_ready", "Service is not ready.")
		return
	}
	respond(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) probe(ctx context.Context) error {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if time.Since(s.probeAt) >= 30*time.Second {
		s.probeErr = processing.Probe(ctx)
		s.probeAt = time.Now()
	}
	return s.probeErr
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	formats := map[string]bool{"jpeg": true, "png": true}
	for _, p := range s.Config.Profiles {
		for _, f := range p.Formats {
			formats[f] = true
		}
	}
	configured := make([]string, 0, len(formats))
	for f := range formats {
		configured = append(configured, f)
	}
	sort.Strings(configured)
	available := make([]string, 0)
	registry := processing.Formats()
	for _, format := range registry {
		if format.Available {
			available = append(available, format.ID)
		}
	}
	respond(w, 200, map[string]any{
		"service_version": Version, "pipeline_version": processing.PipelineVersion,
		"accepted_inputs": []string{"jpeg", "png"}, "configured_outputs": configured,
		"available_outputs": available, "formats": registry,
		"profiles": s.Config.Profiles, "limits": s.Config.Limits, "public_variants": s.Config.PublicVariants,
	})
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		failure(w, 503, "busy", "Upload validation is busy; retry later.")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 200 || strings.TrimSpace(key) != key {
		failure(w, 400, "invalid_key", "Idempotency-Key must be at most 200 bytes with no surrounding whitespace.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.Config.Limits.UploadBytes+65536)
	reader, err := r.MultipartReader()
	if err != nil {
		failure(w, 400, "invalid_multipart", "Expected a multipart upload.")
		return
	}
	tmp, err := os.CreateTemp(s.Files.Path("staging"), "upload-")
	if err != nil {
		s.unavailable(w)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hash := sha256.New()
	fileSeen, profileSeen := false, false
	profileName := "web"
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.uploadError(w, err)
			return
		}
		switch part.FormName() {
		case "file":
			if fileSeen {
				failure(w, 400, "invalid_multipart", "Exactly one file is required.")
				return
			}
			fileSeen = true
			n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(part, s.Config.Limits.UploadBytes+1))
			if n > s.Config.Limits.UploadBytes {
				failure(w, 413, "upload_limit", "Upload exceeds the configured byte limit.")
				return
			}
			if err != nil {
				s.uploadError(w, err)
				return
			}
		case "profile":
			if profileSeen {
				failure(w, 400, "invalid_multipart", "Only one profile field is allowed.")
				return
			}
			profileSeen = true
			b, err := io.ReadAll(io.LimitReader(part, 129))
			if err != nil || len(b) > 128 {
				failure(w, 400, "invalid_profile", "Invalid profile field.")
				return
			}
			profileName = string(b)
		default:
			failure(w, 400, "invalid_multipart", "Only file and profile fields are supported.")
			return
		}
		if err := part.Close(); err != nil {
			s.uploadError(w, err)
			return
		}
	}
	profile, ok := s.Config.Profiles[profileName]
	if !ok || !fileSeen {
		failure(w, 422, "invalid_upload", "A file and a configured profile are required.")
		return
	}
	if err := tmp.Sync(); err != nil {
		s.unavailable(w)
		return
	}
	if err := tmp.Close(); err != nil {
		s.unavailable(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.Config.Limits.ProcessingSeconds)*time.Second)
	defer cancel()
	info, err := processing.Validate(ctx, tmp.Name(), s.Config.Limits)
	if err != nil {
		failure(w, 422, "invalid_image", "Image is unsupported, corrupt, exceeds limits, or has an unsupported color configuration.")
		return
	}
	planned := plannedOutputs(info.Width, profile, info.Format)
	if planned > s.Config.Limits.OutputCount {
		failure(w, 422, "output_limit", "Profile exceeds the configured output count.")
		return
	}
	id, jobID := storage.ID(), storage.ID()
	destination := s.Files.Path("originals/" + id)
	if err := os.Rename(tmp.Name(), destination); err != nil {
		s.unavailable(w)
		return
	}
	// On uncertain database commit retain bytes for reconciliation, not deletion:
	// a disconnected client can receive an error after PostgreSQL committed.
	if err := storage.SyncDir(filepath.Dir(destination)); err != nil {
		s.unavailable(w)
		return
	}
	profileJSON, _ := json.Marshal(profile)
	image, existing, err := s.DB.Accept(r.Context(), database.Image{
		ID: id, JobID: jobID, Format: info.Format, Width: info.Width, Height: info.Height, Bytes: info.Bytes,
	}, hex.EncodeToString(hash.Sum(nil)), key, profileJSON, processing.PipelineVersion, planned)
	if existing || errors.Is(err, database.ErrConflict) {
		_ = s.Files.RemoveOriginal(id)
	}
	if errors.Is(err, database.ErrConflict) {
		failure(w, 409, "idempotency_conflict", "This key was already used with different content or processing settings.")
		return
	}
	if err != nil {
		s.unavailable(w)
		return
	}
	respond(w, 202, map[string]string{"image_id": image.ID, "job_id": image.JobID, "status_url": "/v1/jobs/" + image.JobID})
}

func plannedOutputs(width int, p config.Profile, inputFormat string) int {
	widths := make(map[int]bool)
	for _, w := range p.Widths {
		if w <= width {
			widths[w] = true
		}
	}
	if p.IncludeSource {
		widths[width] = true
	}
	count := 1
	for w := range widths {
		for _, f := range p.Formats {
			if f != inputFormat || w != width {
				count++
			}
		}
	}
	return count
}

func (s *Server) uploadError(w http.ResponseWriter, err error) {
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		failure(w, 413, "upload_limit", "Upload exceeds the configured byte limit.")
		return
	}
	failure(w, 400, "invalid_multipart", "Malformed or incomplete multipart upload.")
}

func (s *Server) unavailable(w http.ResponseWriter) {
	slog.Error("request infrastructure failure")
	failure(w, 503, "unavailable", "Service temporarily unavailable.")
}

func (s *Server) lookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, database.ErrNotFound) {
		failure(w, 404, "not_found", "Resource not found.")
	} else {
		s.unavailable(w)
	}
}

func (s *Server) manifest(im database.Image) map[string]any {
	outputs := make([]map[string]any, 0, len(im.Outputs))
	for _, a := range im.Outputs {
		difference := float64(0)
		if im.Bytes > 0 {
			difference = 100 * (float64(a.Bytes) - float64(im.Bytes)) / float64(im.Bytes)
		}
		outputs = append(outputs, map[string]any{
			"id": a.ID, "format": a.Format, "width": a.Width, "height": a.Height,
			"bytes": a.Bytes, "duration_ms": a.DurationMS, "url": "/media/" + a.ID,
			"original_bytes": im.Bytes, "original_width": im.Width, "original_height": im.Height,
			"percent_size_difference": difference, "smaller": a.Bytes < im.Bytes,
			"includes_resizing": a.Width != im.Width || a.Height != im.Height,
		})
	}
	return map[string]any{
		"id": im.ID, "job_id": im.JobID, "status": im.Status, "format": im.Format,
		"width": im.Width, "height": im.Height, "bytes": im.Bytes, "created_at": im.CreatedAt,
		"original_url": "/v1/images/" + im.ID + "/original", "outputs": outputs,
	}
}

func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	im, err := s.DB.GetImage(r.Context(), r.PathValue("id"))
	if err != nil {
		s.lookupError(w, err)
		return
	}
	respond(w, 200, s.manifest(im))
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	j, err := s.DB.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.lookupError(w, err)
		return
	}
	respond(w, 200, map[string]any{"id": j.ID, "image_id": j.ImageID, "status": j.Status,
		"generated": j.Generated, "planned": j.Planned, "error_code": j.ErrorCode,
		"error_message": j.ErrorMessage, "processing_ms": j.ProcessingMS})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 20
	if q.Get("limit") != "" {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > 100 {
			failure(w, 400, "invalid_limit", "limit must be between 1 and 100.")
			return
		}
		limit = n
	}
	status, cursor := q.Get("status"), q.Get("cursor")
	if status != "" && status != "queued" && status != "processing" && status != "succeeded" && status != "failed" {
		failure(w, 400, "invalid_status", "Unknown status filter.")
		return
	}
	if cursor != "" && !storage.ValidID(cursor) {
		failure(w, 400, "invalid_cursor", "Invalid pagination cursor.")
		return
	}
	images, err := s.DB.ListImages(r.Context(), cursor, status, limit+1)
	if err != nil {
		s.unavailable(w)
		return
	}
	next := ""
	if len(images) > limit {
		images = images[:limit]
		next = images[len(images)-1].ID
	}
	result := make([]map[string]any, 0, len(images))
	for _, im := range images {
		result = append(result, s.manifest(im))
	}
	respond(w, 200, map[string]any{"images": result, "next_cursor": next})
}

func mime(format string) string {
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	case "avif":
		return "image/avif"
	default:
		return "application/octet-stream"
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, relative, format, id string, public bool) {
	f, err := os.Open(s.Files.Path(relative))
	if err != nil {
		s.unavailable(w)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		s.unavailable(w)
		return
	}
	w.Header().Set("Content-Type", mime(format))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", fmt.Sprintf("%q", id))
	if public {
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	} else {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	http.ServeContent(w, r, id, info.ModTime(), f)
}

func (s *Server) original(w http.ResponseWriter, r *http.Request) {
	im, err := s.DB.GetImage(r.Context(), r.PathValue("id"))
	if err != nil {
		s.lookupError(w, err)
		return
	}
	s.serve(w, r, "originals/"+im.ID, im.Format, im.ID, false)
}

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	a, err := s.DB.GetAsset(r.Context(), r.PathValue("id"))
	if err != nil {
		s.lookupError(w, err)
		return
	}
	s.serve(w, r, a.Filename, a.Format, a.ID, s.Config.PublicVariants)
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	im, err := s.DB.Delete(r.Context(), r.PathValue("id"))
	if err != nil {
		s.lookupError(w, err)
		return
	}
	_ = s.Files.RemoveOriginal(im.ID)
	for _, a := range im.Outputs {
		_ = os.Remove(s.Files.Path(a.Filename))
	}
	w.WriteHeader(204)
}
