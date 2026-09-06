package screens

import (
	"fmt"
	"strings"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Describing what discovery found.
//
// The discovery list showed an address, a raw confidence word, and nothing else.
// The DTO carried a process name, a framework guess and the Evidence behind the
// classification — what was probed and what answered — and none of it was
// displayed. A user shown "possible" against a port had no way to learn why
// Portico thought that, or whether the guess was worth trusting.
//
// This is one description, used by the Discovery screen and by the wizard's own
// service question. There is no second discovery implementation: both render
// what the supervisor found.

// DiscoveryChoiceLabel names one discovered service in a single line.
//
// When the discovery authority supplies a DisplayName, that leads — it is what
// a person recognises — followed by the address and protocol that make it
// actionable. Without one, the address leads and the process or framework
// follows. The two shapes never repeat the same fact twice: a label like
// "Next.js · 127.0.0.1:3000 over http — Next.js · http" said everything two
// times and fit on no terminal.
func DiscoveryChoiceLabel(svc ipc.DiscoveredServiceDTO) string {
	if svc.DisplayName != "" {
		label := svc.DisplayName + " · " + svc.Address
		if label == "" && svc.Port > 0 {
			label = fmt.Sprintf("port %d", svc.Port)
		}
		if svc.Protocol != "" {
			label += " · " + svc.Protocol
		}
		return label
	}
	label := svc.Address
	if label == "" && svc.Port > 0 {
		label = fmt.Sprintf("port %d", svc.Port)
	}
	if svc.Protocol != "" {
		label += " over " + svc.Protocol
	}
	// The process is what a person recognises. "port 3000" means nothing;
	// "port 3000 — node" is the thing they started.
	if name := serviceName(svc); name != "" {
		label += " — " + name
	}
	return label
}

// serviceName is the most recognisable name for a service: the framework Portico
// identified, or failing that the process running it.
func serviceName(svc ipc.DiscoveredServiceDTO) string {
	if svc.Framework != "" {
		return svc.Framework
	}
	return svc.Process
}

// ConfidenceSentence says how sure Portico is, in words rather than a label.
//
// The grades are the discovery engine's own (internal/discovery): very_likely
// means the probe succeeded and a process was identified; likely means the
// probe succeeded alone; possible means something is listening and Portico
// could not identify it. This mapping is the only translation — a second list
// of grades beside it drifted, and "high" (a grade the engine never emits)
// ended up described as confirmed.
func ConfidenceSentence(confidence string) string {
	switch strings.ToLower(strings.TrimSpace(confidence)) {
	case "very_likely", "confirmed", "certain", "high":
		return "Portico confirmed this by connecting to it."
	case "likely":
		return "Probably right: this matches a service Portico recognises, but it was not confirmed."
	case "possible":
		return "A guess: something is listening, and Portico could not identify what."
	case "":
		return "Portico did not record how sure it is."
	default:
		return "Confidence: " + confidence + "."
	}
}

// DiscoveryEvidenceLines are the details behind one classification.
//
// Evidence is what was probed and what answered. It is the answer to "why does
// Portico think that?", and it was gathered, carried across IPC, and never shown.
func DiscoveryEvidenceLines(svc ipc.DiscoveredServiceDTO) []string {
	var out []string
	out = append(out, ConfidenceSentence(svc.Confidence))

	if svc.Evidence != "" {
		out = append(out, "What Portico saw: "+svc.Evidence)
	} else {
		out = append(out, "Portico recorded no evidence for this one, so the identification "+
			"rests on the port alone.")
	}
	if svc.Process != "" {
		if svc.PID > 0 {
			out = append(out, fmt.Sprintf("Served by %s (process %d).", svc.Process, svc.PID))
		} else {
			out = append(out, "Served by "+svc.Process+".")
		}
	}
	if svc.Framework != "" {
		out = append(out, "It looks like a "+svc.Framework+" application.")
	}
	if svc.Protocol != "" {
		out = append(out, "Portico would connect to it over "+svc.Protocol+".")
	}
	out = append(out, "If this is not what you want, enter an address yourself instead.")
	return out
}

// discoveryChoices turns discovered services into the same choice type every
// other wizard menu uses, so the service question behaves like the rest of the
// wizard and its details expand the same way.
//
// Availability is the discovery authority's verdict, not this package's guess:
// the supervisor marks what cannot be published and says why, and the list
// shows the mark rather than hiding the row.
func discoveryChoices(services []ipc.DiscoveredServiceDTO) []wizardChoice {
	choices := make([]wizardChoice, 0, len(services)+1)
	for _, svc := range services {
		choices = append(choices, wizardChoice{
			Value:     svc.Address,
			Label:     DiscoveryChoiceLabel(svc),
			Available: svc.Selectable,
			Reason:    svc.DisabledReason,
			Detail:    DiscoveryEvidenceLines(svc),
		})
	}
	// Always offered, and always last: a service on a port the scan cannot see
	// is still a service, and finding nothing must not be a dead end.
	choices = append(choices, wizardChoice{
		Value:     manualAddressChoice,
		Label:     "Enter an address manually",
		Available: true,
		Detail: []string{
			"Type the address yourself. Use this when what you want is not listed, " +
				"or when it is not running yet.",
		},
	})
	return choices
}

// manualAddressChoice is the sentinel for the manual-entry option. It is not an
// address, so it cannot collide with one.
const manualAddressChoice = "\x00manual"

// VisibleDiscoveryServices filters a scan result for the default view.
//
// Only a known guess is hidden. A grade this package does not recognise is
// shown, because a filter that silently drops what it cannot classify is how
// a new confidence grade would have made services vanish from the list.
func VisibleDiscoveryServices(services []ipc.DiscoveredServiceDTO, showAll bool) []ipc.DiscoveredServiceDTO {
	if showAll {
		return services
	}
	result := make([]ipc.DiscoveredServiceDTO, 0, len(services))
	for _, s := range services {
		if !strings.EqualFold(strings.TrimSpace(s.Confidence), "possible") {
			result = append(result, s)
		}
	}
	return result
}
