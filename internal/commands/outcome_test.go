package commands

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
)

func TestOutcomeClassificationAndCanonicalNames(t *testing.T) {
	failure := errors.New("synthetic storage failure")
	s, err := NewServiceWithCommands(Command{Handler: HandlerFunc{
		DefinitionValue: Definition{Name: "reset", Aliases: []string{"clear"}},
		ExecuteFunc:     func(context.Context, Request) (Result, error) { return Result{}, failure },
	}})
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]string{"clear": "reset", "/reset": "reset", "private-input": "unknown"} {
		if got := s.CanonicalName(input); got != want {
			t.Fatalf("canonical name = %q, want %q", got, want)
		}
	}
	result, err := s.Execute(context.Background(), Request{Principal: testPrincipal("user"), Raw: "/clear"})
	if !errors.Is(err, failure) || result.Outcome.Status != "error" || result.Outcome.IsChanged {
		t.Fatalf("failed outcome = %+v, err=%v", result.Outcome, err)
	}
	result, err = s.Execute(context.Background(), Request{Principal: testPrincipal("user"), Raw: "/private-input"})
	if err != nil || result.Outcome.Status != "rejected" || result.Outcome.ReasonCode != "unknown_command" {
		t.Fatalf("unknown outcome = %+v, err=%v", result.Outcome, err)
	}
	encoded, err := json.Marshal(Result{Text: "response", Outcome: Outcome{Operation: "private-operation", IsChanged: true}})
	if err != nil || strings.Contains(string(encoded), "private-operation") || strings.Contains(string(encoded), "Outcome") {
		t.Fatalf("outcome serialized: %s, err=%v", encoded, err)
	}
}

func TestExpectedAccountRejectionIsNotAnOperationalFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failure  error
		rejected bool
	}{
		{"typed identity change", accounts.ErrPrincipalMismatch, true},
		{"untyped matching text", errors.New(accounts.ErrPrincipalMismatch.Error()), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewServiceWithCommands(Command{Handler: HandlerFunc{
				DefinitionValue: Definition{Name: "stop"},
				ExecuteFunc:     func(context.Context, Request) (Result, error) { return Result{}, tc.failure },
			}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.Execute(context.Background(), Request{Principal: testPrincipal("user"), Raw: "/stop"})
			if tc.rejected {
				if err != nil || result.Outcome.Status != "rejected" || result.Outcome.ReasonCode != "principal_mismatch" || result.Outcome.IsChanged || result.Text != "Your account identity changed. Send the command again." {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if !errors.Is(err, tc.failure) || result.Outcome.Status != "error" {
				t.Fatalf("operational result=%+v err=%v", result, err)
			}
		})
	}
}
