package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// UserStore keeps track of which Telegram user IDs (besides the admin) are
// allowed to use the bot. It is backed by a simple JSON file so state
// survives restarts, and is safe for concurrent use.
type UserStore struct {
	mu      sync.RWMutex
	path    string
	adminID int64
	users   map[int64]struct{}
}

type persisted struct {
	Users []int64 `json:"users"`
}

// NewUserStore loads (or creates) the user store at dataDir/authorized_users.json.
func NewUserStore(dataDir string, adminID int64) (*UserStore, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}

	s := &UserStore{
		path:    filepath.Join(dataDir, "authorized_users.json"),
		adminID: adminID,
		users:   make(map[int64]struct{}),
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			// No file yet, that's fine - start empty and persist once.
			if err := s.save(); err != nil {
				return nil, err
			}
			return s, nil
		}
		return nil, fmt.Errorf("reading user store: %w", err)
	}

	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing user store: %w", err)
	}
	for _, id := range p.Users {
		s.users[id] = struct{}{}
	}

	return s, nil
}

// save writes the current state to disk atomically (write to temp file, then rename).
func (s *UserStore) save() error {
	ids := make([]int64, 0, len(s.users))
	for id := range s.users {
		ids = append(ids, id)
	}
	data, err := json.MarshalIndent(persisted{Users: ids}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling user store: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing temp user store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("renaming temp user store: %w", err)
	}
	return nil
}

// IsAuthorized returns true if the given Telegram user id is the admin or
// has been explicitly added by the admin.
func (s *UserStore) IsAuthorized(id int64) bool {
	if id == s.adminID {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.users[id]
	return ok
}

// IsAdmin returns true if the given Telegram user id is the configured admin.
func (s *UserStore) IsAdmin(id int64) bool {
	return id == s.adminID
}

// Add authorizes a new Telegram user id. Returns false if it was already authorized.
func (s *UserStore) Add(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id == s.adminID {
		return false, fmt.Errorf("that is already the admin user")
	}
	if _, ok := s.users[id]; ok {
		return false, nil
	}
	s.users[id] = struct{}{}
	if err := s.save(); err != nil {
		delete(s.users, id)
		return false, err
	}
	return true, nil
}

// Remove revokes access for a Telegram user id. Returns false if it wasn't authorized.
func (s *UserStore) Remove(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.users[id]; !ok {
		return false, nil
	}
	delete(s.users, id)
	if err := s.save(); err != nil {
		s.users[id] = struct{}{}
		return false, err
	}
	return true, nil
}

// List returns all non-admin authorized user ids.
func (s *UserStore) List() []int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]int64, 0, len(s.users))
	for id := range s.users {
		ids = append(ids, id)
	}
	return ids
}
