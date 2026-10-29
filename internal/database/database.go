// Package database owns durable image metadata, publication, and worker leases.
package database

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrConflict  = errors.New("idempotency key conflict")
	ErrNotFound  = errors.New("not found")
	ErrLeaseLost = errors.New("worker lease lost")
)

type Image struct {
	ID        string    `json:"id"`
	JobID     string    `json:"job_id"`
	Status    string    `json:"status"`
	Format    string    `json:"format"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
	Outputs   []Asset   `json:"outputs"`
}

type Asset struct {
	ID         string `json:"id"`
	ImageID    string `json:"image_id"`
	Format     string `json:"format"`
	Filename   string `json:"filename"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
}

type Job struct {
	ID           string          `json:"id"`
	ImageID      string          `json:"image_id"`
	Status       string          `json:"status"`
	Generated    int             `json:"generated"`
	Planned      int             `json:"planned"`
	Profile      json.RawMessage `json:"profile"`
	Pipeline     string          `json:"pipeline"`
	Attempt      int             `json:"attempt"`
	Token        string          `json:"-"`
	ErrorCode    string          `json:"error_code"`
	ErrorMessage string          `json:"error_message"`
	ProcessingMS int64           `json:"processing_ms"`
}

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate serializes migration runners using a transaction-scoped advisory lock.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(726985103); CREATE TABLE IF NOT EXISTS bytedown_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var applied bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM bytedown_migrations WHERE version=$1)`, name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		sql, readErr := migrations.ReadFile(name)
		if readErr != nil {
			return readErr
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO bytedown_migrations(version) VALUES($1)`, name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

const imageColumns = `id,job_id,status,format,width,height,bytes,created_at`
const jobColumns = `j.id,j.image_id,j.status,j.generated,j.planned,i.profile,i.pipeline,j.attempt,j.token,j.error_code,j.error_message,j.processing_ms`

func scanImage(row pgx.Row) (Image, error) {
	im := Image{Outputs: []Asset{}}
	err := row.Scan(&im.ID, &im.JobID, &im.Status, &im.Format, &im.Width, &im.Height, &im.Bytes, &im.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return im, err
}

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.ImageID, &j.Status, &j.Generated, &j.Planned, &j.Profile, &j.Pipeline, &j.Attempt, &j.Token, &j.ErrorCode, &j.ErrorMessage, &j.ProcessingMS)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return j, err
}

func (s *Store) Accept(ctx context.Context, image Image, hash, key string, profile json.RawMessage, pipeline string, planned int) (Image, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Image{}, false, err
	}
	defer tx.Rollback(ctx)
	var nullableKey any
	if key != "" {
		nullableKey = key
	}
	inserted, err := scanImage(tx.QueryRow(ctx, `INSERT INTO images(id,job_id,status,format,width,height,bytes,content_hash,idempotency_key,profile,pipeline)
		VALUES($1,$2,'queued',$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(idempotency_key) DO NOTHING RETURNING `+imageColumns,
		image.ID, image.JobID, image.Format, image.Width, image.Height, image.Bytes, hash, nullableKey, profile, pipeline))
	if errors.Is(err, ErrNotFound) {
		var id string
		// JSONB comparison ignores object key order and insignificant whitespace.
		err = tx.QueryRow(ctx, `SELECT id FROM images WHERE idempotency_key=$1 AND content_hash=$2 AND profile=$3::jsonb AND pipeline=$4 FOR SHARE`, key, hash, profile, pipeline).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return Image{}, false, ErrConflict
		}
		if err != nil {
			return Image{}, false, err
		}
		existing, err := getImage(ctx, tx, id)
		if err != nil {
			return Image{}, false, err
		}
		return existing, true, tx.Commit(ctx)
	}
	if err != nil {
		return Image{}, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO jobs(id,image_id,status,planned) VALUES($1,$2,'queued',$3)`, image.JobID, image.ID, planned); err != nil {
		return Image{}, false, err
	}
	return inserted, false, tx.Commit(ctx)
}

func readAssets(ctx context.Context, tx pgx.Tx, id string) ([]Asset, error) {
	rows, err := tx.Query(ctx, `SELECT id,image_id,format,filename,width,height,bytes,duration_ms FROM assets WHERE image_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Asset{}
	for rows.Next() {
		var a Asset
		if err := rows.Scan(&a.ID, &a.ImageID, &a.Format, &a.Filename, &a.Width, &a.Height, &a.Bytes, &a.DurationMS); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func getImage(ctx context.Context, tx pgx.Tx, id string) (Image, error) {
	im, err := scanImage(tx.QueryRow(ctx, `SELECT `+imageColumns+` FROM images WHERE id=$1`, id))
	if err == nil {
		im.Outputs, err = readAssets(ctx, tx, id)
	}
	return im, err
}

func (s *Store) GetImage(ctx context.Context, id string) (Image, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Image{}, err
	}
	defer tx.Rollback(ctx)
	im, err := getImage(ctx, tx, id)
	if err != nil {
		return Image{}, err
	}
	return im, tx.Commit(ctx)
}

// ListImages uses an exclusive ascending ID cursor, independent of ID timestamp encoding.
func (s *Store) ListImages(ctx context.Context, cursor, status string, limit int) ([]Image, error) {
	if limit <= 0 {
		limit = 100
	} else if limit > 101 {
		limit = 101
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT `+imageColumns+` FROM images WHERE id>$1 AND ($2='' OR status=$2) ORDER BY id LIMIT $3`, cursor, status, limit)
	if err != nil {
		return nil, err
	}
	images := []Image{}
	for rows.Next() {
		im, scanErr := scanImage(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		images = append(images, im)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i := range images {
		images[i].Outputs, err = readAssets(ctx, tx, images[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return images, tx.Commit(ctx)
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	return scanJob(s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs j JOIN images i ON i.id=j.image_id WHERE j.id=$1`, id))
}

