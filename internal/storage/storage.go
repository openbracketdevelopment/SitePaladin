package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Store struct{ Dir string }

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("operating system random source unavailable")
	}
	return hex.EncodeToString(b[:])
}

func ValidID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func New(dir string) (*Store, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"originals", "staging", "outputs"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
			return nil, err
		}
	}
	if err := SyncDir(dir); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) Path(relative string) string { return filepath.Join(s.Dir, relative) }

func (s *Store) Ready() error {
	f, err := os.CreateTemp(s.Path("staging"), "ready-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

// SyncDir makes a completed rename durable before publishing database references.
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (s *Store) RemoveOriginal(id string) error {
	if !ValidID(id) {
		return errors.New("invalid storage identifier")
	}
	err := os.Remove(s.Path("originals/" + id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Reconcile only reclaims old files. New uploads and attempts can be between
// filesystem publication and database commit while this snapshot is taken.
func (s *Store) Reconcile(references []string, now time.Time) error {
	keep := make(map[string]bool, len(references))
	for _, path := range references {
		keep[filepath.Clean(path)] = true
	}
	for _, prefix := range []string{"originals", "outputs", "staging"} {
		err := filepath.WalkDir(s.Path(prefix), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(s.Dir, path)
			if err != nil {
				return err
			}
			if strings.HasPrefix(rel, "..") || keep[rel] {
				return nil
			}
			info, err := d.Info()
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if now.Sub(info.ModTime()) < 48*time.Hour {
				return nil
			}
			return os.Remove(path)
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
