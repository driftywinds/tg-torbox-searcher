package bot

import (
	"sync"

	"torbox-tg-bot/internal/torbox"
)

// selectionStage tracks where a particular chosen search result is in its
// lifecycle so replying with the same number again does the right thing.
type selectionStage string

const (
	stageNew         selectionStage = ""
	stageDownloading selectionStage = "downloading" // uncached, waiting for torbox to finish grabbing it
	stageUploading   selectionStage = "uploading"    // cached (or just finished), uploading to gofile
	stageDone        selectionStage = "done"         // gofile link ready
	stageFailed      selectionStage = "failed"
)

type selection struct {
	Result    torbox.SearchResult
	TorrentID int64
	Stage     selectionStage
	GofileURL string
	LastError string
}

// session holds the last search results for a single chat, plus any
// in-progress selections (by 1-based index into Results).
type session struct {
	mu         sync.Mutex
	Query      string
	Results    []torbox.SearchResult
	Selections map[int]*selection
}

func newSession(query string, results []torbox.SearchResult) *session {
	return &session{
		Query:      query,
		Results:    results,
		Selections: make(map[int]*selection),
	}
}

// sessionStore keeps one session per chat id.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[int64]*session
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[int64]*session)}
}

func (s *sessionStore) set(chatID int64, sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[chatID] = sess
}

func (s *sessionStore) get(chatID int64) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[chatID]
}
