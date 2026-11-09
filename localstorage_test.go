package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultKnownHostsPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG state paths are Linux-specific")
	}
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", state)
	path, err := defaultKnownHostsPath()
	if err != nil || path != filepath.Join(state, "sm2uploader", "hosts.yaml") {
		t.Fatalf("XDG path = %q, %v", path, err)
	}
	t.Setenv("XDG_STATE_HOME", "relative")
	path, err = defaultKnownHostsPath()
	if err != nil || path != filepath.Join(home, ".local", "state", "sm2uploader", "hosts.yaml") {
		t.Fatalf("fallback path = %q, %v", path, err)
	}
}

func TestKnownHostsMigrationAndDiscovery(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "old", "hosts.yaml")
	if err := os.Mkdir(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("printers:\n  - id: 10.0.2.190\n    ip: 10.0.2.190\n    token: secret\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state", "sm2uploader", "hosts.yaml")
	storage, err := NewLocalStorage(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if printer := storage.Find("10.0.2.190"); printer == nil || printer.Token != "secret" {
		t.Fatalf("legacy token not loaded: %+v", printer)
	}
	storage.Add(&Printer{ID: "A350", IP: "10.0.2.190", Model: "Snapmaker A350"})
	if len(storage.Printers) != 1 || storage.Find("A350").Token != "secret" || storage.Find("A350").Model != "Snapmaker A350" {
		t.Fatalf("discovery did not preserve token: %+v", storage.Printers)
	}
	if err := storage.Save(); err != nil {
		t.Fatal(err)
	}
	storage, err = NewLocalStorage(path, legacy)
	if err != nil || storage.Find("A350") == nil || storage.Find("A350").Token != "secret" {
		t.Fatalf("saved token not reused: %+v, %v", storage, err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy file was removed: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("known-hosts permissions: %v, %v", info, err)
		}
	}
}

func TestKnownHostsProtectExistingData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts.yaml")
	legacy := filepath.Join(dir, "legacy.yaml")
	if err := os.WriteFile(path, []byte("not: [valid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("printers: []"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalStorage(path, legacy); err == nil {
		t.Fatal("accepted malformed known-hosts data")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "not: [valid" {
		t.Fatalf("existing data changed: %q, %v", data, err)
	}
}
