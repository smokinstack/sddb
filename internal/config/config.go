package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Config holds all user-configurable dashboard settings.
type Config struct {
	AIProvider        string          `json:"ai_provider"`         // "", "claude", "openai", "ollama"
	AutoUpdate        map[string]bool `json:"auto_update"`         // "agentAddr::containerName" → enabled
	NtfyURL           string          `json:"ntfy_url"`            // full topic URL, e.g. https://ntfy.sh/my-alerts
	NtfyDisabled      bool            `json:"ntfy_disabled"`       // master kill-switch; false = enabled
	NtfyDisabledHosts map[string]bool `json:"ntfy_disabled_hosts"` // agentAddr → true means muted
}

// Store is a thread-safe config loader/saver backed by a JSON file.
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

func Load(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "config.json")
	s := &Store{
		path: path,
		cfg:  Config{AutoUpdate: make(map[string]bool)},
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.cfg); err != nil {
		return nil, err
	}
	if s.cfg.AutoUpdate == nil {
		s.cfg.AutoUpdate = make(map[string]bool)
	}
	if s.cfg.NtfyDisabledHosts == nil {
		s.cfg.NtfyDisabledHosts = make(map[string]bool)
	}
	return s, nil
}

func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update calls fn with a pointer to the config, then saves.
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	return s.save()
}

func (s *Store) IsAutoUpdate(agentAddr, containerName string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.AutoUpdate[agentAddr+"::"+containerName]
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}
