package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReconciliationProtectsReferencesAndInflightFiles(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	referenced := "originals/" + ID()
	orphan := "originals/" + ID()
	newFile := "staging/new"
	old := time.Now().Add(-49 * time.Hour)
	for _, name := range []string{referenced, orphan, newFile} {
		if err := os.WriteFile(s.Path(name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		if name != newFile {
			if err := os.Chtimes(s.Path(name), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Reconcile([]string{referenced}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path(orphan)); !os.IsNotExist(err) {
		t.Fatal("orphan was not removed")
	}
	for _, name := range []string{referenced, newFile} {
		if _, err := os.Stat(s.Path(name)); err != nil {
			t.Fatalf("protected file %s: %v", name, err)
		}
	}
}

func TestIdentifiersAndReadiness(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(ID()) || ValidID("../../etc/passwd") || ValidID("a") {
		t.Fatal("identifier validation")
	}
	if err := s.Ready(); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveOriginal("../escape"); err == nil {
		t.Fatal("unsafe original identifier accepted")
	}
}
