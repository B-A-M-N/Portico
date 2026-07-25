package core

import "time"

// EventType describes the type of event
type EventType string

const (
	EventSupervisorReady        EventType = "supervisor.ready"
	EventSnapshotChanged        EventType = "snapshot.changed"
	EventConnectionCreated      EventType = "connection.created"
	EventConnectionUpdated      EventType = "connection.updated"
	EventConnectionStateChanged EventType = "connection.state_changed"
	EventConnectorStarted       EventType = "connector.started"
	EventConnectorExited        EventType = "connector.exited"
	EventOperationCreated       EventType = "operation.created"
	EventOperationStarted       EventType = "operation.started"
	EventOperationStepStarted   EventType = "operation.step_started"
	EventOperationStepSucceeded EventType = "operation.step_succeeded"
	EventOperationStepFailed    EventType = "operation.step_failed"
	EventOperationRolledBack    EventType = "operation.rolled_back"
	EventOperationCompleted     EventType = "operation.completed"
	EventOperationFailed        EventType = "operation.failed"
	EventDiagnosticFinding      EventType = "diagnostic.finding"
	EventDiagnosticResolved     EventType = "diagnostic.resolved"
	EventTrafficSample          EventType = "traffic.sample"
	EventProviderAuthRequired   EventType = "provider.auth_required"
	EventProviderCapsChanged    EventType = "provider.capabilities_changed"
	EventLogLine                EventType = "log.line"
)

// Event represents an event from the supervisor
type Event struct {
	Type      EventType
	Sequence  int64
	Timestamp time.Time
	Data      interface{}
}

// EventStage describes the stage of an operation event
type EventStage string

const (
	StagePending     EventStage = "pending"
	StageStarted     EventStage = "started"
	StageSucceeded   EventStage = "succeeded"
	StageFailed      EventStage = "failed"
	StageCompensated EventStage = "compensated"
)

// OperationEvent represents an operation event
type OperationEvent struct {
	OperationID  OperationID
	ConnectionID ConnectionID
	Stage        EventStage
	StepID       string
	StepKind     StepKind
	Message      string
	Error        string
	Timestamp    time.Time
}

// ConnectionEvent represents a connection event
type ConnectionEvent struct {
	ConnectionID  ConnectionID
	State         RuntimeState
	PreviousState RuntimeState
	Timestamp     time.Time
}

// ConnectorEvent represents a connector event
type ConnectorEvent struct {
	ConnectionID ConnectionID
	PID          int
	Status       ConnectorStatus
	Timestamp    time.Time
}
