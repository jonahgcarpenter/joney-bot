// Package accounts manages canonical users and their linked gateway accounts.
package accounts

import (
	"crypto/rand"
	"io"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

// Service manages canonical user IDs and linked gateway accounts.
type Service struct {
	path     string
	memories *memory.Store
	log      *config.Logger
	db       *database.DB
	mcp      MCPUserMerger
	now      func() time.Time
	random   io.Reader
	mu       sync.Mutex
	initOnce sync.Once
	initErr  error
}

// NewService constructs the account service; Initialize opens its SQLite database.
func NewService(path string, memories *memory.Store, mcp MCPUserMerger, log *config.Logger) *Service {
	return &Service{path: path, memories: memories, mcp: mcp, log: log, now: time.Now, random: rand.Reader}
}

// Initialize prepares the account-link database.
func (s *Service) Initialize() error {
	s.initOnce.Do(func() {
		s.initErr = s.initialize()
	})
	return s.initErr
}

// Close releases the account-link service's database handle.
func (s *Service) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// UserDataResetCommitted clears runtime state for deleted user-owned MCP configuration.
func (s *Service) UserDataResetCommitted(canonicalUserID string) {
	if s.mcp != nil {
		s.mcp.UserDeleteCommitted(canonicalUserID)
	}
}

func (s *Service) loadLocked() (database.AccountLinkData, error) {
	if err := s.Initialize(); err != nil {
		return database.AccountLinkData{}, err
	}
	return s.db.LoadAccountLinks()
}

func (s *Service) saveLocked(data database.AccountLinkData) error {
	if err := s.Initialize(); err != nil {
		return err
	}
	return s.db.ReplaceAccountLinks(data)
}

func (s *Service) initialize() error {
	db, err := database.Open(s.path, s.log)
	if err != nil {
		return err
	}
	s.db = db
	return nil
}

func (s *Service) ensureInitializedLocked() error {
	return s.Initialize()
}
