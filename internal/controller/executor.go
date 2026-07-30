package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// --------------- operation execution ---------------

// reserveOperation atomically reserves capacity for a new operation.
// It enforces the global concurrency limit and the per-connection
// single-operation constraint under a single lock acquisition.
func (c *Controller) reserveOperation(plan *core.OperationPlan) (*operationRecord, error) {
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if !c.acceptingOps {
		return nil, fmt.Errorf("controller is shutting down and not accepting operations")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// Enforce global concurrency limit
	running := 0
	for _, rec := range c.operations {
		if rec.Snapshot().State == OperationStateRunning {
			running++
		}
	}
	if running >= GlobalOpLimit {
		return nil, fmt.Errorf("operation concurrency limit (%d) reached", GlobalOpLimit)
	}

	// Enforce one mutation per connection
	if rt, ok := c.runtimes[plan.ConnectionID]; ok && rt.ActiveOperation != nil {
		if rec, opOk := c.operations[*rt.ActiveOperation]; opOk {
			if rec.Snapshot().State == OperationStateRunning {
				return nil, fmt.Errorf("connection %s already has an active operation", plan.ConnectionID)
			}
		}
	}

	// Create operation record
	oper := Operation{
		ID:           core.NewOperationID(),
		PlanID:       plan.ID,
		ConnectionID: plan.ConnectionID,
		State:        OperationStateRunning,
		StartedAt:    time.Now().UTC(),
	}
	rec := &operationRecord{oper: oper}
	c.operations[rec.oper.ID] = rec
	// Register the operation before releasing the admission lock. Shutdown
	// first closes admission under the same lock, so Wait cannot miss a
	// just-reserved operation.
	c.operationWG.Add(1)

	// Transition runtime to the appropriate state
	if rt, ok := c.runtimes[plan.ConnectionID]; ok {
		switch plan.Intent {
		case core.IntentOpen:
			rt.State = core.RuntimeOpening
		case core.IntentClose:
			rt.State = core.RuntimeClosing
		case core.IntentRepair:
			rt.State = core.RuntimeRepairing
		case core.IntentDelete:
			rt.State = core.RuntimeClosing
		}
		rt.ActiveOperation = &rec.oper.ID
		rt.LastTransition = time.Now().UTC()
	}

	return rec, nil
}

// completedExecution tracks a successfully completed step and its result
// for potential compensation.
type completedExecution struct {
	Step   core.PlanStep
	Result core.StepResult
}

// execPlan is an internal helper that applies a plan through the provider
// with full precondition checking per SPEC §8.2.
func (c *Controller) execPlan(ctx context.Context, plan *core.OperationPlan, prov core.Provider) (Operation, error) {
	// 0. Validate plan structure (step IDs, duplicate detection, intent consistency).
	if err := plan.Validate(); err != nil {
		return Operation{}, fmt.Errorf("plan validation: %w", err)
	}

	// 1. Confirm the plan has not expired.
	if !plan.ExpiresAt.IsZero() && time.Now().UTC().After(plan.ExpiresAt) {
		return Operation{}, fmt.Errorf("plan %s has expired at %s", plan.ID, plan.ExpiresAt.Format(time.RFC3339))
	}

	// 2. Confirm the profile revision still matches.
	c.mu.RLock()
	profile, profileOk := c.profiles[plan.ConnectionID]
	c.mu.RUnlock()
	if !profileOk {
		return Operation{}, core.ErrProfileNotFound(plan.ConnectionID)
	}
	if plan.ProfileRevision != profile.Revision {
		return Operation{}, fmt.Errorf(
			"profile revision mismatch: plan=%d, current=%d",
			plan.ProfileRevision, profile.Revision,
		)
	}

	// 3. Re-observe the provider state and confirm the observed
	// fingerprint still matches (if the plan was previewed against
	// a specific observed state).
	var observed *core.ObservedConnection
	if planRequiresProvider(plan) {
		var obsErr error
		observed, obsErr = c.Observe(ctx, plan.ConnectionID)
		if obsErr != nil {
			// Non-fatal: warn but proceed. A provider-backed operation still
			// verifies its observed fingerprint below when one was supplied.
			observed = nil
		}
	}

	// 4. Confirm the observed fingerprint still matches.
	if plan.ObservedFingerprint != "" {
		if observed == nil {
			return Operation{}, fmt.Errorf(
				"cannot verify observed fingerprint: provider observation failed",
			)
		}
		currentFP, fpErr := observed.ComputeFingerprint()
		if fpErr != nil {
			return Operation{}, fmt.Errorf("compute observed fingerprint: %w", fpErr)
		}
		if currentFP != plan.ObservedFingerprint {
			err := core.ErrStalePlan(plan.ID, "provider state has changed since preview")
			err.Technical = fmt.Sprintf("expected=%s, got=%s", plan.ObservedFingerprint, currentFP)
			return Operation{}, err
		}
	}

	// 5. Atomically reserve operation capacity (global limit + per-connection).
	rec, err := c.reserveOperation(plan)
	if err != nil {
		return Operation{}, fmt.Errorf("reserve operation: %w", err)
	}
	oper := rec.oper

	// Persist operation record durably. This must succeed before any provider
	// mutation — a journal failure before execution is safety-critical.
	if c.journal != nil {
		startedAt := oper.StartedAt.Format(time.RFC3339)
		if err := c.journal.SaveOperation(ctx, oper.ID, plan.ID, plan.ConnectionID, string(OperationStateRunning), startedAt); err != nil {
			c.operationWG.Done()
			// Clean up the operation and runtime records
			c.mu.Lock()
			delete(c.operations, rec.oper.ID)
			if rt, ok := c.runtimes[plan.ConnectionID]; ok {
				rt.ActiveOperation = nil
				rt.State = core.RuntimeError
				rt.LastTransition = time.Now().UTC()
				rt.Error = &core.PorticoError{
					Code:    "PTO-OP-JOURNAL-FAILED",
					Message: fmt.Sprintf("failed to persist operation: %v", err),
				}
			}
			c.mu.Unlock()
			return Operation{}, fmt.Errorf("journal persistence failed: %w", err)
		}
	}

	// 6. Execute the exact steps through the provider, one by one. Each
	// operation has a supervisor-owned context, detached from the initiating
	// request and cancellable during orderly shutdown.
	operationCtx, cancel := context.WithCancel(context.Background())
	rec.mu.Lock()
	rec.cancel = cancel
	rec.mu.Unlock()
	// The controller owns sequencing, journaling, compensation, and verification.
	// Run in a goroutine so ApplyPlan returns immediately with operation in "running" state.
	go func() {
		defer c.operationWG.Done()
		defer func() {
			rec.mu.Lock()
			rec.cancel = nil
			rec.mu.Unlock()
			cancel()
		}()
		ctx := operationCtx
		completedSteps := []completedExecution{}
		for _, step := range plan.Steps {
			// Handle controller-local steps that don't go to the provider.
			if step.Kind == core.StepFinalizeLocalDeletion {
				// Finalize local deletion is a controller action, not a provider operation.
				// This step succeeds immediately and marks the plan as having at least one step.
				stepResult := core.StepResult{
					StepID:    step.ID,
					Succeeded: true,
					Resources: []core.ProviderResource{},
				}
				if c.stepJournal != nil {
					if err := c.stepJournal.BeginStep(operationCtx, rec.oper.ID, plan.ConnectionID, step); err != nil {
						if failErr := c.failOperation(ctx, rec, plan.ConnectionID, plan.Provider, "PTO-OP-JOURNAL-FAILED", fmt.Errorf("step-start journal failure: %w", err), true); failErr != nil {
							slog.Error("failed to durably record operation failure", "err", failErr)
						}
						return
					}
					c.dispatchCommittedEvents(ctx)
				}

				// Emit step started event.
				if c.stepJournal == nil && c.journal != nil {
					if err := c.journal.AppendOperationEvent(ctx, core.Event{
						Type:      core.EventOperationStepStarted,
						Sequence:  0,
						Timestamp: time.Now().UTC(),
						Data: core.OperationEvent{
							StepID:    step.ID,
							StepKind:  step.Kind,
							Stage:     core.StageStarted,
							Message:   step.Summary,
							Timestamp: time.Now().UTC(),
						},
					}, rec.oper.ID); err != nil {
						slog.Error("failed to persist step started event, stopping execution", "step", step.ID, "err", err)
						rec.Transition(OperationStateFailed, fmt.Errorf("step-start journal failure: %w", err))
						if c.journal != nil {
							_ = c.journal.CompleteOperation(context.Background(), rec.oper.ID, string(OperationStateFailed))
						}
						c.mu.Lock()
						if rt, ok := c.runtimes[plan.ConnectionID]; ok {
							rt.State = core.RuntimeError
							rt.ActiveOperation = nil
							rt.LastTransition = time.Now().UTC()
							rt.Error = &core.PorticoError{
								Code:      "PTO-OP-JOURNAL-FAILED",
								Message:   fmt.Sprintf("Step-start journal failure: %v", err),
								Retryable: true,
								Provider:  plan.Provider,
							}
						}
						c.mu.Unlock()
						return
					}
				}

				// Emit step succeeded event.
				if c.stepJournal != nil {
					if err := c.stepJournal.CommitStepOutcome(ctx, core.StepCommitRequest{
						OperationID: rec.oper.ID, ConnectionID: plan.ConnectionID, Provider: plan.Provider,
						Step: step, Result: stepResult,
					}); err != nil {
						if failErr := c.failOperation(ctx, rec, plan.ConnectionID, plan.Provider, "PTO-OP-PERSIST-FAILED", fmt.Errorf("persist local deletion finalizer: %w", err), true); failErr != nil {
							slog.Error("failed to durably record operation failure", "err", failErr)
						}
						return
					}
					c.dispatchCommittedEvents(ctx)
				} else if c.journal != nil {
					evt := core.Event{
						Type:      core.EventOperationStepSucceeded,
						Sequence:  0,
						Timestamp: time.Now().UTC(),
						Data: core.OperationEvent{
							StepID:    stepResult.StepID,
							StepKind:  step.Kind,
							Stage:     core.StageSucceeded,
							Timestamp: time.Now().UTC(),
						},
					}
					if err := c.journal.AppendOperationEvent(ctx, evt, rec.oper.ID); err != nil {
						slog.Error("failed to persist step event, stopping execution",
							"step", step.ID, "err", err)
						rec.Transition(OperationStateFailed, fmt.Errorf("persistence failure: %w", err))
						if c.journal != nil {
							_ = c.journal.CompleteOperation(context.Background(), rec.oper.ID, string(OperationStateFailed))
						}
						c.mu.Lock()
						if rt, ok := c.runtimes[plan.ConnectionID]; ok {
							rt.State = core.RuntimeError
							rt.ActiveOperation = nil
							rt.LastTransition = time.Now().UTC()
							rt.Error = &core.PorticoError{
								Code:      "PTO-OP-PERSIST-FAILED",
								Message:   fmt.Sprintf("Step event persistence failed: %v", err),
								Retryable: true,
								Provider:  plan.Provider,
							}
						}
						c.mu.Unlock()
						return
					}
				}

				// Record step event in operation.
				stepEvent := StepEvent{
					StepID:    stepResult.StepID,
					Kind:      step.Kind,
					Timestamp: time.Now().UTC(),
					Stage:     core.StageSucceeded,
				}
				rec.AddStepEvent(stepEvent)

				// Continue to next step.
				continue
			}

			// Emit step started event BEFORE provider mutation. The production
			// journal atomically also records the durable recovery-ledger row.
			if c.stepJournal != nil {
				if err := c.stepJournal.BeginStep(ctx, rec.oper.ID, plan.ConnectionID, step); err != nil {
					if failErr := c.failOperation(ctx, rec, plan.ConnectionID, plan.Provider, "PTO-OP-JOURNAL-FAILED", fmt.Errorf("step-start journal failure: %w", err), true); failErr != nil {
						slog.Error("failed to durably record operation failure", "err", failErr)
					}
					return
				}
				c.dispatchCommittedEvents(ctx)
			} else if c.journal != nil {
				if err := c.journal.AppendOperationEvent(ctx, core.Event{
					Type:      core.EventOperationStepStarted,
					Sequence:  0, // will be allocated by store
					Timestamp: time.Now().UTC(),
					Data: core.OperationEvent{
						StepID:    step.ID,
						StepKind:  step.Kind,
						Stage:     core.StageStarted,
						Message:   step.Summary,
						Timestamp: time.Now().UTC(),
					},
				}, rec.oper.ID); err != nil {
					slog.Error("failed to persist step started event, stopping execution", "step", step.ID, "err", err)
					rec.Transition(OperationStateFailed, fmt.Errorf("step-start journal failure: %w", err))
					if c.journal != nil {
						_ = c.journal.CompleteOperation(context.Background(), rec.oper.ID, string(OperationStateFailed))
					}
					c.mu.Lock()
					if rt, ok := c.runtimes[plan.ConnectionID]; ok {
						rt.State = core.RuntimeError
						rt.ActiveOperation = nil
						rt.LastTransition = time.Now().UTC()
						rt.Error = &core.PorticoError{
							Code:      "PTO-OP-JOURNAL-FAILED",
							Message:   fmt.Sprintf("Step-start journal failure: %v", err),
							Retryable: true,
							Provider:  plan.Provider,
						}
					}
					c.mu.Unlock()
					return
				}
			}

			// For delete steps, mark the resource as removal_pending BEFORE
			// calling the provider. This ensures interrupted deletions can be
			// distinguished from not-yet-started ones.
			if isDeleteStep(step.Kind) && step.Technical.ResourceID != "" {
				resType := resourceTypeFromStepKind(step.Kind)
				if c.resourceRemover != nil {
					if err := c.resourceRemover.MarkResourceRemovalPending(ctx, plan.ConnectionID, plan.Provider, resType, step.Technical.ResourceID); err != nil {
						slog.Error("failed to mark resource as removal_pending, stopping execution",
							"resource", step.Technical.ResourceID, "err", err)
						rec.Transition(OperationStateFailed, fmt.Errorf("resource lifecycle update failure: %w", err))
						if c.journal != nil {
							_ = c.journal.CompleteOperation(context.Background(), rec.oper.ID, string(OperationStateFailed))
						}
						c.mu.Lock()
						if rt, ok := c.runtimes[plan.ConnectionID]; ok {
							rt.State = core.RuntimeError
							rt.ActiveOperation = nil
							rt.LastTransition = time.Now().UTC()
							rt.Error = &core.PorticoError{
								Code:      "PTO-OP-LIFECYCLE-UPDATE-FAILED",
								Message:   fmt.Sprintf("Failed to mark resource %s as removal_pending: %v", step.Technical.ResourceID, err),
								Retryable: true,
								Provider:  plan.Provider,
							}
						}
						c.mu.Unlock()
						return
					}
				}
			}

			// Execute the step. Local-origin lifecycle steps are controller-owned;
			// every provider step remains delegated to its selected provider.
			stepResult, execErr := c.executeStep(ctx, plan, prov, step)

			// Persist step result.
			if execErr != nil {
				stepResult.Succeeded = false
				stepResult.Error = execErr
			}
			if stepResult.StepID == "" {
				stepResult.StepID = step.ID
			}

			// Normalize provider results before recording anything.
			// A failed result without an error gets a default error to prevent nil dereference.
			if !stepResult.Succeeded && stepResult.Error == nil {
				stepResult.Error = fmt.Errorf("provider returned unspecified failure for step %s", step.ID)
			}

			// Handle step outcome. The terminal step event, resources,
			// credentials, and lifecycle changes are persisted atomically
			// BEFORE any memory mutation. (SPEC P0 atomic step commit)
			if stepResult.Succeeded {
				// Enforce provider contract invariants at controller boundary
				// BEFORE any persistence or memory mutation.
				for i := range stepResult.Resources {
					res := &stepResult.Resources[i]
					if res.ConnectionID == "" {
						res.ConnectionID = plan.ConnectionID
					}
					if res.ProviderID == "" {
						res.ProviderID = plan.Provider
					}
					var violation error
					switch {
					case res.ConnectionID != plan.ConnectionID:
						violation = fmt.Errorf("provider contract violation: resource %s has connection_id %s, expected %s",
							res.ExternalID, res.ConnectionID, plan.ConnectionID)
					case res.ProviderID != plan.Provider:
						violation = fmt.Errorf("provider contract violation: resource %s has provider_id %s, expected %s",
							res.ExternalID, res.ProviderID, plan.Provider)
					case res.ExternalID == "" || res.Type == "":
						violation = fmt.Errorf("provider contract violation: resource has empty external_id or type")
					}
					if violation != nil {
						slog.Error("provider contract violation", "resource", res.ExternalID, "err", violation)
						if failErr := c.failOperation(ctx, rec, plan.ConnectionID, plan.Provider, "PTO-OP-CONTRACT-VIOLATION",
							fmt.Errorf("provider contract violation: %w", violation), false); failErr != nil {
							slog.Error("failed to durably record contract violation", "err", failErr)
						}
						return
					}
				}

				// Build lifecycle marks for delete steps: the provider confirmed
				// deletion, so mark removed in the same transaction. Policies
				// cascade from Access application deletion.
				var lifecycle []core.LifecycleMark
				var removedApps []string
				if isDeleteStep(step.Kind) && step.Technical.ResourceID != "" {
					lifecycle = append(lifecycle, core.LifecycleMark{
						ProviderID:   plan.Provider,
						ResourceType: resourceTypeFromStepKind(step.Kind),
						ExternalID:   step.Technical.ResourceID,
						NewLifecycle: core.LifecycleRemoved,
					})
					if step.Kind == core.StepDeleteAccessApp {
						removedApps = append(removedApps, step.Technical.ResourceID)
					}
				}
				// Targeted repairs that replace a resource already confirmed
				// absent make that fact durable in the same commit as the new
				// resource. Without this, the stale row would trigger the same
				// repair on every reconciliation cycle after a successful repair.
				lifecycle = append(lifecycle, replacementLifecycleMarks(plan.Provider, step)...)

				// Persist the full step result atomically. Only after the
				// transaction commits may memory be mutated.
				var persistErr error
				request := core.StepCommitRequest{
					OperationID:       rec.oper.ID,
					ConnectionID:      plan.ConnectionID,
					Provider:          plan.Provider,
					Step:              step,
					Result:            stepResult,
					Lifecycle:         lifecycle,
					RemovedAccessApps: removedApps,
				}
				if c.stepJournal != nil {
					persistErr = c.stepJournal.CommitStepOutcome(ctx, request)
				} else if c.stepCommitter != nil {
					persistErr = c.stepCommitter.CommitStepResult(ctx, request)
				} else {
					persistErr = c.persistStepPiecewise(ctx, plan, rec, step, stepResult, lifecycle, removedApps)
				}
				if persistErr != nil {
					// Persistence failure after a successful provider mutation:
					// provider state is now ahead of durable state. Compensate
					// this and all earlier completed steps.
					c.runCompensationForPersistenceFailure(ctx, plan, prov, completedSteps, rec, step, stepResult, persistErr)
					return
				}
				if c.stepJournal != nil {
					c.dispatchCommittedEvents(ctx)
				}

				// Zero credential secret buffers now that they are durably
				// stored; compensation only needs the tunnel ID.
				for i := range stepResult.CredentialMutations {
					for j := range stepResult.CredentialMutations[i].Secret {
						stepResult.CredentialMutations[i].Secret[j] = 0
					}
					stepResult.CredentialMutations[i].Secret = nil
				}

				// Committed — install into memory and record the terminal event.
				completedSteps = append(completedSteps, completedExecution{Step: step, Result: stepResult})
				c.mu.Lock()
				if rt, ok := c.runtimes[plan.ConnectionID]; ok {
					rt.Provider.Resources = applyResourceOutcome(rt.Provider.Resources, stepResult.Resources, lifecycle)
				}
				c.mu.Unlock()
				rec.AddStepEvent(StepEvent{
					StepID:    stepResult.StepID,
					Kind:      step.Kind,
					Stage:     core.StageSucceeded,
					Timestamp: time.Now().UTC(),
				})
			} else {
				// A provider failure is itself a durable step outcome. For deletion
				// this atomically changes removal_pending to removal_failed with the
				// failed event and recovery result.
				var failureLifecycle []core.LifecycleMark
				if isDeleteStep(step.Kind) && step.Technical.ResourceID != "" {
					failureLifecycle = append(failureLifecycle, core.LifecycleMark{
						ProviderID: plan.Provider, ResourceType: resourceTypeFromStepKind(step.Kind),
						ExternalID: step.Technical.ResourceID, NewLifecycle: core.LifecycleRemovalFailed,
					})
				}
				failureRequest := core.StepCommitRequest{OperationID: rec.oper.ID, ConnectionID: plan.ConnectionID,
					Provider: plan.Provider, Step: step, Result: stepResult, Lifecycle: failureLifecycle}
				var outcomeErr error
				if c.stepJournal != nil {
					outcomeErr = c.stepJournal.CommitStepOutcome(ctx, failureRequest)
				} else if c.stepCommitter != nil {
					outcomeErr = c.stepCommitter.CommitStepResult(ctx, failureRequest)
				} else {
					outcomeErr = c.persistStepPiecewise(ctx, plan, rec, step, stepResult, failureLifecycle, nil)
				}
				if outcomeErr != nil {
					if failErr := c.failOperation(ctx, rec, plan.ConnectionID, plan.Provider, "PTO-OP-OUTCOME-JOURNAL-FAILED", fmt.Errorf("failed to persist step outcome: %w", outcomeErr), true); failErr != nil {
						slog.Error("failed to durably record operation failure", "err", failErr)
					}
					return
				}
				if c.stepJournal != nil {
					c.dispatchCommittedEvents(ctx)
				}
				rec.AddStepEvent(StepEvent{
					StepID:    stepResult.StepID,
					Kind:      step.Kind,
					Stage:     core.StageFailed,
					Error:     stepResult.Error.Error(),
					Timestamp: time.Now().UTC(),
				})
				// Execute compensations for completed steps in reverse order.
				slog.Warn("step failed, executing compensations", "step", step.ID, "error", stepResult.Error)
				c.runCompensation(ctx, plan, prov, completedSteps, rec, step, stepResult, false)
				return
			}
		}

		// 7. All steps succeeded — verify outcome before committing terminal state.
		if planRequiresProvider(plan) {
			if err := c.verifyOperationOutcome(ctx, plan.Intent, plan.ConnectionID); err != nil {
				slog.Warn("operation verification failed", "connection", plan.ConnectionID, "intent", plan.Intent, "err", err)
				// Outcome verification failure after successful provider steps.
				// Run safe compensation to avoid leaving an unknown state.
				c.runCompensation(ctx, plan, prov, completedSteps, rec, plan.Steps[len(plan.Steps)-1], core.StepResult{
					Succeeded: false,
					Error:     fmt.Errorf("outcome verification failed: %v", err),
				}, false)
				return
			}
		}

		// 8. Commit terminal success.
		// For delete, the store's CommitDeleteSuccess atomically marks the operation
		// completed and removes all records. For other intents, we update state normally.
		if plan.Intent == core.IntentDelete {
			// Delegate to the store's atomic delete transaction.
			if c.runtimeCommitter != nil {
				if err := c.runtimeCommitter.CommitDeleteSuccess(ctx, plan.ConnectionID, rec.oper.ID); err != nil {
					slog.Warn("delete commit failed", "connection", plan.ConnectionID, "op", rec.oper.ID, "err", err)
					rec.Transition(OperationStateFailed, fmt.Errorf("deletion commit failed: %v", err))
					if c.journal != nil {
						_ = c.journal.CompleteOperation(ctx, rec.oper.ID, string(OperationStateFailed))
					}
					c.mu.Lock()
					if rt, ok := c.runtimes[plan.ConnectionID]; ok {
						rt.State = core.RuntimeOrphaned
						rt.ActiveOperation = nil
						rt.LastTransition = time.Now().UTC()
						rt.Error = &core.PorticoError{
							Code:      "PTO-OP-DELETE-COMMIT-FAILED",
							Message:   fmt.Sprintf("Deletion commit failed: %v", err),
							Retryable: true,
							Segment:   core.SegmentConnector,
							Provider:  plan.Provider,
						}
					}
					c.mu.Unlock()
					return
				}
			}
			rec.Transition(OperationStateCompleted, nil)
			c.dispatchCommittedEvents(ctx)

			// Remove in-memory state after successful store commit.
			c.mu.Lock()
			delete(c.profiles, plan.ConnectionID)
			delete(c.runtimes, plan.ConnectionID)
			c.mu.Unlock()

			// Note: CommitDeleteSuccess already marks the operation completed atomically.
			// No need to call CompleteOperation again.
		} else {
			// Non-delete intents: persist runtime transition BEFORE in-memory
			// transition so that a persistence failure leaves the operation in
			// a failed state rather than reporting success with stale durable state.

			// Build proposed committed state from current state.
			c.mu.RLock()
			profile, hasProfile := c.profiles[plan.ConnectionID]
			_, hasRuntime := c.runtimes[plan.ConnectionID]
			c.mu.RUnlock()

			if !hasProfile || !hasRuntime {
				err := fmt.Errorf("profile or runtime not found for connection %s", plan.ConnectionID)
				rec.Transition(OperationStateFailed, err)
				if c.journal != nil {
					_ = c.journal.CompleteOperation(ctx, rec.oper.ID, string(OperationStateFailed))
				}
				return
			}

			// Compute fallback committed state, used only when no runtime
			// committer is wired (lightweight test wiring).
			newDesired := profile.Desired
			switch plan.Intent {
			case core.IntentOpen:
				newDesired = core.DesiredOpen
			case core.IntentClose:
				newDesired = core.DesiredClosed
			}
			newRuntimeState := core.RuntimeClosed
			switch plan.Intent {
			case core.IntentOpen:
				newRuntimeState = core.RuntimeOpen
			case core.IntentClose:
				newRuntimeState = core.RuntimeClosed
			case core.IntentRepair:
				newRuntimeState = core.RuntimeOpen
			}

			// Persist runtime transition first. If this fails, do not mutate memory.
			var commit *core.RuntimeCommitResult
			var persistErr error
			if c.runtimeCommitter != nil {
				switch plan.Intent {
				case core.IntentOpen:
					commit, persistErr = c.runtimeCommitter.CommitOpenSuccess(ctx, plan.ConnectionID, rec.oper.ID, rec.oper.StartedAt)
				case core.IntentClose:
					commit, persistErr = c.runtimeCommitter.CommitCloseSuccess(ctx, plan.ConnectionID, rec.oper.ID)
				case core.IntentRepair:
					commit, persistErr = c.runtimeCommitter.CommitRepairSuccess(ctx, plan.ConnectionID, rec.oper.ID)
				default:
					persistErr = c.runtimeCommitter.CommitOperationFailure(ctx, plan.ConnectionID, rec.oper.ID, "unknown intent", plan.Provider, false)
				}
			}

			if persistErr != nil {
				slog.Error("failed to persist runtime transition, marking operation failed",
					"connection", plan.ConnectionID, "intent", plan.Intent, "err", persistErr)
				rec.Transition(OperationStateFailed, fmt.Errorf("persistence failure: %w", persistErr))
				if c.journal != nil {
					_ = c.journal.CompleteOperation(ctx, rec.oper.ID, string(OperationStateFailed))
				}
				// Durably represent the failure so the persisted runtime does
				// not remain in a transitional state.
				if c.runtimeCommitter != nil {
					if fErr := c.runtimeCommitter.CommitOperationFailure(ctx, plan.ConnectionID, rec.oper.ID,
						fmt.Sprintf("terminal commit failed: %v", persistErr), plan.Provider, true); fErr != nil {
						slog.Error("failed to durably commit runtime failure",
							"connection", plan.ConnectionID, "err", fErr)
					} else {
						c.dispatchCommittedEvents(ctx)
					}
				}
				c.mu.Lock()
				if rt, ok := c.runtimes[plan.ConnectionID]; ok {
					rt.State = core.RuntimeError
					rt.ActiveOperation = nil
					rt.LastTransition = time.Now().UTC()
					rt.Error = &core.PorticoError{
						Code:      "PTO-OP-PERSIST-FAILED",
						Message:   fmt.Sprintf("Persistence failure: %v", persistErr),
						Retryable: true,
						Provider:  plan.Provider,
					}
				}
				c.mu.Unlock()
				return
			}

			// Persistence succeeded — install the exact committed values into
			// memory. The store transaction owns revision and transition
			// timestamps; memory must mirror them.
			transition := time.Now().UTC()
			if commit != nil {
				newDesired = commit.DesiredState
				newRuntimeState = commit.RuntimeState
				transition = commit.LastTransition
			}
			c.mu.Lock()
			if p, ok := c.profiles[plan.ConnectionID]; ok {
				p.Desired = newDesired
				if commit != nil {
					p.Revision = commit.ProfileRevision
				} else if plan.Intent != core.IntentRepair {
					// Fallback mirrors the store behavior: revision changes
					// only when the desired profile state changes.
					p.Revision++
				}
				c.profiles[plan.ConnectionID] = p
			}

			if rt, ok := c.runtimes[plan.ConnectionID]; ok {
				rt.State = newRuntimeState
				rt.ActiveOperation = nil
				rt.LastTransition = transition
				rt.Error = nil
			}
			c.mu.Unlock()

			// Transition in-memory operation state to completed.
			// Note: The store's CommitOpenSuccess/CommitCloseSuccess/CommitRepairSuccess
			// already marks the operation as completed atomically. We do NOT call
			// CompleteOperation again here to avoid duplicate completion records.
			rec.Transition(OperationStateCompleted, nil)
			c.dispatchCommittedEvents(ctx)
		}
	}()

	return rec.Snapshot(), nil
}

func replacementLifecycleMarks(provider core.ProviderID, step core.PlanStep) []core.LifecycleMark {
	if step.Technical.Parameters == nil {
		return nil
	}
	marks := make([]core.LifecycleMark, 0, 2)
	if appID := step.Technical.Parameters["replaces_access_app_id"]; appID != "" {
		marks = append(marks, core.LifecycleMark{
			ProviderID: provider, ResourceType: core.ResourceAccessApp, ExternalID: appID,
			NewLifecycle: core.LifecycleExternallyRemoved,
		})
	}
	if policyID := step.Technical.Parameters["replaces_access_policy_id"]; policyID != "" {
		marks = append(marks, core.LifecycleMark{
			ProviderID: provider, ResourceType: core.ResourceAccessPolicy, ExternalID: policyID,
			NewLifecycle: core.LifecycleExternallyRemoved,
		})
	}
	return marks
}

// applyResourceOutcome keeps the in-memory runtime inventory aligned with
// the store transaction that has just committed. In particular, replacements
// must retire their exact missing predecessor instead of accumulating stale
// rows that future observation or deletion could act on.
func applyResourceOutcome(existing, additions []core.ProviderResource, lifecycle []core.LifecycleMark) []core.ProviderResource {
	resources := make([]core.ProviderResource, len(existing))
	for i := range existing {
		resources[i] = existing[i].Clone()
	}
	for _, mark := range lifecycle {
		for i := range resources {
			if resources[i].ProviderID == mark.ProviderID && resources[i].Type == mark.ResourceType && resources[i].ExternalID == mark.ExternalID {
				resources[i].Lifecycle = mark.NewLifecycle
			}
		}
	}
	for _, addition := range additions {
		updated := false
		for i := range resources {
			if resources[i].ProviderID == addition.ProviderID && resources[i].Type == addition.Type && resources[i].ExternalID == addition.ExternalID {
				resources[i] = addition.Clone()
				resources[i].Lifecycle = core.LifecyclePresent
				updated = true
				break
			}
		}
		if !updated {
			addition = addition.Clone()
			addition.Lifecycle = core.LifecyclePresent
			resources = append(resources, addition)
		}
	}
	return resources
}

// isDeleteStep returns true if the step kind is a resource deletion step.
// executeStep centralizes the boundary between controller-owned local source
// lifecycle and provider-owned remote lifecycle. Keeping this in the normal
// executor path preserves journaling, persistence, and compensation behavior.
func (c *Controller) executeStep(ctx context.Context, plan *core.OperationPlan, prov core.Provider, step core.PlanStep) (core.StepResult, error) {
	switch step.Kind {
	case core.StepStartOrigin, core.StepStopOrigin:
		c.mu.RLock()
		profile := c.profiles[plan.ConnectionID]
		manager := c.originManager
		c.mu.RUnlock()
		if profile == nil {
			return core.StepResult{}, core.ErrProfileNotFound(plan.ConnectionID)
		}
		if manager == nil {
			return core.StepResult{}, fmt.Errorf("local source manager is unavailable")
		}
		var err error
		if step.Kind == core.StepStartOrigin {
			originURL := step.Technical.Parameters["origin_url"]
			if originURL == "" {
				return core.StepResult{}, fmt.Errorf("start-origin step is missing origin_url")
			}
			err = manager.Start(ctx, plan.ConnectionID, profile.GetSource(), originURL)
		} else {
			err = manager.Stop(ctx, plan.ConnectionID)
		}
		return core.StepResult{StepID: step.ID, Succeeded: err == nil, Error: err}, err
	default:
		if prov == nil {
			return core.StepResult{}, core.ErrProviderNotFound(plan.Provider)
		}
		return prov.ExecuteStep(ctx, plan.ConnectionID, step)
	}
}

func isDeleteStep(kind core.StepKind) bool {
	switch kind {
	case core.StepDeleteTunnel, core.StepDeleteDNSRecord, core.StepDeleteAccessApp, core.StepDeleteAccessPolicy:
		return true
	}
	return false
}

// resourceTypeFromStepKind maps a delete step kind to its resource type.
func resourceTypeFromStepKind(kind core.StepKind) core.ResourceType {
	switch kind {
	case core.StepDeleteTunnel:
		return core.ResourceTunnel
	case core.StepDeleteDNSRecord:
		return core.ResourceDNSRecord
	case core.StepDeleteAccessApp:
		return core.ResourceAccessApp
	case core.StepDeleteAccessPolicy:
		return core.ResourceAccessPolicy
	}
	return ""
}

// markRuntimeError records a runtime error on the controller's in-memory state.
func (c *Controller) markRuntimeError(connID core.ConnectionID, err error, code string, provider core.ProviderID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rt, ok := c.runtimes[connID]; ok {
		rt.State = core.RuntimeError
		rt.ActiveOperation = nil
		rt.LastTransition = time.Now().UTC()
		rt.Error = &core.PorticoError{
			Code:      code,
			Message:   fmt.Sprintf("%v", err),
			Retryable: true,
			Provider:  provider,
		}
	}
}

// runCompensation executes compensations for completed steps in reverse order.
// This is called when a step fails, or (with emergency=true) when persistence
// fails after a successful provider mutation. In emergency mode a
// compensation-start journal failure does not block the remote compensation,
// because leaving live remote resources is worse than an unjournaled rollback.
func (c *Controller) runCompensation(ctx context.Context, plan *core.OperationPlan, prov core.Provider,
	completedSteps []completedExecution, rec *operationRecord, failedStep core.PlanStep, stepResult core.StepResult,
	emergency bool) {

	compensationFailed := false
	for i := len(completedSteps) - 1; i >= 0; i-- {
		executed := completedSteps[i]
		if executed.Step.Compensation == nil {
			continue
		}

		// Materialize compensation with the correct resource IDs from the completed step result.
		// Select resource by type instead of positionally.
		compTech := executed.Step.Compensation.Technical
		if compTech.ResourceID == "" {
			desiredType := resourceTypeFromStepKind(executed.Step.Compensation.Kind)
			compTech.ResourceID = selectResourceByType(executed.Result.Resources, desiredType)
		}
		if compensationRequiresResourceID(executed.Step.Compensation.Kind) && compTech.ResourceID == "" {
			compensationFailed = true
			c.recordCleanupResources(ctx, plan, rec, executed.Result.Resources, "compensation_pending", "exact resource ID could not be materialized for compensation")
			c.markCompensationResourcesFailed(ctx, plan, executed)
			c.appendCleanupFinding(ctx, plan.ConnectionID,
				"Compensation requires an exact resource ID",
				fmt.Sprintf("Operation %s could not materialize compensation %s from step %s; no unambiguous %s resource ID was returned.", rec.oper.ID, executed.Step.Compensation.ID, executed.Step.ID, resourceTypeFromStepKind(executed.Step.Compensation.Kind)))
			continue
		}
		compPlanStep := core.PlanStep{
			ID:        executed.Step.Compensation.ID,
			Kind:      executed.Step.Compensation.Kind,
			Technical: compTech,
		}

		// Journal compensation started - must succeed before remote mutation
		// unless this is the emergency persistence-failure recovery path.
		compensationJournal, hasCompensationJournal := c.stepJournal.(CompensationJournal)
		if hasCompensationJournal {
			if err := compensationJournal.BeginCompensation(ctx, rec.oper.ID, plan.ConnectionID, compPlanStep); err != nil {
				if !emergency {
					slog.Error("failed to persist compensation start - blocking compensation", "step", compPlanStep.ID, "err", err)
					compensationFailed = true
					c.recordCleanupResources(ctx, plan, rec, executed.Result.Resources, "compensation_pending", "could not journal compensation start: "+err.Error())
					continue
				}
				slog.Error("compensation start ledger failed - proceeding emergency", "step", compPlanStep.ID, "err", err)
			}
		} else if c.journal != nil {
			if err := c.journal.AppendOperationEvent(ctx, core.Event{
				Type:      core.EventOperationStepStarted,
				Sequence:  0,
				Timestamp: time.Now().UTC(),
				Data: core.OperationEvent{
					StepID:    compPlanStep.ID,
					StepKind:  compPlanStep.Kind,
					Stage:     core.StageStarted,
					Message:   "Compensating for failed step " + failedStep.ID,
					Timestamp: time.Now().UTC(),
				},
			}, rec.oper.ID); err != nil {
				if !emergency {
					slog.Error("failed to persist compensation started event - blocking compensation",
						"step", compPlanStep.ID, "err", err)
					compensationFailed = true
					c.markCompensationResourcesFailed(ctx, plan, executed)
					// Do NOT execute remote mutation when journal fails.
					continue
				}
				slog.Error("compensation-start journal failed - proceeding (emergency recovery)",
					"step", compPlanStep.ID, "err", err)
			}
		}

		compResult, compErr := c.executeStep(ctx, plan, prov, compPlanStep)
		if compErr != nil && compResult.Error == nil {
			compResult.Error = compErr
		}
		compSucceeded := compErr == nil && compResult.Succeeded

		// If compensation succeeded and the completed step had credential
		// mutations, delete the credential since the resource is being rolled back.
		if compSucceeded {
			for _, cred := range executed.Result.CredentialMutations {
				if c.credentialStorer != nil {
					if delErr := c.credentialStorer.DeleteTunnelCredentialExact(ctx, plan.ConnectionID, plan.Provider, cred.TunnelID); delErr != nil {
						slog.Error("failed to delete credential during compensation - recording",
							"tunnel", cred.TunnelID, "err", delErr)
						// This is NOT best-effort. A stale credential is a retained secret.
						// Mark compensation as failed so it can be retried, and record
						// a cleanup finding for secret-store reconciliation.
						compensationFailed = true
						c.appendCleanupFinding(ctx, plan.ConnectionID,
							"Stale tunnel credential retained after rollback",
							fmt.Sprintf("Credential for tunnel %s could not be deleted during compensation: %v. It must be removed via secret-store reconciliation.", cred.TunnelID, delErr))
					}
				}
			}
			// Mark resource lifecycle as removed on successful compensation.
			if c.resourceRemover != nil && len(executed.Result.Resources) > 0 {
				for _, res := range executed.Result.Resources {
					if rmErr := c.resourceRemover.MarkResourceRemoved(ctx, plan.ConnectionID, plan.Provider, res.Type, res.ExternalID); rmErr != nil {
						slog.Warn("failed to mark resource removed during compensation",
							"resource", res.ExternalID, "err", rmErr)
					}
				}
			}
		} else {
			// Failed compensation leaves the created resources live remotely:
			// mark them removal_failed so cleanup/reconciliation can retry.
			c.markCompensationResourcesFailed(ctx, plan, executed)
			cleanupError := "compensation returned failure"
			if compErr != nil {
				cleanupError = compErr.Error()
			} else if compResult.Error != nil {
				cleanupError = compResult.Error.Error()
			}
			c.recordCleanupResources(ctx, plan, rec, executed.Result.Resources, "compensation_failed", cleanupError)
		}

		// Journal compensation result - treat failure as durable recovery failure.
		if hasCompensationJournal {
			if err := compensationJournal.CommitCompensationOutcome(ctx, rec.oper.ID, plan.ConnectionID, compPlanStep, compResult); err != nil {
				slog.Error("failed to persist compensation outcome", "step", compPlanStep.ID, "err", err)
				compensationFailed = true
				c.recordCleanupResources(ctx, plan, rec, executed.Result.Resources, "outcome_unknown", "compensation executed but outcome could not be journaled: "+err.Error())
			}
		} else if c.journal != nil {
			compOpEvt := core.OperationEvent{
				StepID:    compPlanStep.ID,
				StepKind:  compPlanStep.Kind,
				Stage:     core.StageCompensated,
				Timestamp: time.Now().UTC(),
			}
			compEvtType := core.EventOperationStepSucceeded
			if !compSucceeded {
				compEvtType = core.EventOperationStepFailed
				compOpEvt.Stage = core.StageFailed
				compErrMsg := ""
				if compErr != nil {
					compErrMsg = compErr.Error()
				} else if compResult.Error != nil {
					compErrMsg = compResult.Error.Error()
				} else {
					compErrMsg = "compensation returned failure without error"
				}
				compOpEvt.Error = compErrMsg
			}
			if jErr := c.journal.AppendOperationEvent(ctx, core.Event{
				Type:      compEvtType,
				Sequence:  0,
				Timestamp: time.Now().UTC(),
				Data:      compOpEvt,
			}, rec.oper.ID); jErr != nil {
				slog.Error("failed to persist compensation result - durable recovery failure",
					"step", compPlanStep.ID, "err", jErr)
				compensationFailed = true
				// Preserve the exact external IDs as an unresolved cleanup record.
				ids := make([]string, 0, len(executed.Result.Resources))
				for _, res := range executed.Result.Resources {
					ids = append(ids, fmt.Sprintf("%s/%s", res.Type, res.ExternalID))
				}
				c.appendCleanupFinding(ctx, plan.ConnectionID,
					"Compensation result could not be journaled",
					fmt.Sprintf("Compensation for step %s executed but its result could not be persisted (%v). Affected resources: %s", compPlanStep.ID, jErr, strings.Join(ids, ", ")))
			}
		}

		if !compSucceeded {
			compensationFailed = true
			slog.Error("compensation failed",
				"step", executed.Step.Compensation.ID,
				"error", compErr,
				"stepResult", compResult.Error)
		}
	}

	if compensationFailed {
		slog.Error("operation partially rolled back due to compensation failures",
			"connection", plan.ConnectionID, "operation", rec.oper.ID)
	}

	rec.Transition(OperationStateFailed, stepResult.Error)
	c.markRuntimeError(plan.ConnectionID, stepResult.Error, "PTO-OP-PROVIDER-STEP", plan.Provider)

	if c.journal != nil {
		if err := c.journal.CompleteOperation(context.Background(), rec.oper.ID, string(OperationStateFailed)); err != nil {
			slog.Error("failed to persist operation completion after compensation",
				"operation", rec.oper.ID, "err", err)
		}
	}
	if c.runtimeCommitter != nil {
		if err := c.runtimeCommitter.CommitOperationFailure(ctx, plan.ConnectionID, rec.oper.ID, stepResult.Error.Error(), plan.Provider, true); err != nil {
			slog.Error("failed to commit durable runtime failure after compensation",
				"operation", rec.oper.ID, "err", err)
		} else {
			c.dispatchCommittedEvents(ctx)
		}
	}
}

// markCompensationResourcesFailed marks the resources of a completed step as
// removal_failed when their compensation could not be executed or failed,
// preserving them for retry or manual cleanup.
func (c *Controller) markCompensationResourcesFailed(ctx context.Context, plan *core.OperationPlan, executed completedExecution) {
	if c.resourceRemover == nil {
		return
	}
	for _, res := range executed.Result.Resources {
		if err := c.resourceRemover.MarkResourceRemovalFailed(ctx, plan.ConnectionID, plan.Provider, res.Type, res.ExternalID); err != nil {
			slog.Warn("failed to mark resource removal_failed after failed compensation",
				"resource", res.ExternalID, "err", err)
		}
	}
}

func (c *Controller) recordCleanupResources(ctx context.Context, plan *core.OperationPlan, rec *operationRecord, resources []core.ProviderResource, state, lastError string) {
	if c.cleanupRecorder == nil {
		return
	}
	for _, resource := range resources {
		if resource.Type == "" || resource.ExternalID == "" {
			continue
		}
		if err := c.cleanupRecorder.RecordCleanupItem(ctx, rec.oper.ID, plan.ConnectionID, plan.Provider, resource.Type, resource.ExternalID, state, lastError); err != nil {
			slog.Error("record cleanup item", "resource", resource.ExternalID, "state", state, "err", err)
		}
	}
}

// appendCleanupFinding records an actionable diagnostic finding for manual
// or automatic cleanup. Journal failures are logged, never fatal.
func (c *Controller) appendCleanupFinding(ctx context.Context, connID core.ConnectionID, summary, explanation string) {
	if c.journal == nil {
		return
	}
	finding := core.DiagnosticFinding{
		ID:           core.FindingID(fmt.Sprintf("finding-%d", time.Now().UnixNano())),
		ConnectionID: connID,
		Severity:     core.SeverityError,
		Summary:      summary,
		Explanation:  explanation,
		ObservedAt:   time.Now().UTC(),
	}
	if err := c.journal.AppendFinding(ctx, finding); err != nil {
		slog.Error("failed to append cleanup finding", "connection", connID, "err", err)
	}
}

// failOperation transitions the operation and runtime to a failed state.
func (c *Controller) failOperation(ctx context.Context, rec *operationRecord, connID core.ConnectionID, provider core.ProviderID,
	code string, cause error, retryable bool) error {
	// The durable transition is authoritative. Do not show an in-memory error
	// state when persistence failed as well; startup recovery must be allowed to
	// classify that uncertain operation from its last committed step.
	if c.runtimeCommitter != nil {
		if err := c.runtimeCommitter.CommitOperationFailure(ctx, connID, rec.oper.ID, cause.Error(), provider, retryable); err != nil {
			return fmt.Errorf("commit operation failure: %w", err)
		}
	} else if c.journal != nil {
		if err := c.journal.CompleteOperation(ctx, rec.oper.ID, string(OperationStateFailed)); err != nil {
			return fmt.Errorf("complete operation: %w", err)
		}
	}
	rec.Transition(OperationStateFailed, cause)
	c.mu.Lock()
	if rt, ok := c.runtimes[connID]; ok {
		rt.State = core.RuntimeError
		rt.ActiveOperation = nil
		rt.LastTransition = time.Now().UTC()
		rt.Error = &core.PorticoError{
			Code:      code,
			Message:   cause.Error(),
			Retryable: retryable,
			Provider:  provider,
		}
	}
	c.mu.Unlock()
	c.dispatchCommittedEvents(ctx)
	return nil
}

// persistStepPiecewise persists a successful step result using individual
// store operations. Used only when no atomic step committer is wired
// (e.g. lightweight test wiring). Semantics mirror CommitStepResult.
func (c *Controller) persistStepPiecewise(ctx context.Context, plan *core.OperationPlan, rec *operationRecord,
	step core.PlanStep, stepResult core.StepResult, lifecycle []core.LifecycleMark, removedApps []string) error {

	if c.journal != nil {
		evt := core.Event{
			Type:      core.EventOperationStepSucceeded,
			Sequence:  0,
			Timestamp: time.Now().UTC(),
			Data: core.OperationEvent{
				StepID:    stepResult.StepID,
				StepKind:  step.Kind,
				Stage:     core.StageSucceeded,
				Timestamp: time.Now().UTC(),
			},
		}
		if err := c.journal.AppendOperationEvent(ctx, evt, rec.oper.ID); err != nil {
			return fmt.Errorf("persist step event: %w", err)
		}
	}
	if c.resourceSaver != nil {
		for i := range stepResult.Resources {
			if err := c.resourceSaver.SaveResource(ctx, &stepResult.Resources[i]); err != nil {
				return fmt.Errorf("persist resource %s: %w", stepResult.Resources[i].ExternalID, err)
			}
		}
	}
	if c.credentialStorer != nil {
		for _, cred := range stepResult.CredentialMutations {
			if err := c.credentialStorer.SaveTunnelCredential(ctx, plan.ConnectionID, plan.Provider, cred.TunnelID, cred.Secret); err != nil {
				return fmt.Errorf("persist credential for tunnel %s: %w", cred.TunnelID, err)
			}
		}
	}
	if c.resourceRemover != nil {
		for _, mark := range lifecycle {
			if err := c.resourceRemover.MarkResourceRemoved(ctx, plan.ConnectionID, mark.ProviderID, mark.ResourceType, mark.ExternalID); err != nil {
				return fmt.Errorf("mark resource removed %s: %w", mark.ExternalID, err)
			}
		}
		for _, appID := range removedApps {
			if err := c.resourceRemover.MarkAssociatedPoliciesRemoved(ctx, plan.ConnectionID, plan.Provider, appID); err != nil {
				slog.Warn("failed to mark associated policies removed", "app", appID, "err", err)
			}
		}
	}
	return nil
}

// runCompensationForPersistenceFailure is called when persistence fails after
// a successful provider mutation. It runs compensation and marks the operation
// as recovery-required.
func (c *Controller) runCompensationForPersistenceFailure(ctx context.Context, plan *core.OperationPlan,
	prov core.Provider, completedSteps []completedExecution, rec *operationRecord,
	currentStep core.PlanStep, currentResult core.StepResult, persistErr error) {

	slog.Error("persistence failed after provider mutation - running compensation",
		"step", currentStep.ID, "error", persistErr)
	c.recordCleanupResources(ctx, plan, rec, currentResult.Resources, "outcome_unknown", "provider mutation succeeded but normal result persistence failed: "+persistErr.Error())

	// Include the current step in compensation since the provider mutation succeeded.
	// Emergency mode: the journal may be the failing component, so a
	// compensation-start journal failure must not leave live remote resources.
	stepsToCompensate := append(completedSteps, completedExecution{Step: currentStep, Result: currentResult})
	c.runCompensation(ctx, plan, prov, stepsToCompensate, rec,
		currentStep, core.StepResult{
			StepID:              currentResult.StepID,
			Succeeded:           false,
			Error:               fmt.Errorf("persistence failure after mutation: %w", persistErr),
			Resources:           currentResult.Resources,
			CredentialMutations: currentResult.CredentialMutations,
		}, true)

	// Record recovery evidence with the exact external IDs so manual or
	// automatic recovery can locate the provider-side state.
	ids := make([]string, 0, len(currentResult.Resources))
	for _, res := range currentResult.Resources {
		ids = append(ids, fmt.Sprintf("%s/%s", res.Type, res.ExternalID))
	}
	c.appendCleanupFinding(ctx, plan.ConnectionID,
		"Operation requires recovery: persistence failed after provider mutation",
		fmt.Sprintf("Step %s mutated provider state but its result could not be persisted (%v). Affected resources: %s", currentStep.ID, persistErr, strings.Join(ids, ", ")))

	// Mark the operation as recovery-required with the exact external IDs.
	// This preserves the evidence needed for manual or automatic recovery.
	if c.journal != nil {
		_ = c.journal.AppendOperationEvent(ctx, core.Event{
			Type:      core.EventOperationStepFailed,
			Sequence:  0,
			Timestamp: time.Now().UTC(),
			Data: core.OperationEvent{
				StepID:    currentStep.ID,
				StepKind:  currentStep.Kind,
				Stage:     core.StageFailed,
				Error:     fmt.Sprintf("persistence failure after mutation: %v", persistErr),
				Timestamp: time.Now().UTC(),
			},
		}, rec.oper.ID)
	}
}

// selectResourceByType finds a resource matching the given type.
// Returns empty string if no match or multiple ambiguous matches exist.
func selectResourceByType(resources []core.ProviderResource, desiredType core.ResourceType) string {
	var found string
	for _, r := range resources {
		if r.Type == desiredType {
			if found != "" {
				// Multiple matches - ambiguous, fail closed
				return ""
			}
			found = r.ExternalID
		}
	}
	return found
}

func compensationRequiresResourceID(kind core.StepKind) bool {
	switch kind {
	case core.StepDeleteTunnel, core.StepDeleteDNSRecord, core.StepDeleteAccessApp,
		core.StepDeleteAccessPolicy, core.StepUpdateDNSRecord, core.StepUpdateAccessPolicy:
		return true
	}
	return false
}