// GetAsset exposes only assets belonging to a successfully published image.
func (s *Store) GetAsset(ctx context.Context, id string) (Asset, error) {
	var a Asset
	err := s.pool.QueryRow(ctx, `SELECT a.id,a.image_id,a.format,a.filename,a.width,a.height,a.bytes,a.duration_ms
		FROM assets a JOIN images i ON i.id=a.image_id WHERE a.id=$1 AND i.status='succeeded'`, id).
		Scan(&a.ID, &a.ImageID, &a.Format, &a.Filename, &a.Width, &a.Height, &a.Bytes, &a.DurationMS)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return a, err
}

func (s *Store) Claim(ctx context.Context, lease time.Duration) (Job, error) {
	if lease.Microseconds() <= 0 {
		return Job{}, errors.New("lease must be positive")
	}
	for {
		j, exhausted, err := s.claim(ctx, lease)
		if err != nil || !exhausted {
			return j, err
		}
	}
}

func (s *Store) claim(ctx context.Context, lease time.Duration) (Job, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback(ctx)
	j, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs j JOIN images i ON i.id=j.image_id
		WHERE j.status='queued' OR (j.status='processing' AND j.lease_until<=clock_timestamp())
		ORDER BY j.created_at,j.id FOR UPDATE OF j SKIP LOCKED LIMIT 1`))
	if err != nil {
		return Job{}, false, err
	}
	if j.Attempt >= 3 {
		if _, err = tx.Exec(ctx, `UPDATE jobs SET status='failed',token='',lease_until=NULL,error_code='attempts_exhausted',error_message='Processing attempts exhausted' WHERE id=$1`, j.ID); err != nil {
			return Job{}, false, err
		}
		if _, err = tx.Exec(ctx, `UPDATE images SET status='failed' WHERE id=$1`, j.ImageID); err != nil {
			return Job{}, false, err
		}
		return Job{}, true, tx.Commit(ctx)
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return Job{}, false, err
	}
	j.Token = hex.EncodeToString(token)
	if _, err = tx.Exec(ctx, `UPDATE jobs SET status='processing',attempt=attempt+1,token=$2,lease_until=clock_timestamp()+$3::bigint*interval '1 microsecond',generated=0,error_code='',error_message='' WHERE id=$1`, j.ID, j.Token, lease.Microseconds()); err != nil {
		return Job{}, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE images SET status='processing' WHERE id=$1`, j.ImageID); err != nil {
		return Job{}, false, err
	}
	j.Status, j.Generated, j.Attempt, j.ErrorCode, j.ErrorMessage = "processing", 0, j.Attempt+1, "", ""
	return j, false, tx.Commit(ctx)
}

// All mutations lock the job before the image, including deletion.
func owned(ctx context.Context, tx pgx.Tx, id, token string) (string, error) {
	var imageID string
	err := tx.QueryRow(ctx, `SELECT image_id FROM jobs WHERE id=$1 FOR UPDATE`, id).Scan(&imageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrLeaseLost
	}
	if err != nil {
		return "", err
	}
	var exists string
	if err = tx.QueryRow(ctx, `SELECT id FROM images WHERE id=$1 FOR UPDATE`, imageID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrLeaseLost
		}
		return "", err
	}
	var valid bool
	err = tx.QueryRow(ctx, `SELECT status='processing' AND token=$2 AND lease_until>clock_timestamp() FROM jobs WHERE id=$1`, id, token).Scan(&valid)
	if err != nil {
		return "", err
	}
	if !valid {
		return "", ErrLeaseLost
	}
	return imageID, nil
}

