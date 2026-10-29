package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var testContext = context.Background()

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin, err := Open(testContext, dsn)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "test_bytedown_" + hex.EncodeToString(b)
	if _, err := admin.pool.Exec(testContext, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(testContext, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		if _, err := admin.pool.Exec(testContext, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	if err := s.Migrate(testContext); err != nil {
		t.Fatal(err)
	}
	return s
}

func accept(t *testing.T, s *Store, id, key string, planned int) Image {
	t.Helper()
	im, existing, err := s.Accept(testContext, Image{ID: id, JobID: "job-" + id, Format: "png", Width: 32, Height: 24, Bytes: 100}, "hash", key, json.RawMessage(`{"width":32,"nested":{"a":1,"b":2}}`), "v1", planned)
	if err != nil || existing {
		t.Fatalf("accept: existing=%v err=%v", existing, err)
	}
	return im
}

func claim(t *testing.T, s *Store) Job {
	t.Helper()
	j, err := s.Claim(testContext, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func expire(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.pool.Exec(testContext, `UPDATE jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func asset(imageID, id string) Asset {
	return Asset{ID: id, ImageID: imageID, Format: "webp", Filename: "variants/" + imageID + "/" + id + ".webp", Width: 16, Height: 12, Bytes: 42, DurationMS: 8}
}

func TestMigrateConcurrent(t *testing.T) {
	s := testStore(t)
	if _, err := s.pool.Exec(testContext, `DROP TABLE assets,jobs,images,bytedown_migrations`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Migrate(testContext); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var count int
	if err := s.pool.QueryRow(testContext, `SELECT count(*) FROM bytedown_migrations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration count %d: %v", count, err)
	}
}

func TestAcceptIdempotency(t *testing.T) {
	s := testStore(t)
	first := accept(t, s, "first", "key", 2)
	got, existing, err := s.Accept(testContext, Image{ID: "unused", JobID: "unused-job", Format: "png", Width: 32, Height: 24}, "hash", "key", json.RawMessage(`{ "nested":{"b":2,"a":1}, "width":32 }`), "v1", 2)
	if err != nil || !existing || got.ID != first.ID {
		t.Fatalf("canonical JSONB replay: %#v %v %v", got, existing, err)
	}
	for _, tc := range []struct {
		hash, profile, pipeline string
	}{
		{"different", `{"width":32,"nested":{"a":1,"b":2}}`, "v1"},
		{"hash", `{"width":99}`, "v1"},
		{"hash", `{"width":32,"nested":{"a":1,"b":2}}`, "v2"},
	} {
		_, _, err := s.Accept(testContext, Image{ID: "conflict", JobID: "conflict-job", Format: "png", Width: 32, Height: 24}, tc.hash, "key", json.RawMessage(tc.profile), tc.pipeline, 2)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expected conflict: %v", err)
		}
	}
	accept(t, s, "second", "", 2)
	accept(t, s, "third", "", 2)
	_, _, err = s.Accept(testContext, Image{ID: "rollback", JobID: "rollback-job", Format: "png", Width: 32, Height: 24}, "hash", "", json.RawMessage(`{}`), "v1", 0)
	if err == nil {
		t.Fatal("invalid job accepted")
	}
	if _, err := s.GetImage(testContext, "rollback"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed acceptance left image: %v", err)
	}
}

func TestAcceptConcurrent(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(fmt.Sprint(same), func(t *testing.T) {
			s := testStore(t)
			const n = 12
			type result struct {
				im       Image
				existing bool
				err      error
			}
			results := make(chan result, n)
			start := make(chan struct{})
			for i := range n {
				go func(i int) {
					<-start
					hash := "same"
					if !same {
						hash = fmt.Sprint(i)
					}
					im, existing, err := s.Accept(testContext, Image{ID: fmt.Sprintf("image-%d", i), JobID: fmt.Sprintf("job-%d", i), Format: "png", Width: 1, Height: 1}, hash, "racing-key", json.RawMessage(`{}`), "v1", 1)
					results <- result{im, existing, err}
				}(i)
			}
			close(start)
			created, conflicts := 0, 0
			id := ""
			for range n {
				r := <-results
				if errors.Is(r.err, ErrConflict) {
					conflicts++
					continue
				}
				if r.err != nil {
					t.Fatal(r.err)
				}
				if !r.existing {
					created++
				}
				if id != "" && id != r.im.ID {
					t.Fatalf("multiple image IDs: %s %s", id, r.im.ID)
				}
				id = r.im.ID
			}
			if created != 1 || (same && conflicts != 0) || (!same && conflicts != n-1) {
				t.Fatalf("created=%d conflicts=%d", created, conflicts)
			}
		})
	}
}

func TestConcurrentClaim(t *testing.T) {
	s := testStore(t)
	const n = 8
	for i := range n {
		accept(t, s, fmt.Sprintf("image-%02d", i), "", 1)
	}
	jobs := make(chan Job, n*2)
	errs := make(chan error, n*2)
	var wg sync.WaitGroup
	for range n * 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := s.Claim(testContext, time.Minute)
			if err != nil {
				if !errors.Is(err, ErrNotFound) {
					errs <- err
				}
				return
			}
			jobs <- j
		}()
	}
	wg.Wait()
	close(jobs)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	seen := map[string]bool{}
	for j := range jobs {
		if seen[j.ID] || j.Attempt != 1 || len(j.Token) != 64 || j.Status != "processing" {
			t.Fatalf("bad or duplicate claim %#v", j)
		}
		seen[j.ID] = true
	}
	if len(seen) != n {
		t.Fatalf("claimed %d jobs, want %d", len(seen), n)
	}
}

