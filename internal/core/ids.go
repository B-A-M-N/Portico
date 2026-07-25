package core

import "github.com/google/uuid"

// ConnectionID is a unique identifier for a connection profile.
type ConnectionID string

// NewConnectionID creates a new connection ID.
func NewConnectionID() ConnectionID {
	return ConnectionID(uuid.New().String())
}

// ProviderID identifies a provider.
type ProviderID string

// ProviderAccountID identifies a provider account.
type ProviderAccountID string

// OperationID identifies an operation.
type OperationID string

// NewOperationID creates a new operation ID.
func NewOperationID() OperationID {
	return OperationID(uuid.New().String())
}

// PlanID identifies an operation plan.
type PlanID string

// NewPlanID creates a new plan ID.
func NewPlanID() PlanID {
	return PlanID(uuid.New().String())
}
