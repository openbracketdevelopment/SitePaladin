package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/openbracketdevelopment/bytedown/internal/config"
	"github.com/openbracketdevelopment/bytedown/internal/database"
	"github.com/openbracketdevelopment/bytedown/internal/processing"
	"github.com/openbracketdevelopment/bytedown/internal/storage"
)

const lease = 30 * time.Second

type Worker struct {
	Config config.Config
	DB     *database.Store
	Files  *storage.Store
}

func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := 0; i < w.Config.WorkerConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.reconcile(ctx)
	}()
	wg.Wait()
	return nil
}

func (w *Worker) reconcile(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		refs, err := w.DB.ReferencedPaths(ctx)
		if err == nil {
			err = w.Files.Reconcile(refs, time.Now())
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("storage reconciliation incomplete")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		j, err := w.DB.Claim(ctx, lease)
		if err == nil {
			w.process(ctx, j)
			continue
		}
		if !errors.Is(err, database.ErrNotFound) && ctx.Err() == nil {
			slog.Warn("job claim temporarily unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (w *Worker) process(parent context.Context, j database.Job) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(w.Config.Limits.ProcessingSeconds)*time.Second)
	defer cancel()
	done := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				beatCtx, stop := context.WithTimeout(ctx, lease/3)
				err := w.DB.Heartbeat(beatCtx, j.ID, j.Token, lease)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { close(done); <-heartbeatDone }()
	fail := func(code, message string, retry bool) {
		updateCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := w.DB.Fail(updateCtx, j.ID, j.Token, code, message, retry); err != nil && !errors.Is(err, database.ErrLeaseLost) {
			slog.Warn("job failure state update deferred to lease recovery", "job_id", j.ID)
		}
	}
	var profile config.Profile
	if j.Pipeline != processing.PipelineVersion || json.Unmarshal(j.Profile, &profile) != nil {
		fail("pipeline_unavailable", "This job requires an unavailable pipeline version.", false)
		return
	}
	stage, err := os.MkdirTemp(w.Files.Path("staging"), j.ID+"-"+j.Token+"-")
	if err != nil {
		fail("storage_unavailable", "Temporary storage unavailable.", true)
		return
	}
	defer os.RemoveAll(stage)
	progressStop := make(chan struct{})
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-progressStop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				entries, err := os.ReadDir(stage)
				if err != nil {
					continue
				}
				count := 0
				for _, entry := range entries {
					if entry.IsDir() || len(entry.Name()) == 0 || entry.Name()[0] == '.' {
						continue
					}
					switch filepath.Ext(entry.Name()) {
					case ".jpg", ".png", ".webp", ".avif":
						count++
					}
				}
				updateCtx, stop := context.WithTimeout(ctx, 3*time.Second)
				err = w.DB.Progress(updateCtx, j.ID, j.Token, count)
				stop()
				if errors.Is(err, database.ErrLeaseLost) {
					cancel()
					return
				}
			}
		}
	}()
	start := time.Now()
	outputs, err := processing.Run(ctx, w.Files.Path("originals/"+j.ImageID), stage, profile, w.Config.Limits)
	close(progressStop)
	<-progressDone
	if err != nil {
		switch {
		case parent.Err() != nil:
			fail("worker_stopped", "Worker stopped before processing completed.", true)
		case ctx.Err() != nil:
			fail("processing_timeout", "Processing exceeded its deadline or lost its lease.", false)
		case errors.Is(err, processing.ErrUnavailable):
			fail("processing_unavailable", "Processing infrastructure temporarily unavailable.", true)
		default:
			fail("processing_failed", "Image processing failed; no outputs were published.", false)
		}
		return
	}
	if len(outputs) != j.Planned {
		fail("incomplete_outputs", "The required output set was not produced.", false)
		return
	}
	relative := filepath.Join("outputs", j.ID, j.Token)
	destination := w.Files.Path(relative)
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		fail("storage_unavailable", "Output storage unavailable.", true)
		return
	}
	if err := storage.SyncDir(w.Files.Path("outputs")); err != nil {
		fail("storage_unavailable", "Output storage could not be synchronized.", true)
		return
	}
	assets := make([]database.Asset, 0, len(outputs))
	for _, out := range outputs {
		if out.Filename == "" || filepath.Base(out.Filename) != out.Filename || out.Filename == "." || out.Filename == ".." {
			fail("invalid_output", "Processor returned an invalid output.", false)
			return
		}
		f, err := os.Open(filepath.Join(stage, out.Filename))
		if err != nil {
			fail("storage_unavailable", "An output could not be read.", true)
			return
		}
		err = f.Sync()
		_ = f.Close()
		if err != nil {
			fail("storage_unavailable", "An output could not be synchronized.", true)
			return
		}
		// Asset identities remain stable across attempts; filenames are only
		// reachable after a fenced, complete database publication.
		sum := sha256.Sum256([]byte(j.ID + "/" + out.Format + "/" + out.Filename))
		assets = append(assets, database.Asset{
			ID: hex.EncodeToString(sum[:16]), ImageID: j.ImageID,
			Format: out.Format, Filename: filepath.Join(relative, out.Filename),
			Width: out.Width, Height: out.Height, Bytes: out.Bytes, DurationMS: out.DurationMS,
		})
	}
	if err := storage.SyncDir(stage); err != nil {
		fail("storage_unavailable", "Output staging could not be synchronized.", true)
		return
	}
	if err := os.Rename(stage, destination); err != nil {
		fail("storage_unavailable", "Output publication storage unavailable.", true)
		return
	}
	if err := storage.SyncDir(filepath.Dir(destination)); err != nil {
		fail("storage_unavailable", "Output directory could not be synchronized.", true)
		return
	}
	if err := w.DB.Progress(ctx, j.ID, j.Token, len(assets)); err != nil {
		// Do not remove published files on an ambiguous DB error. Reconciliation
		// reclaims them if no transaction references them after the grace period.
		return
	}
	if err := w.DB.Complete(ctx, j.ID, j.Token, assets, time.Since(start).Milliseconds()); err != nil {
		slog.Warn("manifest publication not confirmed; lease recovery will reconcile", "job_id", j.ID)
		return
	}
	slog.Info("image processing completed", "job_id", j.ID, "outputs", len(assets), "duration_ms", time.Since(start).Milliseconds())
}