func TestLeaseFencingAndRecovery(t *testing.T) {
	s := testStore(t)
	accept(t, s, "image", "", 1)
	j := claim(t, s)
	if err := s.Heartbeat(testContext, j.ID, j.Token, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress(testContext, j.ID, "wrong", 1); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong token: %v", err)
	}
	expire(t, s, j.ID)
	for name, fn := range map[string]func() error{
		"heartbeat": func() error { return s.Heartbeat(testContext, j.ID, j.Token, time.Minute) },
		"progress":  func() error { return s.Progress(testContext, j.ID, j.Token, 1) },
		"complete":  func() error { return s.Complete(testContext, j.ID, j.Token, []Asset{asset("image", "a")}, 1) },
		"fail":      func() error { return s.Fail(testContext, j.ID, j.Token, "failure", "failed", true) },
	} {
		if err := fn(); !errors.Is(err, ErrLeaseLost) {
			t.Errorf("%s expired lease: %v", name, err)
		}
	}
	next := claim(t, s)
	if next.Token == j.Token || next.Attempt != 2 {
		t.Fatalf("bad reclaim: %#v", next)
	}
	if err := s.Complete(testContext, j.ID, j.Token, []Asset{asset("image", "a")}, 1); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker: %v", err)
	}
	if err := s.Complete(testContext, next.ID, next.Token, []Asset{asset("image", "a")}, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress(testContext, next.ID, next.Token, 1); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("completed worker: %v", err)
	}
}

