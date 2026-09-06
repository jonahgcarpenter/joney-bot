package commands

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	// ErrDuplicateCommand is returned when two handlers register the same command name.
	ErrDuplicateCommand = errors.New("duplicate command")
	// ErrDuplicateAlias is returned when two handlers register the same alias.
	ErrDuplicateAlias = errors.New("duplicate command alias")
)

// Service parses and dispatches slash commands.
type Service struct {
	handlers    map[string]Handler
	resolvers   map[string]FenceTargetResolver
	aliases     map[string]string
	definitions map[string]Definition
}

// NewServiceWithCommands creates a command service from command registrations.
func NewServiceWithCommands(commands ...Command) (*Service, error) {
	service := &Service{
		handlers:    make(map[string]Handler, len(commands)),
		resolvers:   make(map[string]FenceTargetResolver),
		aliases:     make(map[string]string),
		definitions: make(map[string]Definition, len(commands)),
	}

	for _, command := range commands {
		handler := command.Handler
		if handler == nil {
			continue
		}
		definition := normalizeDefinition(handler.Definition())
		if definition.Name == "" {
			return nil, fmt.Errorf("command definition missing name")
		}
		if definition.OutOfBand && definition.UserExclusive {
			return nil, fmt.Errorf("out-of-band command %s cannot be user-exclusive", definition.Name)
		}
		if _, exists := service.handlers[definition.Name]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateCommand, definition.Name)
		}
		if _, exists := service.aliases[definition.Name]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateCommand, definition.Name)
		}
		resolver, resolvesTargets := handler.(FenceTargetResolver)
		if definition.OutOfBand && resolvesTargets {
			return nil, fmt.Errorf("out-of-band command %s cannot resolve fence targets", definition.Name)
		}
		handler = applyMiddleware(handler, command.Middleware)
		service.handlers[definition.Name] = handler
		if resolvesTargets {
			service.resolvers[definition.Name] = resolver
		}
		service.definitions[definition.Name] = definition
		for _, alias := range definition.Aliases {
			if alias == "" {
				continue
			}
			if _, exists := service.handlers[alias]; exists {
				return nil, fmt.Errorf("%w: %s", ErrDuplicateAlias, alias)
			}
			if _, exists := service.aliases[alias]; exists {
				return nil, fmt.Errorf("%w: %s", ErrDuplicateAlias, alias)
			}
			service.aliases[alias] = definition.Name
		}
	}

	return service, nil
}

// Execute parses and runs a command attempt. Unknown attempts return a user-facing result.
func (s *Service) Execute(ctx context.Context, req Request) (Result, error) {
	if !req.Principal.Valid() {
		return Result{}, fmt.Errorf("command request has no valid principal")
	}
	parsed, ok := Parse(req.Raw)
	if !ok || parsed.Name == "" {
		return Result{Text: "Unknown command: /"}, nil
	}

	name := parsed.Name
	canonicalName := name
	if target, ok := s.aliases[name]; ok {
		canonicalName = target
	}
	handler, ok := s.handlers[canonicalName]
	if !ok {
		return Result{Text: "Unknown command: /" + name}, nil
	}

	req.Raw = parsed.Raw
	req.Name = canonicalName
	req.Args = parsed.Args
	req.ArgsText = parsed.ArgsText
	return handler.Execute(ctx, req)
}

// ResolveFenceTargets parses a command request and asks its handler for any
// additional canonical users that must be fenced during execution.
func (s *Service) ResolveFenceTargets(ctx context.Context, req Request) ([]string, error) {
	if !req.Principal.Valid() {
		return nil, fmt.Errorf("command request has no valid principal")
	}
	parsed, ok := Parse(req.Raw)
	if !ok || parsed.Name == "" {
		return nil, nil
	}
	canonicalName := parsed.Name
	if target, ok := s.aliases[canonicalName]; ok {
		canonicalName = target
	}
	resolver := s.resolvers[canonicalName]
	if resolver == nil {
		return nil, nil
	}
	req.Raw = parsed.Raw
	req.Name = canonicalName
	req.Args = parsed.Args
	req.ArgsText = parsed.ArgsText
	return resolver.ResolveFenceTargets(ctx, req)
}

// Definitions returns all registered command definitions.
func (s *Service) Definitions() []Definition {
	definitions := make([]Definition, 0, len(s.definitions))
	for _, definition := range s.definitions {
		definitions = append(definitions, definition)
	}
	sort.Slice(definitions, func(i, j int) bool {
		return definitions[i].Name < definitions[j].Name
	})
	return definitions
}

// Definition returns the registered definition for a command name or alias.
func (s *Service) Definition(name string) (Definition, bool) {
	name = normalizeName(name)
	if target, ok := s.aliases[name]; ok {
		name = target
	}
	definition, ok := s.definitions[name]
	return definition, ok
}

func applyMiddleware(handler Handler, middleware []Middleware) Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		if middleware[i] != nil {
			handler = middleware[i](handler)
		}
	}
	return handler
}

func normalizeDefinition(definition Definition) Definition {
	definition.Name = normalizeName(definition.Name)
	aliases := make([]string, 0, len(definition.Aliases))
	seen := make(map[string]bool, len(definition.Aliases))
	for _, alias := range definition.Aliases {
		alias = normalizeName(alias)
		if alias == "" || alias == definition.Name || seen[alias] {
			continue
		}
		aliases = append(aliases, alias)
		seen[alias] = true
	}
	definition.Aliases = aliases
	definition.Summary = strings.TrimSpace(definition.Summary)
	definition.Usage = strings.TrimSpace(definition.Usage)
	return definition
}

func normalizeName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}
