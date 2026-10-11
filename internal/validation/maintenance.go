package validation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/JorgeMuehlebach/repo-sync/internal/strictjson"
)

const maintenanceTimeout = 2 * time.Minute

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// MaintenanceRequest supplies only identities pinned by setup. RepositoryPaths
// prevent the trusted runtime snapshot from being staged inside any enrolled
// source checkout.
type MaintenanceRequest struct {
	Runtime         Runtime
	RepositoryPaths []string
	StatusPath      string
}

type MaintenanceReport struct {
	SchemaVersion    int               `json:"schema_version"`
	Protocol         string            `json:"protocol"`
	ContractVersion  string            `json:"contract_version"`
	Command          string            `json:"command"`
	GeneratedAt      string            `json:"generated_at"`
	Verdict          Verdict           `json:"verdict"`
	Checks           []Check           `json:"checks"`
	ValidatorResults []ValidatorResult `json:"validator_results"`
	LinkActions      []LinkAction      `json:"link_actions"`
}

type Maintainer interface {
	PlanManagedSkillLinks(context.Context, MaintenanceRequest) (MaintenanceReport, error)
	ApplyManagedSkillLinks(context.Context, MaintenanceRequest) (MaintenanceReport, error)
}

type MaintenanceRunner struct {
	Executor Executor
	Timeout  time.Duration
}

func (r MaintenanceRunner) PlanManagedSkillLinks(parent context.Context, request MaintenanceRequest) (MaintenanceReport, error) {
	return r.runManagedSkillLinks(parent, request, "plan")
}

func (r MaintenanceRunner) ApplyManagedSkillLinks(parent context.Context, request MaintenanceRequest) (MaintenanceReport, error) {
	return r.runManagedSkillLinks(parent, request, "apply")
}

func (r MaintenanceRunner) runManagedSkillLinks(parent context.Context, request MaintenanceRequest, operation string) (MaintenanceReport, error) {
	if err := validateMaintenanceRequest(request); err != nil {
		return MaintenanceReport{}, err
	}
	prepared, err := prepareRuntimeArtifacts(parent, request.Runtime, request.RepositoryPaths)
	if err != nil {
		return MaintenanceReport{}, err
	}
	defer prepared.close()
	executor := r.Executor
	if executor == nil {
		executor = SystemExecutor{}
	}
	timeout := r.Timeout
	if timeout <= 0 || timeout > maintenanceTimeout {
		timeout = maintenanceTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	stdout := &limitedBuffer{limit: maxOutputBytes}
	stderr := &limitedBuffer{limit: maxOutputBytes}
	args := []string{
		"links", operation, "--add-only", "--format", "json",
		"--registry", prepared.registry.path,
		"--trust-state", prepared.trustState.path,
		"--repo-sync-status", request.StatusPath,
	}
	if err := prepared.verify(ctx); err != nil {
		return MaintenanceReport{}, err
	}
	exitCode, runErr := executor.Run(ctx, prepared.contextctl.path, args, stdout, stderr)
	if errors.Is(stdout.err, errOutputLimit) || errors.Is(stderr.err, errOutputLimit) {
		return MaintenanceReport{}, fmt.Errorf("contextctl output exceeded the protocol limit")
	}
	if ctx.Err() != nil {
		return MaintenanceReport{}, fmt.Errorf("contextctl maintenance timed out")
	}
	if stderr.Len() != 0 {
		return MaintenanceReport{}, fmt.Errorf("contextctl wrote unexpected stderr")
	}
	report, err := decodeMaintenanceReport(stdout.Bytes(), "links-"+operation)
	if err != nil {
		return MaintenanceReport{}, err
	}
	if !validExitVerdict(exitCode, report.Verdict) {
		return MaintenanceReport{}, fmt.Errorf("contextctl exit status and verdict disagree")
	}
	if runErr != nil && exitCode == 0 {
		return MaintenanceReport{}, fmt.Errorf("contextctl execution failed")
	}
	if exitCode == 64 || exitCode == 70 || (exitCode != 0 && exitCode != 20 && exitCode != 21 && exitCode != 22) {
		return MaintenanceReport{}, fmt.Errorf("contextctl integration failed")
	}
	return report, nil
}

func validateMaintenanceRequest(request MaintenanceRequest) error {
	if len(request.RepositoryPaths) == 0 || !filepath.IsAbs(request.StatusPath) {
		return fmt.Errorf("maintenance requires enrolled repositories and an absolute status path")
	}
	for _, path := range request.RepositoryPaths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("maintenance repository paths must be absolute")
		}
	}
	for _, path := range []string{request.Runtime.ContextctlPath, request.Runtime.RegistryPath, request.Runtime.TrustStatePath} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("maintenance runtime paths must be absolute")
		}
	}
	for _, digest := range []string{request.Runtime.ContextctlDigest, request.Runtime.RegistryDigest, request.Runtime.TrustStateDigest} {
		if !digestPattern.MatchString(digest) {
			return fmt.Errorf("maintenance runtime digests are required")
		}
	}
	return nil
}

