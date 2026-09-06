// Package bootstrap implements first-administrator bootstrap through an
// authenticated Discord, iMessage, or Home Assistant account.
package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

const codeAlphabet = "23456789ABCDEFGH"

var (
	// ErrUnavailable indicates that bootstrap was consumed or an administrator exists.
	ErrUnavailable = errors.New("administrator bootstrap unavailable")
	// ErrInvalidCode indicates that the supplied bootstrap code does not match.
	ErrInvalidCode = errors.New("invalid administrator bootstrap code")
)

// Accounts exposes the account operations needed by administrator bootstrap.
type Accounts interface {
	HasAdmin() (bool, error)
	ClaimBootstrapAdmin(context.Context, identity.Principal) (string, bool, error)
}

// Service owns one process-local, single-use administrator bootstrap code.
type Service struct {
	accounts Accounts
	mu       sync.Mutex
	hash     [sha256.Size]byte
	active   bool
}

// New creates a bootstrap service. A code is returned only when no administrator exists.
func New(accounts Accounts, options ...Option) (*Service, string, error) {
	if accounts == nil {
		return nil, "", fmt.Errorf("bootstrap account store is required")
	}
	configured := optionsConfig{random: rand.Reader}
	for _, option := range options {
		if err := option(&configured); err != nil {
			return nil, "", err
		}
	}
	service := &Service{accounts: accounts}
	hasAdmin, err := accounts.HasAdmin()
	if err != nil {
		return nil, "", fmt.Errorf("check administrator bootstrap availability: %w", err)
	}
	if hasAdmin {
		return service, "", nil
	}
	code, err := generateCode(configured.random)
	if err != nil {
		return nil, "", err
	}
	service.hash = sha256.Sum256([]byte(normalizeCode(code)))
	service.active = true
	return service, code, nil
}

// Definition describes the bootstrap command.
func (s *Service) Definition() commands.Definition {
	return commands.Definition{Name: "bootstrap", Summary: "Claim the first administrator account.", Usage: "/bootstrap <code>", UserExclusive: true}
}

// Execute validates and consumes the bootstrap code for a supported authenticated principal.
func (s *Service) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if !req.Principal.Authenticated() || (req.Principal.Gateway != "discord" && req.Principal.Gateway != "imessage" && req.Principal.Gateway != "homeassistant") {
		return commands.Result{Text: "Bootstrap is available only from an authenticated Discord, iMessage, or Home Assistant account.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "authentication_required"}}, nil
	}
	if len(req.Args) != 1 {
		return commands.Result{Text: commands.UsageText(s.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	userID, err := s.redeem(ctx, req.Principal, req.Args[0])
	switch {
	case err == nil:
		return commands.Result{Text: "Administrator access granted to account " + userID + "."}, nil
	case errors.Is(err, ErrInvalidCode):
		return commands.Result{Text: "That bootstrap code is invalid.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_code"}}, nil
	case errors.Is(err, ErrUnavailable):
		return commands.Result{Text: "Bootstrap is unavailable or has already been completed.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "unavailable"}}, nil
	default:
		return commands.Result{}, err
	}
}

func (s *Service) redeem(ctx context.Context, principal identity.Principal, code string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return "", ErrUnavailable
	}
	candidate := sha256.Sum256([]byte(normalizeCode(code)))
	if subtle.ConstantTimeCompare(candidate[:], s.hash[:]) != 1 {
		return "", ErrInvalidCode
	}
	userID, claimed, err := s.accounts.ClaimBootstrapAdmin(ctx, principal)
	if err != nil {
		return "", err
	}
	if !claimed {
		return "", ErrUnavailable
	}
	s.active = false
	s.hash = [sha256.Size]byte{}
	return userID, nil
}

func normalizeCode(code string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
}

func generateCode(random io.Reader) (string, error) {
	data := make([]byte, 8)
	if _, err := io.ReadFull(random, data); err != nil {
		return "", fmt.Errorf("generate administrator bootstrap code: %w", err)
	}
	characters := make([]byte, 0, 19)
	for i, value := range data {
		if i > 0 && i%2 == 0 {
			characters = append(characters, '-')
		}
		characters = append(characters, codeAlphabet[value>>4], codeAlphabet[value&0x0f])
	}
	return string(characters), nil
}

type optionsConfig struct {
	random io.Reader
}

// Option customizes bootstrap dependencies.
type Option func(*optionsConfig) error

// WithRandom overrides the cryptographically secure random source for tests.
func WithRandom(random io.Reader) Option {
	return func(config *optionsConfig) error {
		if random == nil {
			return fmt.Errorf("bootstrap random source is nil")
		}
		config.random = random
		return nil
	}
}
