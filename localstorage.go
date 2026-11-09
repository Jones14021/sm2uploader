package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

type LocalStorage struct {
	Printers []*Printer `yaml:"printers"`
	savePath string
}

func defaultKnownHostsPath() (string, error) {
	if runtime.GOOS == "linux" {
		stateDir := os.Getenv("XDG_STATE_HOME")
		if !filepath.IsAbs(stateDir) {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			stateDir = filepath.Join(home, ".local", "state")
		}
		return filepath.Join(stateDir, "sm2uploader", "hosts.yaml"), nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "sm2uploader", "hosts.yaml"), nil
}

func NewLocalStorage(savePath, legacyPath string) (*LocalStorage, error) {
	s := &LocalStorage{
		Printers: []*Printer{},
		savePath: savePath,
	}

	b, err := os.ReadFile(savePath)
	migrating := false
	if os.IsNotExist(err) && legacyPath != "" && legacyPath != savePath {
		b, err = os.ReadFile(legacyPath)
		migrating = err == nil
	}
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read known hosts: %w", err)
	}
	if err := yaml.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("parse known hosts: %w", err)
	}
	if migrating {
		if err := s.Save(); err != nil {
			return nil, fmt.Errorf("migrate known hosts: %w", err)
		}
	}
	return s, nil
}

// Add to add printers to LocalStorage
func (ls *LocalStorage) Add(printers ...*Printer) {
	// Iterate over each printer
	for _, p := range printers {
		// Skip if printer ID is empty
		if p.ID == "" {
			continue
		}
		// Iterate over each printer in LocalStorage
		for idx, x := range ls.Printers {
			// If printer ID matches, update IP and Token if necessary
			if x.ID == p.ID || (x.IP != "" && x.IP == p.IP) {
				if x.ID == x.IP && p.ID != p.IP {
					ls.Printers[idx].ID = p.ID
				}
				if x.IP != p.IP {
					ls.Printers[idx].IP = p.IP
				}
				if p.Model != "" {
					ls.Printers[idx].Model = p.Model
					ls.Printers[idx].Sacp = p.Sacp
				}
				if p.Token != "" && x.Token != p.Token {
					ls.Printers[idx].Token = p.Token
				}
				// Go to exists label
				goto exists
			}
		}
		// Append printer to LocalStorage
		ls.Printers = append(ls.Printers, p)
		// Label for when printer already exists
	exists:
	}
}

func (ls *LocalStorage) Save() error {
	b, err := yaml.Marshal(ls)
	if err != nil {
		return err
	}
	dir := filepath.Dir(ls.savePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".hosts-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(b); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), ls.savePath)
}

func (ls *LocalStorage) Find(host string) *Printer {
	for _, p := range ls.Printers {
		if p.ID == host || p.IP == host {
			return p
		}
	}
	return nil
}