func TestRetryBounds(t *testing.T) {
	for _, crashed := range []bool{true, false} {
		t.Run(fmt.Sprint(crashed), func(t *testing.T) {
			s := testStore(t)
			accept(t, s, "image", "", 1)
			for attempt := 1; attempt <= 3; attempt++ {
				j := claim(t, s)
				if j.Attempt != attempt {
					t.Fatalf("attempt %d, want %d", j.Attempt, attempt)
				}
				if crashed {
					expire(t, s, j.ID)
				} else if err := s.Fail(testContext, j.ID, j.Token, "encode_failed", "Encoding failed", true); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Claim(testContext, time.Minute); !errors.Is(err, ErrNotFound) {
				t.Fatalf("fourth attempt: %v", err)
			}
			j, err := s.GetJob(testContext, "job-image")
			if err != nil || j.Status != "failed" || j.Attempt != 3 || j.Token != "" {
				t.Fatalf("exhausted job %#v: %v", j, err)
			}
			im, err := s.GetImage(testContext, "image")
			if err != nil || im.Status != "failed" || len(im.Outputs) != 0 {
				t.Fatalf("exhausted image %#v: %v", im, err)
			}
		})
	}
}

func TestAtomicPublication(t *testing.T) {
	s := testStore(t)
	accept(t, s, "image", "", 2)
	j := claim(t, s)
	if err := s.Progress(testContext, j.ID, j.Token, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress(testContext, j.ID, j.Token, 0); err == nil {
		t.Fatal("progress regressed")
	}
	if err := s.Progress(testContext, j.ID, j.Token, 3); err == nil {
		t.Fatal("progress exceeded plan")
	}
	a, b := asset("image", "a"), asset("image", "b")
	if err := s.Complete(testContext, j.ID, j.Token, []Asset{a}, 10); err == nil {
		t.Fatal("partial manifest accepted")
	}
	b.ID = a.ID
	if err := s.Complete(testContext, j.ID, j.Token, []Asset{a, b}, 10); err == nil {
		t.Fatal("duplicate asset accepted")
	}
	im, err := s.GetImage(testContext, "image")
	if err != nil || len(im.Outputs) != 0 || im.Status != "processing" {
		t.Fatalf("partial manifest leaked: %#v %v", im, err)
	}
	if _, err := s.GetAsset(testContext, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial asset visible: %v", err)
	}
	b.ID = "b"
	if err := s.Complete(testContext, j.ID, j.Token, []Asset{a, b}, 10); err != nil {
		t.Fatal(err)
	}
	im, err = s.GetImage(testContext, "image")
	if err != nil || len(im.Outputs) != 2 || im.Status != "succeeded" {
		t.Fatalf("publication: %#v %v", im, err)
	}
	published, err := s.GetAsset(testContext, "a")
	if err != nil || published != a {
		t.Fatalf("published asset %#v: %v", published, err)
	}
	j, err = s.GetJob(testContext, j.ID)
	if err != nil || j.Status != "succeeded" || j.Generated != 2 || j.ProcessingMS != 10 {
		t.Fatalf("completed job %#v %v", j, err)
	}
	paths, err := s.ReferencedPaths(testContext)
	if err != nil || len(paths) != 3 {
		t.Fatalf("references %#v %v", paths, err)
	}
	references := make(map[string]bool)
	for _, p := range paths {
		references[p] = true
	}
	if !references["originals/image"] || !references[a.Filename] || !references[b.Filename] {
		t.Fatalf("incorrect storage-relative references: %#v", paths)
	}
	deleted, err := s.Delete(testContext, "image")
	if err != nil || len(deleted.Outputs) != 2 {
		t.Fatalf("delete cleanup manifest %#v %v", deleted, err)
	}
	if _, err := s.GetJob(testContext, j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job did not cascade: %v", err)
	}
	if _, err := s.GetAsset(testContext, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("asset did not cascade: %v", err)
	}
	paths, err = s.ReferencedPaths(testContext)
	if err != nil || len(paths) != 0 {
		t.Fatalf("delete references %#v %v", paths, err)
	}
}

func TestFailedPartialAndDeletion(t *testing.T) {
	s := testStore(t)
	accept(t, s, "failed", "reusable", 2)
	j := claim(t, s)
	if err := s.Progress(testContext, j.ID, j.Token, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Fail(testContext, j.ID, j.Token, "encode_failed", "Encoding failed", false); err != nil {
		t.Fatal(err)
	}
	im, err := s.GetImage(testContext, "failed")
	if err != nil || im.Status != "failed" || len(im.Outputs) != 0 {
		t.Fatalf("failed partial %#v %v", im, err)
	}
	if _, err := s.Delete(testContext, "failed"); err != nil {
		t.Fatal(err)
	}
	accept(t, s, "processing", "reusable", 1)
	j = claim(t, s)
	if _, err := s.Delete(testContext, j.ImageID); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		s.Complete(testContext, j.ID, j.Token, []Asset{asset(j.ImageID, "a")}, 1),
		s.Heartbeat(testContext, j.ID, j.Token, time.Minute),
		s.Progress(testContext, j.ID, j.Token, 1),
		s.Fail(testContext, j.ID, j.Token, "failure", "Failed", true),
	} {
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("publication after delete: %v", err)
		}
	}
}

func TestDeleteCompleteRace(t *testing.T) {
	s := testStore(t)
	for i := range 12 {
		id := fmt.Sprintf("image-%02d", i)
		accept(t, s, id, "", 1)
		j := claim(t, s)
		done := make(chan error, 1)
		go func() { done <- s.Complete(testContext, j.ID, j.Token, []Asset{asset(id, "a")}, 1) }()
		deleted, err := s.Delete(testContext, id)
		if err != nil {
			t.Fatal(err)
		}
		err = <-done
		if err != nil && !errors.Is(err, ErrLeaseLost) {
			t.Fatal(err)
		}
		if err == nil && len(deleted.Outputs) != 1 {
			t.Fatal("delete did not return concurrently published manifest")
		}
		if _, err := s.GetImage(testContext, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted image visible: %v", err)
		}
	}
}

func TestListImages(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"a", "b", "c"} {
		accept(t, s, id, "", 1)
	}

	j := claim(t, s)
	if err := s.Fail(testContext, j.ID, j.Token, "failure", "Failed", false); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListImages(testContext, "", "", 2)
	if err != nil || len(page) != 2 || page[0].ID != "a" || page[1].ID != "b" {
		t.Fatalf("first page %#v %v", page, err)
	}
	page, err = s.ListImages(testContext, "b", "", 2)
	if err != nil || len(page) != 1 || page[0].ID != "c" {
		t.Fatalf("second page %#v %v", page, err)
	}
	page, err = s.ListImages(testContext, "", "failed", 10)
	if err != nil || len(page) != 1 || page[0].ID != j.ImageID {
		t.Fatalf("filtered page %#v %v", page, err)
	}
}

func TestListImagesLookahead(t *testing.T) {
	s := testStore(t)
	for i := range 103 {
		accept(t, s, fmt.Sprintf("image-%03d", i), "", 1)
	}
	for _, limit := range []int{101, 102} {
		page, err := s.ListImages(testContext, "", "", limit)
		if err != nil || len(page) != 101 {
			t.Fatalf("lookahead limit %d: got %d images, err=%v", limit, len(page), err)
		}
	}
	page, err := s.ListImages(testContext, "", "", 0)
	if err != nil || len(page) != 100 {
		t.Fatalf("default limit: got %d images, err=%v", len(page), err)
	}
}
