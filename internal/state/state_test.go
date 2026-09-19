package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := New()
	want.Enabled = true
	want.Repositories["example/docs"] = Repository{Path: "/work/docs", LastSync: time.Unix(123, 0).UTC()}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Repositories["example/docs"].Path != "/work/docs" {
		t.Fatalf("Load() = %#v", got)
	}
}

func TestConcurrentUpdatesPreserveGlobalAndRepositoryState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Update(path, func(current *State) error {
		current.Enabled = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	const count = 12
	errorsFound := make(chan error, count)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			err := Update(path, func(current *State) error {
				current.Repositories["repo-"+strconv.Itoa(index)] = Repository{Path: "/repo"}
				return nil
			})
			if err != nil {
				errorsFound <- err
			}
		}(i)
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("repository updates overwrote the disabled state")
	}
	if len(got.Repositories) != count {
		t.Fatalf("repository count = %d, want %d", len(got.Repositories), count)
	}
}

func TestLoadLegacyStateAddsSchemaVersionWithoutLosingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := []byte(`{"enabled":true,"repositories":{"owner/repo#main":{"path":"/work/repo","last_error":"old raw error"}}}`)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != 1 || got.Repositories["owner/repo#main"].Path != "/work/repo" {
		t.Fatalf("Load() = %#v", got)
	}
}

func TestSaveLoadStructuredFailureAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Unix(123, 0).UTC()
	want := New()
	want.Repositories["mirror"] = Repository{
		ID:              "mirror",
		Mode:            "mirror",
		Path:            "/work/mirror",
		AcceptedCommit:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AcceptedTree:    "cccccccccccccccccccccccccccccccccccccccc",
		CandidateCommit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		CandidateTree:   "dddddddddddddddddddddddddddddddddddddddd",
		ValidationRuntime: ValidationRuntime{
			Protocol:           "contextctl.report.v2",
			SourceID:           "example.source",
			ContextctlPath:     "/bin/contextctl",
			ContextctlDigest:   "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			RegistryPath:       "/config/registry.json",
			RegistryRevision:   1,
			RegistryDigest:     "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			TrustStatePath:     "/config/trust.json",
			TrustStateRevision: 1,
			TrustStateDigest:   "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		},
		Validation: Validation{
			ContractVersion: "2.0.0",
			ObjectID:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Verdict:         "hold",
			CheckIDs:        []string{"CTX-VALIDATOR-BUNDLE-CHANGED"},
			ValidatorResults: []ValidatorResult{{
				ContractID:            "example.validator",
				ContractVersion:       "1.0.0",
				Disposition:           "hold",
				CheckIDs:              []string{"CTX-VALIDATOR-BUNDLE-CHANGED"},
				CandidateBundleDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}},
			CheckedAt: now,
		},
		Failure:      &Failure{Code: "CTX-VALIDATOR-BUNDLE-CHANGED", Phase: "validation", Summary: "candidate validator bundle requires approval", OccurredAt: now},
		Notification: Notification{FailureKey: "fingerprint", Status: "sent", AttemptedAt: now},
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestLoadRejectsDuplicateUnknownAndFutureSchemaState(t *testing.T) {
	tests := map[string]string{
		"duplicate":                          `{"schema_version":1,"enabled":false,"enabled":true,"repositories":{}}`,
		"unknown":                            `{"schema_version":1,"enabled":false,"repositories":{},"raw_output":"secret"}`,
		"unpublished validator bundle field": `{"schema_version":1,"enabled":false,"repositories":{"mirror":{"path":"/work/mirror","validation":{"validator_results":[{"contract_id":"example.validator","contract_version":"1.0.0","bundle_version":"1.0.0","disposition":"pass","check_ids":["CTX-SCHEMA-PASS"]}]}}}}`,
		"future schema":                      `{"schema_version":2,"enabled":false,"repositories":{}}`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load() accepted ambiguous or incompatible state")
			}
		})
	}
}
