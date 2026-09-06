package accounts

import (
	"errors"
	"fmt"
)

// PolicyError identifies an expected account-policy rejection. Its message is
// user-facing; only ReasonCode is suitable for telemetry.
type PolicyError struct {
	ReasonCode string
	message    string
}

func (e *PolicyError) Error() string { return e.message }

func policyError(reason, format string, args ...any) error {
	return &PolicyError{ReasonCode: reason, message: fmt.Sprintf(format, args...)}
}

// PolicyReason returns a bounded reason for expected rejection, not SQL failure.
func PolicyReason(err error) (string, bool) {
	var policy *PolicyError
	if errors.As(err, &policy) {
		return policy.ReasonCode, true
	}
	for _, entry := range []struct {
		err    error
		reason string
	}{
		{ErrChallengeInvalid, "challenge_invalid"}, {ErrChallengeSameActor, "same_actor"},
		{ErrGatewayConflict, "gateway_conflict"}, {ErrMCPConflict, "mcp_conflict"},
		{ErrLinkBanned, "banned"}, {ErrPrincipalMismatch, "principal_mismatch"},
	} {
		if errors.Is(err, entry.err) {
			return entry.reason, true
		}
	}
	return "", false
}