func decodeMaintenanceReport(data []byte, expectedCommand string) (MaintenanceReport, error) {
	if len(data) == 0 || len(data) > maxOutputBytes || !bytes.Equal(data, bytes.TrimSpace(data)) {
		return MaintenanceReport{}, &ProtocolError{Code: CodeUnsupported}
	}
	var raw map[string]json.RawMessage
	if err := strictjson.Decode(data, &raw, false); err != nil {
		return MaintenanceReport{}, &ProtocolError{Code: CodeUnsupported}
	}
	for _, field := range []string{"schema_version", "protocol", "contract_version", "command", "generated_at", "verdict", "checks", "validator_results", "link_actions"} {
		if _, ok := raw[field]; !ok {
			return MaintenanceReport{}, &ProtocolError{Code: CodeUnsupported}
		}
	}
	if !rawArray(raw["checks"]) || !rawArray(raw["validator_results"]) || !rawArray(raw["link_actions"]) {
		return MaintenanceReport{}, &ProtocolError{Code: CodeUnsupported}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var report MaintenanceReport
	if err := decoder.Decode(&report); err != nil {
		return MaintenanceReport{}, &ProtocolError{Code: CodeUnsupported}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return MaintenanceReport{}, &ProtocolError{Code: CodeUnsupported}
	}
	if err := validateMaintenanceReport(report, expectedCommand); err != nil {
		var protocolError *ProtocolError
		if errors.As(err, &protocolError) {
			return MaintenanceReport{}, err
		}
		return MaintenanceReport{}, &ProtocolError{Code: CodeUnsupported}
	}
	return report, nil
}

func validateMaintenanceReport(report MaintenanceReport, expectedCommand string) error {
	if report.SchemaVersion != ReportSchemaVersion || report.Protocol != ReportProtocol || report.Command != expectedCommand || (expectedCommand != "links-plan" && expectedCommand != "links-apply") || !compatibleContract(report.ContractVersion) {
		return fmt.Errorf("unsupported maintenance report identity")
	}
	if _, err := time.Parse(time.RFC3339, report.GeneratedAt); err != nil {
		return fmt.Errorf("invalid maintenance report time")
	}
	if len(report.Checks) < 1 || len(report.Checks) > 512 || len(report.ValidatorResults) > 64 || len(report.LinkActions) > 1024 {
		return fmt.Errorf("maintenance report collection bounds are invalid")
	}
	checkIDs := make(map[string]bool, len(report.Checks))
	derived := VerdictPass
	for _, check := range report.Checks {
		if len(check.ID) > 100 || !checkIDPattern.MatchString(check.ID) || checkIDs[check.ID] || len(check.Paths) > 128 || !unique(check.Paths) {
			return fmt.Errorf("maintenance check is invalid")
		}
		checkIDs[check.ID] = true
		for _, path := range check.Paths {
			if !safeDiagnosticPath(path) {
				return fmt.Errorf("maintenance diagnostic path is invalid")
			}
		}
		switch check.Status {
		case "pass":
		case "fail":
			derived = VerdictFail
		case "hold":
			if derived != VerdictFail {
				derived = VerdictHold
			}
		case "skip":
			if derived == VerdictPass {
				derived = VerdictIncomplete
			}
		default:
			return fmt.Errorf("maintenance check status is invalid")
		}
	}
	if len(report.ValidatorResults) != 0 || report.Verdict != derived {
		return fmt.Errorf("maintenance verdict is inconsistent")
	}
	if report.Verdict != VerdictPass && len(report.LinkActions) != 0 {
		return &ProtocolError{Code: CodeMaintenanceUnsafe}
	}
	seenLinks := make(map[string]bool, len(report.LinkActions))
	for _, action := range report.LinkActions {
		if action.Action != "create" && action.Action != "noop" {
			return &ProtocolError{Code: CodeMaintenanceUnsafe}
		}
		if action.Ownership != "managed" || action.SkillName == "" || len(action.SkillName) > 80 || !skillNamePattern.MatchString(action.SkillName) || action.SourceID == "" || len(action.SourceID) > 160 || !dottedIDPattern.MatchString(action.SourceID) {
			return fmt.Errorf("maintenance link identity is invalid")
		}
		if len(action.LinkPath) > 1024 || !filepath.IsAbs(action.LinkPath) || strings.ContainsAny(action.LinkPath, "\x00\r\n") || seenLinks[action.LinkPath] {
			return fmt.Errorf("maintenance link path is invalid")
		}
		seenLinks[action.LinkPath] = true
		if action.TargetPath != "" && (len(action.TargetPath) > 1024 || !filepath.IsAbs(action.TargetPath) || strings.ContainsAny(action.TargetPath, "\x00\r\n")) {
			return fmt.Errorf("maintenance target path is invalid")
		}
		if action.TargetPath == "" {
			return fmt.Errorf("maintenance target is missing")
		}
	}
	return nil
}
