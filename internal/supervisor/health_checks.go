package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/store"
)

// One interpretation of machine health.
//
// There were four. Readiness aggregated provider availability and per-connection
// blockers. Doctor re-derived provider status from the same DTO with its own switch
// over the same availability strings, reaching its own verdicts. Setup rendered
// readiness. Recovery interpreted a failure to connect. Each had its own idea of
// what "ready" meant, and the copy that drifted was whichever one nobody was
// reading that week.
//
// A HealthCheck is one named question with one answer: its state, what it means in
// a sentence, and what to do about it. Doctor prints them; the readiness screen
// shows them; recovery shows the subset that can be answered without a supervisor.
// Adding a check adds it everywhere, which is the point.
//
// Every check here is observational. `portico doctor` is read-only by default and
// these are what it reads, so a check that changed state would make it not read-only
// — remediation stays behind the repair and preview paths that already exist for it.

// checkState is how a check came out.
type checkState string

const (
	// checkOK means the thing checked is as it should be.
	checkOK checkState = "ok"
	// checkAttention means something needs doing, and Portico works meanwhile.
	checkAttention checkState = "attention"
	// checkProblem means something is wrong that will stop work.
	checkProblem checkState = "problem"
	// checkUnknown means the check could not be run. It is not "ok": reporting an
	// unrunnable check as a pass is how a broken machine looks healthy.
	checkUnknown checkState = "unknown"
)

// healthChecks assembles every check the supervisor can answer.
//
// The order is deliberate: the things that stop Portico working come before the
// things that need attention, because a reader stops at the first real problem.
func (h *supervisorHandler) healthChecks(ctx context.Context) []ipc.HealthCheckDTO {
	var checks []ipc.HealthCheckDTO

	checks = append(checks, h.storeChecks(ctx)...)
	checks = append(checks, h.providerChecks(ctx)...)
	checks = append(checks, h.socketCheck())

	return checks
}

// storeChecks are the database, the key that encrypts it, the journal and the
// outstanding cleanup obligations.
func (h *supervisorHandler) storeChecks(ctx context.Context) []ipc.HealthCheckDTO {
	var checks []ipc.HealthCheckDTO
	st := h.sup.store
	if st == nil {
		return []ipc.HealthCheckDTO{{
			ID: "database", Title: "Database", State: string(checkUnknown),
			Summary: "Portico's database is not open, so nothing about stored state can be checked.",
		}}
	}

	// The key and the database have to match. A database encrypted under a key
	// that is no longer present cannot be read, and the symptom — every account
	// failing to decrypt — looks like every credential being wrong at once.
	checks = append(checks, h.keyDatabaseCheck())

	// Obligations Portico recorded and has not discharged. These are resources at
	// a provider that Portico believes should not exist.
	if count, err := st.CountUnresolvedCleanupItems(ctx); err != nil {
		checks = append(checks, ipc.HealthCheckDTO{
			ID: "cleanup", Title: "Provider cleanup", State: string(checkUnknown),
			Summary:   "Portico could not read its record of resources awaiting cleanup.",
			Technical: err.Error(),
		})
	} else if count > 0 {
		checks = append(checks, ipc.HealthCheckDTO{
			ID: "cleanup", Title: "Provider cleanup", State: string(checkAttention),
			Summary: fmt.Sprintf(
				"%d provider resource(s) were recorded as needing removal and Portico has "+
					"no record of removing them.", count),
			Detail: "These may still exist at the provider, where they can cost money and " +
				"hold on to a hostname. This is a history of every obligation recorded, not " +
				"a live queue, so some may already have been removed by hand.",
			NextAction: "Check the provider's own dashboard for resources Portico no longer lists.",
		})
	} else {
		checks = append(checks, ipc.HealthCheckDTO{
			ID: "cleanup", Title: "Provider cleanup", State: string(checkOK),
			Summary: "No provider resources are recorded as awaiting cleanup.",
		})
	}

	// The event journal. A client reconnecting with a cursor older than the oldest
	// retained sequence cannot be replayed to and has to resynchronise.
	count, oldest, newest, err := st.EventJournalHealth(ctx)
	switch {
	case err != nil:
		checks = append(checks, ipc.HealthCheckDTO{
			ID: "events", Title: "Event history", State: string(checkUnknown),
			Summary:   "Portico could not read its event history.",
			Technical: err.Error(),
		})
	case count == 0:
		checks = append(checks, ipc.HealthCheckDTO{
			ID: "events", Title: "Event history", State: string(checkOK),
			Summary: "No events have been recorded yet.",
		})
	default:
		state := checkOK
		summary := fmt.Sprintf("%d event(s) retained, %d through %d.", count, oldest, newest)
		detail := ""
		// Retention trims the journal, so a full one is normal. A journal *over*
		// the limit means trimming has stopped, which is worth knowing before the
		// database grows without bound.
		if limit := store.EventRetentionLimit(); limit > 0 && int(count) > limit {
			state = checkAttention
			detail = fmt.Sprintf(
				"The journal keeps %d events and holds %d, so old events are not being "+
					"trimmed.", limit, count)
		}
		checks = append(checks, ipc.HealthCheckDTO{
			ID: "events", Title: "Event history", State: string(state),
			Summary: summary, Detail: detail,
		})
	}

	return checks
}

