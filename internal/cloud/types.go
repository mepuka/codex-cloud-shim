package cloud

import "time"

// ListResponse and Task mirror `codex cloud list --json` exactly as measured
// (docs/probe/list-t010.json; docs/codex-cloud-contract.md "States and
// polling").
type ListResponse struct {
	Tasks  []Task  `json:"tasks"`
	Cursor *string `json:"cursor"`
}

type Task struct {
	ID               string   `json:"id"`
	URL              string   `json:"url"`
	Title            string   `json:"title"`
	Status           string   `json:"status"`
	UpdatedAt        string   `json:"updated_at"`
	EnvironmentID    *string  `json:"environment_id"` // measured null
	EnvironmentLabel string   `json:"environment_label"`
	Summary          *Summary `json:"summary"`
	IsReview         bool     `json:"is_review"`
	AttemptTotal     int      `json:"attempt_total"`
}

type Summary struct {
	FilesChanged int `json:"files_changed"`
	LinesAdded   int `json:"lines_added"`
	LinesRemoved int `json:"lines_removed"`
}

// UpdatedAtTime parses the RFC3339Nano timestamp on demand. Parse failure
// degrades (ok=false) — callers skip stall/recency checks — never a crash
// (design.md §9).
func (t Task) UpdatedAtTime() (time.Time, bool) {
	ts, err := time.Parse(time.RFC3339Nano, t.UpdatedAt)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// State is the shim's conservative mapping of the cloud status vocabulary.
// Only "pending" and "ready" have been measured; everything else maps to
// StateUnknown and callers must report Task.Status verbatim and never guess
// terminality (docs/codex-cloud-contract.md "Open items", design.md C4).
type State string

const (
	StatePending State = "pending"
	StateReady   State = "ready"
	StateUnknown State = "unknown"
)

// MapState maps a verbatim cloud status onto the measured vocabulary.
func MapState(status string) State {
	switch status {
	case "pending":
		return StatePending
	case "ready":
		return StateReady
	default:
		return StateUnknown
	}
}
