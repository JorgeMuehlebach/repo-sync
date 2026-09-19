package app

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/JorgeMuehlebach/repo-sync/internal/securefile"
	"github.com/JorgeMuehlebach/repo-sync/internal/strictjson"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"
)

// This is the frozen Context v2 schema from
// llm-config/context-system/schemas/v2/repo-sync-status.schema.json.
//
//go:embed repo-sync-status.schema.json
var contextStatusSchemaData []byte

var (
	contextStatusSchemaOnce sync.Once
	compiledContextStatus   *jsonschema.Schema
	contextStatusSchemaErr  error
)

type statusCompletionExpectation struct {
	RepositoryID     string
	AcceptedCommit   string
	AcceptedTree     string
	CandidateCommit  string
	ValidationConfig contextValidationConfig
	LastValidation   contextValidation
}

func verifyPersistedContextStatus(path string, expectedBytes []byte, expected contextStatus, completion *statusCompletionExpectation) error {
	reopened, data, err := readPersistedContextStatusData(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, expectedBytes) {
		return fmt.Errorf("reopened context status does not match the attempted update")
	}
	if !reflect.DeepEqual(reopened, expected) {
		return fmt.Errorf("reopened context status identity changed")
	}
	if completion != nil {
		if err := verifyStatusCompletion(reopened, *completion); err != nil {
			return err
		}
	}
	return nil
}

func readPersistedContextStatus(path string) (contextStatus, error) {
	report, _, err := readPersistedContextStatusData(path)
	return report, err
}

func readPersistedContextStatusData(path string) (contextStatus, []byte, error) {
	file, err := securefile.OpenRegularBeneath(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return contextStatus{}, nil, fmt.Errorf("reopen context status: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < 0 || info.Size() > maxContextStatusBytes {
		return contextStatus{}, nil, fmt.Errorf("reopened context status exceeds the protocol limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxContextStatusBytes+1))
	if err != nil || len(data) > maxContextStatusBytes {
		return contextStatus{}, nil, fmt.Errorf("read reopened context status")
	}
	if err := validateContextStatusSchema(data); err != nil {
		return contextStatus{}, nil, err
	}
	var reopened contextStatus
	if err := strictjson.Decode(data, &reopened, true); err != nil {
		return contextStatus{}, nil, fmt.Errorf("decode reopened context status: %w", err)
	}
	return reopened, data, nil
}

func validateContextStatusSchema(data []byte) error {
	if err := strictjson.RejectDuplicateKeys(data); err != nil {
		return fmt.Errorf("context status is not strict JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("decode context status for schema validation: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("context status contains trailing JSON")
	}
	contextStatusSchemaOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		compiler.AssertFormat = true
		if err := compiler.AddResource("repo-sync-status.schema.json", bytes.NewReader(contextStatusSchemaData)); err != nil {
			contextStatusSchemaErr = err
			return
		}
		compiledContextStatus, contextStatusSchemaErr = compiler.Compile("repo-sync-status.schema.json")
	})
	if contextStatusSchemaErr != nil || compiledContextStatus == nil {
		return fmt.Errorf("trusted context status schema is unavailable")
	}
	if err := compiledContextStatus.Validate(value); err != nil {
		return fmt.Errorf("context status failed the frozen v2 schema")
	}
	return nil
}

func verifyStatusCompletion(status contextStatus, expected statusCompletionExpectation) error {
	var matched *contextRepository
	for index := range status.Repositories {
		if status.Repositories[index].RepositoryID != expected.RepositoryID {
			continue
		}
		if matched != nil {
			return fmt.Errorf("context status contains duplicate attempted repository")
		}
		matched = &status.Repositories[index]
	}
	if matched == nil || matched.AcceptedCommit != expected.AcceptedCommit || matched.CandidateCommit != expected.CandidateCommit || matched.LastValidation == nil || matched.LastValidation.TreeObjectID != expected.AcceptedTree {
		return fmt.Errorf("context status does not identify the attempted accepted update")
	}
	if !reflect.DeepEqual(matched.ValidationConfig, expected.ValidationConfig) || !reflect.DeepEqual(*matched.LastValidation, expected.LastValidation) {
		return fmt.Errorf("context status does not contain the attempted validation evidence")
	}
	return nil
}
