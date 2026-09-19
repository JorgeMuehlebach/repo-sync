package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/JorgeMuehlebach/repo-sync/internal/atomicfile"
	"github.com/JorgeMuehlebach/repo-sync/internal/machinelock"
	"github.com/JorgeMuehlebach/repo-sync/internal/strictjson"
)

type Finding struct {
	CheckID string `json:"check_id"`
	Path    string `json:"path,omitempty"`
}

type Failure struct {
	Code       string    `json:"code"`
	Phase      string    `json:"phase"`
	Summary    string    `json:"summary"`
	OccurredAt time.Time `json:"occurred_at"`
	Findings   []Finding `json:"findings,omitempty"`
}

type Validation struct {
	ContractVersion  string            `json:"contract_version,omitempty"`
	ObjectID         string            `json:"object_id,omitempty"`
	Verdict          string            `json:"verdict,omitempty"`
	CheckIDs         []string          `json:"check_ids,omitempty"`
	ValidatorResults []ValidatorResult `json:"validator_results,omitempty"`
	CheckedAt        time.Time         `json:"checked_at,omitempty"`
}

type ValidatorResult struct {
	ContractID            string   `json:"contract_id"`
	ContractVersion       string   `json:"contract_version"`
	Disposition           string   `json:"disposition"`
	CheckIDs              []string `json:"check_ids"`
	SourceID              string   `json:"source_id,omitempty"`
	SourceRole            string   `json:"source_role,omitempty"`
	AcceptedBundleDigest  string   `json:"accepted_bundle_digest,omitempty"`
	CandidateBundleDigest string   `json:"candidate_bundle_digest,omitempty"`
}

// ValidationRuntime is machine-local derived state. None of these paths are
// emitted by the sanitized status protocol.
type ValidationRuntime struct {
	Protocol           string `json:"protocol,omitempty"`
	SourceID           string `json:"source_id,omitempty"`
	ContextctlPath     string `json:"contextctl_path,omitempty"`
	ContextctlDigest   string `json:"contextctl_digest,omitempty"`
	RegistryPath       string `json:"registry_path,omitempty"`
	RegistryRevision   int    `json:"registry_revision,omitempty"`
	RegistryDigest     string `json:"registry_digest,omitempty"`
	TrustStatePath     string `json:"trust_state_path,omitempty"`
	TrustStateRevision int    `json:"trust_state_revision,omitempty"`
	TrustStateDigest   string `json:"trust_state_digest,omitempty"`
}

type Notification struct {
	FailureKey  string    `json:"failure_key,omitempty"`
	Status      string    `json:"status,omitempty"`
	AttemptedAt time.Time `json:"attempted_at,omitempty"`
	Version     uint64    `json:"version,omitempty"`
}

// PendingCompletion is private crash-recovery evidence for a Git side effect
// that has not yet crossed the reopened shared-status completion barrier.
// Ordinary status writers must never expose these identities.
type PendingCompletion struct {
	AcceptedCommit  string     `json:"accepted_commit"`
	AcceptedTree    string     `json:"accepted_tree"`
	CandidateCommit string     `json:"candidate_commit"`
	CandidateTree   string     `json:"candidate_tree"`
	Validation      Validation `json:"validation"`
	CompletedAt     time.Time  `json:"completed_at"`
}

type Repository struct {
	Revision          uint64             `json:"revision,omitempty"`
	ID                string             `json:"id,omitempty"`
	Mode              string             `json:"mode,omitempty"`
	Path              string             `json:"path"`
	LastAttempt       time.Time          `json:"last_attempt,omitempty"`
	LastSuccess       time.Time          `json:"last_success,omitempty"`
	LastSync          time.Time          `json:"last_sync,omitempty"`  // v0.1 compatibility
	LastError         string             `json:"last_error,omitempty"` // v0.1 compatibility; never set by new code
	AcceptedCommit    string             `json:"accepted_commit,omitempty"`
	AcceptedTree      string             `json:"accepted_tree,omitempty"`
	CandidateCommit   string             `json:"candidate_commit,omitempty"`
	CandidateTree     string             `json:"candidate_tree,omitempty"`
	ValidationRuntime ValidationRuntime  `json:"validation_runtime,omitempty"`
	Validation        Validation         `json:"validation,omitempty"`
	PendingCompletion *PendingCompletion `json:"pending_completion,omitempty"`
	Failure           *Failure           `json:"failure,omitempty"`
	Notification      Notification       `json:"notification,omitempty"`
}

type State struct {
	SchemaVersion int                   `json:"schema_version,omitempty"`
	Enabled       bool                  `json:"enabled"`
	Repositories  map[string]Repository `json:"repositories"`
}

// State's schema is private Repo Sync persistence and is intentionally
// independent from the contextctl report, registry, and context-status
// handshake schemas. A context contract bump must not mechanically bump it.
func New() State {
	return State{SchemaVersion: 1, Repositories: make(map[string]Repository)}
}

func Load(path string) (State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return State{}, err
	}
	result := New()
	if err := strictjson.Decode(data, &result, true); err != nil {
		return State{}, fmt.Errorf("parse state %s: %w", path, err)
	}
	if result.Repositories == nil {
		result.Repositories = make(map[string]Repository)
	}
	if result.SchemaVersion != 0 && result.SchemaVersion != 1 {
		return State{}, fmt.Errorf("parse state %s: unsupported schema version %d", path, result.SchemaVersion)
	}
	if result.SchemaVersion == 0 {
		result.SchemaVersion = 1
	}
	return result, nil
}

func Save(path string, value State) error {
	if value.Repositories == nil {
		value.Repositories = make(map[string]Repository)
	}
	if value.SchemaVersion == 0 {
		value.SchemaVersion = 1
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')
	return atomicfile.Write(path, data, 0o600)
}

func Update(path string, update func(*State) error) error {
	release, err := acquire(path + ".lock")
	if err != nil {
		return err
	}
	defer release()
	current, err := Load(path)
	if err != nil {
		return err
	}
	if err := update(&current); err != nil {
		return err
	}
	return Save(path, current)
}

func acquire(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	_, release, err := machinelock.Acquire(path, 5*time.Second, "state")
	return release, err
}
