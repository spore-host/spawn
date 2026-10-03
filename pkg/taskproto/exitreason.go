package taskproto

// ExitReason is a stable, machine-readable token for why spored is ending an
// instance's life. It is passed as $1 to the terminal-flush hook (spawn#632) and
// recorded verbatim as completion.json's terminal_reason.
//
// It is deliberately NOT the human-readable reason string that spored logs and
// puts in user warnings ("TTL expired", "cost limit reached ($2.50)"). Those are
// prose: they are localized, formatted with live values, and reworded freely.
// Deriving a retry class by string-matching them would mean a copy-edit could
// silently change what an adapter decides to retry — the hook maps these tokens
// instead, so the two can be changed independently.
type ExitReason string

const (
	// ExitTTLExpired — the TTL deadline was reached. The reported case in #632:
	// the lifecycle limit fired while the user command was still running.
	ExitTTLExpired ExitReason = "ttl_expired"
	// ExitCostLimitExceeded — the accumulated cost passed spawn:cost-limit.
	ExitCostLimitExceeded ExitReason = "cost_limit_exceeded"
	// ExitIdleTimeout — the idle timer fired. Stops (or hibernates) rather than
	// terminates, so the disk survives; the record still matters because the
	// command was cut short.
	ExitIdleTimeout ExitReason = "idle_timeout"
	// ExitSpotInterruption — a spot reclaim notice arrived. The pre-stop budget
	// here is the ~2-minute spot window, not the usual 5 minutes.
	ExitSpotInterruption ExitReason = "spot_interruption"
	// ExitCompleted — the workload signalled completion and on_complete is acting.
	// The flush hook deliberately does nothing on this path: the wrapper has
	// already written the authoritative record by the time spored sees the signal.
	ExitCompleted ExitReason = "completed"
)

// Note there is no "hibernated" or "stopped" reason, though both are things
// spored does. These tokens name the LIMIT that ended the instance's life, not
// the action taken in response: --hibernate-on-idle reports ExitIdleTimeout
// because the idle timer is what fired, and on_complete=hibernate reports
// ExitCompleted. Mixing the two vocabularies would make terminal_reason
// ambiguous — a reader could not tell "it ran out of time" from "it was asked to
// sleep".