func (s *Store) mutate(ctx context.Context, id, token string, f func(pgx.Tx, string) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	imageID, err := owned(ctx, tx, id, token)
	if err != nil {
		return err
	}
	if err = f(tx, imageID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Heartbeat(ctx context.Context, id, token string, lease time.Duration) error {
	if lease.Microseconds() <= 0 {
		return errors.New("lease must be positive")
	}
	return s.mutate(ctx, id, token, func(tx pgx.Tx, _ string) error {
		tag, err := tx.Exec(ctx, `UPDATE jobs SET lease_until=clock_timestamp()+$2::bigint*interval '1 microsecond' WHERE id=$1 AND lease_until>clock_timestamp()`, id, lease.Microseconds())
		return fencedResult(tag.RowsAffected(), err)
	})
}

func (s *Store) Progress(ctx context.Context, id, token string, generated int) error {
	return s.mutate(ctx, id, token, func(tx pgx.Tx, _ string) error {
		tag, err := tx.Exec(ctx, `UPDATE jobs SET generated=$2 WHERE id=$1 AND $2>=generated AND $2<=planned AND lease_until>clock_timestamp()`, id, generated)
		if err == nil && tag.RowsAffected() == 0 {
			var valid bool
			if err = tx.QueryRow(ctx, `SELECT lease_until>clock_timestamp() FROM jobs WHERE id=$1`, id).Scan(&valid); err == nil {
				if valid {
					err = errors.New("invalid progress")
				} else {
					err = ErrLeaseLost
				}
			}
		}
		return err
	})
}

func (s *Store) Complete(ctx context.Context, id, token string, assets []Asset, processingMS int64) error {
	return s.mutate(ctx, id, token, func(tx pgx.Tx, imageID string) error {
		var planned int
		if err := tx.QueryRow(ctx, `SELECT planned FROM jobs WHERE id=$1`, id).Scan(&planned); err != nil {
			return err
		}
		if len(assets) != planned || processingMS < 0 {
			return errors.New("incomplete output manifest")
		}
		for _, a := range assets {
			if a.ImageID != imageID || a.ID == "" || a.Filename == "" || path.IsAbs(a.Filename) || path.Clean(a.Filename) != a.Filename || a.Filename == "." || strings.HasPrefix(a.Filename, "../") || strings.Contains(a.Filename, "\\") {
				return errors.New("invalid asset")
			}
			if _, err := tx.Exec(ctx, `INSERT INTO assets(id,image_id,format,filename,width,height,bytes,duration_ms) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, a.ID, imageID, a.Format, a.Filename, a.Width, a.Height, a.Bytes, a.DurationMS); err != nil {
				return err
			}
		}
		// Check again after inserting the entire manifest: slow writes must not revive an expired attempt.
		tag, err := tx.Exec(ctx, `UPDATE jobs SET status='succeeded',generated=planned,processing_ms=$2,token='',lease_until=NULL WHERE id=$1 AND lease_until>clock_timestamp()`, id, processingMS)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		_, err = tx.Exec(ctx, `UPDATE images SET status='succeeded' WHERE id=$1`, imageID)
		return err
	})
}

func (s *Store) Fail(ctx context.Context, id, token, code, message string, retry bool) error {
	return s.mutate(ctx, id, token, func(tx pgx.Tx, imageID string) error {
		var status string
		err := tx.QueryRow(ctx, `UPDATE jobs SET status=CASE WHEN $2 AND attempt<3 THEN 'queued' ELSE 'failed' END,
			generated=0,token='',lease_until=NULL,error_code=$3,error_message=$4 WHERE id=$1 AND lease_until>clock_timestamp() RETURNING status`, id, retry, code, message).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE images SET status=$2 WHERE id=$1`, imageID, status)
		return err
	})
}

func fencedResult(affected int64, err error) error {
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, id string) (Image, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Image{}, err
	}
	defer tx.Rollback(ctx)
	var jobID string
	if err = tx.QueryRow(ctx, `SELECT id FROM jobs WHERE image_id=$1 FOR UPDATE`, id).Scan(&jobID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return Image{}, err
	}
	im, err := scanImage(tx.QueryRow(ctx, `SELECT `+imageColumns+` FROM images WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return Image{}, err
	}
	im.Outputs, err = readAssets(ctx, tx, id)
	if err != nil {
		return Image{}, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM images WHERE id=$1`, id); err != nil {
		return Image{}, err
	}
	return im, tx.Commit(ctx)
}

// ReferencedPaths returns storage-relative paths for originals and published assets.
// Reconciliation must apply a grace period for files belonging to active attempts.
func (s *Store) ReferencedPaths(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT 'originals/' || id FROM images UNION SELECT filename FROM assets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