// keyDatabaseCheck reports whether the encryption key and the database agree.
func (h *supervisorHandler) keyDatabaseCheck() ipc.HealthCheckDTO {
	check := ipc.HealthCheckDTO{ID: "encryption_key", Title: "Encryption key"}

	dataDir := filepath.Dir(h.sup.paths.DatabasePath)
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		check.State = string(checkUnknown)
		check.Summary = "Portico could not read its data directory to find the encryption key."
		check.Technical = err.Error()
		return check
	}

	keys := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".bin" {
			continue
		}
		if len(name) >= 7 && name[:7] == "portico" {
			keys++
		}
	}

	switch {
	case keys == 0:
		// No key and no accounts is a fresh installation. No key with accounts
		// stored is the mismatch: the credentials cannot be decrypted.
		if h.storedAccountCount() > 0 {
			check.State = string(checkProblem)
			check.Summary = "The key that encrypts saved credentials is missing, so no saved " +
				"account can be used."
			check.Detail = "Portico's database holds accounts encrypted under a key that is no " +
				"longer in its data directory. The accounts cannot be recovered without it."
			check.NextAction = "Restore the key file if you have a backup, or set the affected " +
				"accounts up again."
			return check
		}
		check.State = string(checkOK)
		check.Summary = "No encryption key yet, and nothing needs one."
		return check
	case keys > 1:
		check.State = string(checkAttention)
		check.Summary = fmt.Sprintf(
			"%d encryption keys are present. Portico uses the newest; the others are from "+
				"previous rotations.", keys)
		check.NextAction = "Keep them until every account has been re-saved under the new key."
		return check
	}

	check.State = string(checkOK)
	check.Summary = "The encryption key is present."
	return check
}

// storedAccountCount is how many provider accounts are saved, across providers.
func (h *supervisorHandler) storedAccountCount() int {
	if h.sup.registry == nil {
		return 0
	}
	total := 0
	for _, snap := range h.sup.registry.DiscoverIdentities(context.Background()) {
		total += len(snap.Accounts) + len(snap.PendingAccounts)
	}
	return total
}

// socketCheck reports on the socket clients connect through.
func (h *supervisorHandler) socketCheck() ipc.HealthCheckDTO {
	check := ipc.HealthCheckDTO{ID: "socket", Title: "Connection socket"}

	path := h.sup.paths.SocketPath
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// The supervisor is answering this, so it is running. A missing socket
			// while running means clients cannot reach it at all.
			check.State = string(checkProblem)
			check.Summary = "The socket clients connect through is missing, so nothing can " +
				"reach the supervisor."
			check.NextAction = "Restart the supervisor to recreate it."
			return check
		}
		check.State = string(checkUnknown)
		check.Summary = "Portico could not check its socket."
		check.Technical = err.Error()
		return check
	}

	// The socket must not be readable by other users: anything that can open it
	// can drive every connection Portico manages.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		check.State = string(checkProblem)
		check.Summary = "The socket is accessible to other users on this machine."
		check.Detail = fmt.Sprintf(
			"Anything that can open it can control every connection Portico manages. Its "+
				"permissions are %04o and should be 0600.", mode)
		check.NextAction = "Restart the supervisor, which recreates the socket with the right " +
			"permissions."
		return check
	}

	check.State = string(checkOK)
	check.Summary = "The socket is present and private to this user."
	return check
}

// providerChecks report each provider's position, and the accounts behind it.
//
// This is the interpretation doctor used to make for itself with a switch over
// availability strings. summariseProviderReadiness already answered the same
// question for the readiness screen, so this reads that rather than adding a third
// reading of the same field.
func (h *supervisorHandler) providerChecks(ctx context.Context) []ipc.HealthCheckDTO {
	if h.sup.registry == nil {
		return []ipc.HealthCheckDTO{{
			ID: "providers", Title: "Providers", State: string(checkUnknown),
			Summary: "No provider registry is loaded, so no provider can be checked.",
		}}
	}

	var checks []ipc.HealthCheckDTO
	usable := 0
	snapshots := h.sup.registry.DiscoverIdentities(ctx)

	for _, snap := range snapshots {
		summary, blocked := summariseProviderReadiness(snap, nil)
		if !blocked {
			usable++
		}

		check := ipc.HealthCheckDTO{
			ID:      "provider:" + string(snap.ID),
			Title:   snap.DisplayName,
			Summary: summary,
		}
		switch {
		case !blocked:
			check.State = string(checkOK)
		case snap.Availability == provider.AvailabilityNotImplemented:
			// Not an unhealthy machine: a provider Portico has not built yet.
			check.State = string(checkOK)
		case snap.Availability == provider.AvailabilityDegraded:
			check.State = string(checkProblem)
		default:
			check.State = string(checkAttention)
		}

		if len(snap.SetupActions) > 0 {
			check.NextAction = snap.SetupActions[0]
		}
		// A saved account that was never confirmed is the specific gap worth
		// naming: it looks configured and cannot open a connection.
		if len(snap.PendingAccounts) > 0 {
			check.Detail = fmt.Sprintf(
				"%d saved account(s) have never been confirmed against the provider and "+
					"cannot be used.", len(snap.PendingAccounts))
			if check.State == string(checkOK) {
				check.State = string(checkAttention)
			}
		}
		checks = append(checks, check)
	}

	// The overall verdict comes first, because it is the answer most readers want.
	overall := ipc.HealthCheckDTO{ID: "providers", Title: "Providers"}
	switch {
	case len(snapshots) == 0:
		overall.State = string(checkProblem)
		overall.Summary = "No providers are installed, so no connection can be made."
		overall.NextAction = "Install a provider's client, or configure an account for one."
	case usable == 0:
		overall.State = string(checkAttention)
		overall.Summary = fmt.Sprintf(
			"None of the %d installed provider(s) is ready to use yet.", len(snapshots))
		overall.NextAction = "Set one up from the Providers screen."
	default:
		overall.State = string(checkOK)
		overall.Summary = fmt.Sprintf("%d of %d provider(s) ready to use.", usable, len(snapshots))
	}

	return append([]ipc.HealthCheckDTO{overall}, checks...)
}
