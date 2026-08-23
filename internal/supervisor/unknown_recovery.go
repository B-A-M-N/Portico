package supervisor

import (
	"context"
)

// UnknownStepObservation is the result of resolving an unknown-outcome step.
type UnknownStepObservation string

const (
	StepAbsent      UnknownStepObservation = "absent"
	StepExactMatch  UnknownStepObservation = "exact_match"
	StepAmbiguous   UnknownStepObservation = "ambiguous"
	StepTransient   UnknownStepObservation = "transient"
)

// UnknownOutcomeResolver resolves the outcome of an interrupted provider
// mutation using provider-specific immutable recovery keys.
type UnknownOutcomeResolver interface {
	ResolveUnknownStep(
		ctx context.Context,
		step StepResolutionRequest,
	) (UnknownStepObservation, error)
}

// StepResolutionRequest carries the context needed to resolve an unknown step.
type StepResolutionRequest struct {
	StepID       string
	StepKind     string
	Provider     string
	AccountID    string
	Parameters   map[string]string
	ConnectionID string
}

// tunnelRecoveryResolver resolves unknown tunnel-create outcomes.
type tunnelRecoveryResolver struct {
	lookup func(ctx context.Context, accountID, name string) (string, error)
}

func (r *tunnelRecoveryResolver) ResolveUnknownStep(
	ctx context.Context,
	req StepResolutionRequest,
) (UnknownStepObservation, error) {
	name := req.Parameters["name"]
	if name == "" {
		return StepAbsent, nil
	}
	tunnelID, err := r.lookup(ctx, req.AccountID, name)
	if err != nil {
		return StepTransient, err
	}
	if tunnelID == "" {
		return StepAbsent, nil
	}
	return StepExactMatch, nil
}
