package webapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Track struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	PartPath   string `json:"partPath"`
	Artwork    string `json:"artwork,omitempty"`
	DurationMS int64  `json:"durationMs"`
}

type Permissions struct {
	CanControl bool `json:"canControl"`
	CanQueue   bool `json:"canQueue"`
}

type Member struct {
	ID          string      `json:"id"`
	Username    string      `json:"username"`
	Host        bool        `json:"host"`
	Permissions Permissions `json:"permissions"`
}

type Session struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	PasswordHash  string            `json:"passwordHash"`
	ShareSecret   string            `json:"shareSecret"`
	Queue         []Track           `json:"queue"`
	CurrentIndex  int               `json:"currentIndex"`
	IsPlaying     bool              `json:"isPlaying"`
	PositionMS    int64             `json:"positionMs"`
	PositionAt    time.Time         `json:"-"`
	StreamVersion int64             `json:"streamVersion"`
	Members       map[string]Member `json:"members"`
	CreatedAt     time.Time         `json:"createdAt"`
}

type persistedState struct {
	Sessions map[string]*Session `json:"sessions"`
}

type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	path     string
}

// snapshot gives HTTP handlers a stable copy after releasing the store lock.
// Playback work can then run without holding a mutex while FFmpeg starts.
func (s *Store) snapshot(sessionID string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session := s.sessions[sessionID]
	if session == nil {
		return nil, false
	}
	return cloneSession(session), true
}

func cloneSession(session *Session) *Session {
	copy := *session
	copy.Queue = append([]Track(nil), session.Queue...)
	copy.Members = make(map[string]Member, len(session.Members))
	for id, member := range session.Members {
		copy.Members[id] = member
	}
	return &copy
}

func NewStore(path string) (*Store, error) {
	s := &Store{sessions: map[string]*Session{}, path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var persisted persistedState
	if err := json.Unmarshal(data, &persisted); err != nil {
		return nil, err
	}
	if persisted.Sessions != nil {
		s.sessions = persisted.Sessions
	}
	for _, session := range s.sessions {
		if session.Members == nil {
			session.Members = map[string]Member{}
		}
		// Never resume audio automatically after a backend restart.
		session.IsPlaying = false
		session.PositionAt = time.Now()
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(persistedState{Sessions: s.sessions}, "", "  ")
	if err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(s.path), ".sessions.tmp")
	if err := os.WriteFile(temporary, data, 0600); err != nil {
		return err
	}
	return os.Rename(temporary, s.path)
}

func id(prefix string) string {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(value)
}

func (s *Session) position(now time.Time) int64 {
	if !s.IsPlaying {
		return s.PositionMS
	}
	return s.PositionMS + now.Sub(s.PositionAt).Milliseconds()
}

func (s *Session) current() (Track, bool) {
	if s.CurrentIndex < 0 || s.CurrentIndex >= len(s.Queue) {
		return Track{}, false
	}
	return s.Queue[s.CurrentIndex], true
}
