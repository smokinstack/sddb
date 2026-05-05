package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Credentials holds the stored admin username and bcrypt hash.
type Credentials struct {
	Username string `json:"username"`
	Hash     string `json:"hash"`
}

// SetAdmin writes a new set of admin credentials to path.
func SetAdmin(path, username, password string) error {
	if username == "" {
		return fmt.Errorf("username cannot be empty")
	}
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(Credentials{Username: username, Hash: string(hash)}, "", "  ")
	return os.WriteFile(path, data, 0600)
}

// Load reads credentials from path. Returns nil, nil when the file doesn't
// exist (meaning no auth is configured).
func Load(path string) (*Credentials, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Credentials
	return &c, json.Unmarshal(data, &c)
}

// Verify returns true when username and password match the stored credentials.
func Verify(c *Credentials, username, password string) bool {
	if c == nil || c.Username != username {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(c.Hash), []byte(password)) == nil
}

const (
	sessionTTL    = 24 * time.Hour
	SessionCookie = "sddb_session"
)

// Sessions is an in-memory store of session tokens.
type Sessions struct {
	mu   sync.Mutex
	data map[string]time.Time
}

func NewSessions() *Sessions {
	s := &Sessions{data: make(map[string]time.Time)}
	go s.reapLoop()
	return s
}

func (s *Sessions) Create() string {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	s.mu.Lock()
	s.data[token] = time.Now().Add(sessionTTL)
	s.mu.Unlock()
	return token
}

func (s *Sessions) Valid(token string) bool {
	s.mu.Lock()
	expiry, ok := s.data[token]
	s.mu.Unlock()
	return ok && time.Now().Before(expiry)
}

func (s *Sessions) Delete(token string) {
	s.mu.Lock()
	delete(s.data, token)
	s.mu.Unlock()
}

func (s *Sessions) reapLoop() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		s.mu.Lock()
		for token, expiry := range s.data {
			if now.After(expiry) {
				delete(s.data, token)
			}
		}
		s.mu.Unlock()
	}
}
