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
	"sort"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	ModePublish Mode = "publish"
	ModeMirror  Mode = "mirror"
)

type Verdict string

const (
	VerdictPass       Verdict = "pass"
	VerdictFail       Verdict = "fail"
	VerdictHold       Verdict = "hold"
	VerdictIncomplete Verdict = "incomplete"
)

const (
	ReportSchemaVersion = 2
	ReportProtocol      = "contextctl.report.v2"

	CodeUnavailable     = "CTX-VALIDATOR-UNAVAILABLE"
	CodeUnsupported     = "CTX-CONTRACT-UNSUPPORTED"
	CodeInvalidReport   = CodeUnsupported // retained as a source-compatible alias
	CodeBundleChanged   = "CTX-VALIDATOR-BUNDLE-CHANGED"
	CodeTrustTamper     = "CTX-VALIDATOR-TRUST-TAMPER"
	CodeMigrationNeeded = "CTX-MIGRATION-REQUIRED"
	maxOutputBytes      = 1 << 20
	maxContextctlBytes  = 128 << 20
	maxRuntimeJSONBytes = 4 << 20
	// contextctl's frozen 840-second validator budget leaves one bounded minute
	// for startup, generic checks, serialization, and termination.
	defaultTimeout = 15 * time.Minute
)

var (
	dottedIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)+$`)
	checkIDPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)+$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	objectIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	semverPattern   = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

type Runtime struct {
	ContextctlPath   string
	ContextctlDigest string
	RegistryPath     string
	RegistryDigest   string
	TrustStatePath   string
	TrustStateDigest string
	SourceID         string
}

// Request names an immutable Git tree. Candidate content is data and never
// supplies an executable command, runner, or approval path.
type Request struct {
	RepositoryID   string
	SourceRoot     string
	RepositoryPath string
	Mode           Mode
	Tree           string
	Commit         string
	Runtime        Runtime
}

type Candidate struct {
	Kind     string `json:"kind"`
	SourceID string `json:"source_id"`
	ObjectID string `json:"object_id,omitempty"`
}

type Check struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Paths  []string `json:"paths"`
}

type ValidatorResult struct {
	ContractID            string   `json:"contract_id"`
	ContractVersion       string   `json:"contract_version"`
	Disposition           string   `json:"disposition"`
	CheckIDs              []string `json:"check_ids"`
	SourceID              string   `json:"source_id,omitempty"`
	SourceRole            string   `json:"source_role,omitempty"`
	CandidateBundleDigest string   `json:"candidate_bundle_digest,omitempty"`
	AcceptedBundleDigest  string   `json:"accepted_bundle_digest,omitempty"`
}

type LinkAction struct {
	Action     string `json:"action"`
	Ownership  string `json:"ownership"`
	SkillName  string `json:"skill_name,omitempty"`
	SourceID   string `json:"source_id,omitempty"`
	LinkPath   string `json:"link_path"`
	TargetPath string `json:"target_path,omitempty"`
}

type Report struct {
	SchemaVersion    int               `json:"schema_version"`
	Protocol         string            `json:"protocol"`
	ContractVersion  string            `json:"contract_version"`
	Command          string            `json:"command"`
	GeneratedAt      string            `json:"generated_at"`
	Verdict          Verdict           `json:"verdict"`
	Promotable       bool              `json:"promotable"`
	Candidate        Candidate         `json:"candidate"`
	Checks           []Check           `json:"checks"`
	ValidatorResults []ValidatorResult `json:"validator_results"`
	LinkActions      []LinkAction      `json:"link_actions"`
}

type Finding struct {
	CheckID string `json:"check_id"`
	Path    string `json:"path,omitempty"`
}

type ProtocolError struct {
	Code string
}

func (e *ProtocolError) Error() string { return "trusted validation protocol failed" }

func ErrorCode(err error) string {
	var protocolError *ProtocolError
	if errors.As(err, &protocolError) {
		return protocolError.Code
	}
	return CodeUnsupported
}

func (r Report) Findings() []Finding {
	var findings []Finding
	for _, check := range r.Checks {
		if check.Status == "pass" || check.Status == "skip" {
			continue
		}
		if len(check.Paths) == 0 {
			findings = append(findings, Finding{CheckID: check.ID})
			continue
		}
		for _, path := range check.Paths {
			findings = append(findings, Finding{CheckID: check.ID, Path: path})
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].CheckID == findings[j].CheckID {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].CheckID < findings[j].CheckID
	})
	return findings
}

type Validator interface {
	Validate(context.Context, Request) (Report, error)
}

type Unavailable struct{}

func (Unavailable) Validate(context.Context, Request) (Report, error) {
	return Report{}, &ProtocolError{Code: CodeUnavailable}
}

type Executor interface {
	Run(ctx context.Context, executable string, args []string, stdout, stderr io.Writer) (int, error)
}

type Runner struct {
	Executor Executor
	Timeout  time.Duration
}

func (r Runner) Validate(parent context.Context, request Request) (Report, error) {
	if err := ValidateRequest(request); err != nil {
		return Report{}, err
	}
	prepared, err := prepareRuntime(parent, request)
	if err != nil {
		return Report{}, err
	}
	defer prepared.close()
	executor := r.Executor
	if executor == nil {
		executor = SystemExecutor{}
	}
	timeout := r.Timeout
	if timeout <= 0 || timeout > defaultTimeout {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	stdout := &limitedBuffer{limit: maxOutputBytes}
	stderr := &limitedBuffer{limit: maxOutputBytes}
	args := []string{
		"validate", "--format", "json",
		"--registry", prepared.registry.path,
		"--trust-state", prepared.trustState.path,
		"--source", request.Runtime.SourceID,
		"--source-root", request.SourceRoot,
		"--git-repository", request.RepositoryPath,
		"--git-tree", request.Tree,
	}
	if err := prepared.verify(ctx); err != nil {
		return Report{}, err
	}
	exitCode, runErr := executor.Run(ctx, prepared.contextctl.path, args, stdout, stderr)
	if errors.Is(stdout.err, errOutputLimit) || errors.Is(stderr.err, errOutputLimit) {
		return Report{}, fmt.Errorf("contextctl output exceeded the protocol limit")
	}
	if ctx.Err() != nil {
		return Report{}, fmt.Errorf("contextctl validation timed out")
	}
	if stderr.Len() != 0 {
		return Report{}, fmt.Errorf("contextctl wrote unexpected stderr")
	}
	report, decodeErr := decodeReport(stdout.Bytes())
	if decodeErr != nil {
		var protocolError *ProtocolError
		if errors.As(decodeErr, &protocolError) {
			return Report{}, decodeErr
		}
		return Report{}, &ProtocolError{Code: CodeUnsupported}
	}
	if err := Normalize(&report, request); err != nil {
		return Report{}, err
	}
	if !validExitVerdict(exitCode, report.Verdict) {
		return Report{}, fmt.Errorf("contextctl exit status and verdict disagree")
	}
	if runErr != nil && exitCode == 0 {
		return Report{}, fmt.Errorf("contextctl execution failed")
	}
	if exitCode == 64 || exitCode == 70 || (exitCode != 0 && exitCode != 20 && exitCode != 21 && exitCode != 22) {
		return Report{}, fmt.Errorf("contextctl integration failed")
	}
	return report, nil
}

func ValidateRequest(request Request) error {
	if request.RepositoryID == "" || !filepath.IsAbs(request.SourceRoot) || !filepath.IsAbs(request.RepositoryPath) {
		return fmt.Errorf("repository identity and absolute path are required")
	}
	if request.Mode != ModePublish && request.Mode != ModeMirror {
		return fmt.Errorf("invalid validation mode")
	}
	if !objectIDPattern.MatchString(request.Tree) {
		return fmt.Errorf("invalid candidate tree")
	}
	if request.Commit != "" && !objectIDPattern.MatchString(request.Commit) {
		return fmt.Errorf("invalid candidate commit")
	}
	if len(request.Runtime.SourceID) > 160 || !dottedIDPattern.MatchString(request.Runtime.SourceID) {
		return fmt.Errorf("invalid validation source id")
	}
	for _, path := range []string{request.Runtime.ContextctlPath, request.Runtime.RegistryPath, request.Runtime.TrustStatePath} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("validation runtime paths must be absolute")
		}
	}
	for _, digest := range []string{request.Runtime.ContextctlDigest, request.Runtime.RegistryDigest, request.Runtime.TrustStateDigest} {
		if !digestPattern.MatchString(digest) {
			return fmt.Errorf("validation runtime digests are required")
		}
	}
	return nil
}

func Normalize(report *Report, request Request) error {
	if report.SchemaVersion == 1 || report.Protocol == "contextctl.report.v1" {
		major, _, stable := contractVersion(report.ContractVersion)
		if report.SchemaVersion == 1 && report.Protocol == "contextctl.report.v1" && stable && major == 1 {
			if err := validateReportBody(report, "", ""); err != nil {
				return &ProtocolError{Code: CodeUnsupported}
			}
			return &ProtocolError{Code: CodeMigrationNeeded}
		}
		return &ProtocolError{Code: CodeUnsupported}
	}
	if report.SchemaVersion != ReportSchemaVersion || report.Protocol != ReportProtocol || report.Command != "validate" {
		return &ProtocolError{Code: CodeUnsupported}
	}
	if !compatibleContract(report.ContractVersion) {
		return &ProtocolError{Code: CodeUnsupported}
	}
	return validateReportBody(report, request.Runtime.SourceID, request.Tree)
}

// validateReportBody checks the validation-report envelope independently of
// its schema-family header. Empty expected identities are used only to
// recognize a frozen v1 report: its candidate must still be intrinsically
// well-formed, but it is never accepted or interpreted as a v2 result.
func validateReportBody(report *Report, expectedSourceID, expectedObjectID string) error {
	if _, err := time.Parse(time.RFC3339, report.GeneratedAt); err != nil {
		return fmt.Errorf("contextctl generated_at is invalid")
	}
	if report.Command != "validate" {
		return fmt.Errorf("contextctl command is invalid")
	}
	if report.Verdict != VerdictPass && report.Verdict != VerdictFail && report.Verdict != VerdictHold && report.Verdict != VerdictIncomplete {
		return fmt.Errorf("contextctl verdict is invalid")
	}
	if report.Candidate.Kind != "git-tree" || len(report.Candidate.SourceID) > 160 || !dottedIDPattern.MatchString(report.Candidate.SourceID) || !objectIDPattern.MatchString(report.Candidate.ObjectID) {
		return fmt.Errorf("contextctl candidate identity is invalid")
	}
	if (expectedSourceID != "" && report.Candidate.SourceID != expectedSourceID) || (expectedObjectID != "" && report.Candidate.ObjectID != expectedObjectID) {
		return fmt.Errorf("contextctl candidate identity does not match the request")
	}
	reportSourceID := expectedSourceID
	if reportSourceID == "" {
		reportSourceID = report.Candidate.SourceID
	}
	if len(report.Checks) < 1 || len(report.Checks) > 512 || len(report.ValidatorResults) > 64 || len(report.LinkActions) != 0 {
		return fmt.Errorf("contextctl report collection bounds are invalid")
	}
	if report.Verdict == VerdictPass && len(report.ValidatorResults) == 0 {
		return fmt.Errorf("passing contextctl report has no applicable validator")
	}
	checkIDs := make(map[string]bool, len(report.Checks))
	hasFail := false
	hasHold := false
	hasSkip := false
	for _, check := range report.Checks {
		if !checkIDPattern.MatchString(check.ID) || len(check.ID) > 100 {
			return fmt.Errorf("contextctl check id is invalid")
		}
		if checkIDs[check.ID] {
			return fmt.Errorf("contextctl check ids are not unique")
		}
		checkIDs[check.ID] = true
		if check.Status != "pass" && check.Status != "fail" && check.Status != "hold" && check.Status != "skip" {
			return fmt.Errorf("contextctl check status is invalid")
		}
		if len(check.Paths) > 128 || !unique(check.Paths) {
			return fmt.Errorf("contextctl check paths are invalid")
		}
		for _, path := range check.Paths {
			if !safeDiagnosticPath(path) {
				return fmt.Errorf("contextctl diagnostic path is invalid")
			}
		}
		switch check.Status {
		case "fail":
			hasFail = true
		case "hold":
			hasHold = true
		case "skip":
			hasSkip = true
		}
	}
	validatorIdentities := make(map[string]bool, len(report.ValidatorResults))
	for _, result := range report.ValidatorResults {
		if len(result.ContractID) > 160 || !dottedIDPattern.MatchString(result.ContractID) || !semverPattern.MatchString(result.ContractVersion) {
			return fmt.Errorf("contextctl validator identity is invalid")
		}
		if result.Disposition != "pass" && result.Disposition != "fail" && result.Disposition != "hold" && result.Disposition != "skip" {
			return fmt.Errorf("contextctl validator disposition is invalid")
		}
		identity := result.ContractID + "@" + result.ContractVersion
		if validatorIdentities[identity] {
			return fmt.Errorf("contextctl validator identities are not unique")
		}
		validatorIdentities[identity] = true
		if result.SourceID != "" && (len(result.SourceID) > 160 || !dottedIDPattern.MatchString(result.SourceID) || result.SourceID != reportSourceID) {
			return fmt.Errorf("contextctl validator source identity is invalid")
		}
		if result.SourceRole != "" && result.SourceRole != "writable" && result.SourceRole != "context-mirror" && result.SourceRole != "development" && result.SourceRole != "deployed" {
			return fmt.Errorf("contextctl validator source role is invalid")
		}
		if len(result.CheckIDs) < 1 || len(result.CheckIDs) > 64 || !unique(result.CheckIDs) {
			return fmt.Errorf("contextctl validator check ids are invalid")
		}
		for _, id := range result.CheckIDs {
			if !checkIDPattern.MatchString(id) || !checkIDs[id] {
				return fmt.Errorf("contextctl validator check id is invalid")
			}
		}
		for _, digest := range []string{result.CandidateBundleDigest, result.AcceptedBundleDigest} {
			if digest != "" && !digestPattern.MatchString(digest) {
				return fmt.Errorf("contextctl validator digest is invalid")
			}
		}
		switch result.Disposition {
		case "fail":
			hasFail = true
		case "hold":
			hasHold = true
		case "skip":
			hasSkip = true
		}
	}
	derived := VerdictPass
	if hasFail {
		derived = VerdictFail
	} else if hasHold {
		derived = VerdictHold
	} else if hasSkip {
		derived = VerdictIncomplete
	}
	if report.Verdict != derived || report.Promotable != (derived == VerdictPass) {
		return fmt.Errorf("contextctl verdict is inconsistent with its checks")
	}
	return nil
}

func decodeReport(data []byte) (Report, error) {
	if len(data) == 0 || len(data) > maxOutputBytes {
		return Report{}, &ProtocolError{Code: CodeUnsupported}
	}
	family, err := classifyReportIdentity(data)
	if err != nil {
		return Report{}, err
	}
	if !bytes.Equal(data, bytes.TrimSpace(data)) {
		return Report{}, fmt.Errorf("contextctl report contains leading or trailing bytes")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return Report{}, fmt.Errorf("contextctl report contains duplicate object keys")
	}
	var required map[string]json.RawMessage
	if err := json.Unmarshal(data, &required); err != nil {
		return Report{}, fmt.Errorf("decode contextctl report")
	}
	for _, field := range []string{"schema_version", "protocol", "contract_version", "command", "generated_at", "verdict", "promotable", "checks", "validator_results", "link_actions", "candidate"} {
		if _, ok := required[field]; !ok {
			return Report{}, fmt.Errorf("contextctl report is missing a required field")
		}
	}
	if err := validateReportShape(required); err != nil {
		return Report{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var report Report
	if err := decoder.Decode(&report); err != nil {
		return Report{}, fmt.Errorf("decode contextctl report")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Report{}, fmt.Errorf("contextctl report contains trailing data")
	}
	if family == reportFamilyLegacy {
		if err := validateReportBody(&report, "", ""); err != nil {
			return Report{}, &ProtocolError{Code: CodeUnsupported}
		}
		return Report{}, &ProtocolError{Code: CodeMigrationNeeded}
	}
	return report, nil
}

type reportFamily uint8

const (
	reportFamilyCurrent reportFamily = iota + 1
	reportFamilyLegacy
)

func classifyReportIdentity(data []byte) (reportFamily, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return 0, &ProtocolError{Code: CodeUnsupported}
	}
	var identity struct {
		SchemaVersion   int    `json:"schema_version"`
		Protocol        string `json:"protocol"`
		ContractVersion string `json:"contract_version"`
	}
	if err := json.Unmarshal(data, &identity); err != nil {
		return 0, &ProtocolError{Code: CodeUnsupported}
	}
	if identity.SchemaVersion == 1 || identity.Protocol == "contextctl.report.v1" {
		major, _, stable := contractVersion(identity.ContractVersion)
		if identity.SchemaVersion == 1 && identity.Protocol == "contextctl.report.v1" && stable && major == 1 {
			return reportFamilyLegacy, nil
		}
		return 0, &ProtocolError{Code: CodeUnsupported}
	}
	if identity.SchemaVersion != ReportSchemaVersion || identity.Protocol != ReportProtocol {
		return 0, &ProtocolError{Code: CodeUnsupported}
	}
	major, minor, ok := contractVersion(identity.ContractVersion)
	if !ok || major != 2 || minor != 0 {
		return 0, &ProtocolError{Code: CodeUnsupported}
	}
	return reportFamilyCurrent, nil
}

func validateReportShape(top map[string]json.RawMessage) error {
	if !rawBoolean(top["promotable"]) || !rawObject(top["candidate"]) || !rawArray(top["checks"]) || !rawArray(top["validator_results"]) || !rawArray(top["link_actions"]) {
		return fmt.Errorf("contextctl report has an invalid required field type")
	}
	var candidate map[string]json.RawMessage
	if err := json.Unmarshal(top["candidate"], &candidate); err != nil {
		return fmt.Errorf("decode contextctl report")
	}
	for _, field := range []string{"kind", "source_id", "object_id"} {
		if value, ok := candidate[field]; !ok || !rawString(value) {
			return fmt.Errorf("contextctl candidate is missing a required field")
		}
	}
	var checks []map[string]json.RawMessage
	if err := json.Unmarshal(top["checks"], &checks); err != nil {
		return fmt.Errorf("decode contextctl report")
	}
	for _, check := range checks {
		for _, field := range []string{"id", "status", "paths"} {
			value, ok := check[field]
			if !ok || (field == "paths" && !rawArray(value)) || (field != "paths" && !rawString(value)) {
				return fmt.Errorf("contextctl check is missing a required field")
			}
		}
	}
	var results []map[string]json.RawMessage
	if err := json.Unmarshal(top["validator_results"], &results); err != nil {
		return fmt.Errorf("decode contextctl report")
	}
	for _, result := range results {
		for _, field := range []string{"contract_id", "contract_version", "disposition", "check_ids"} {
			value, ok := result[field]
			if !ok || (field == "check_ids" && !rawArray(value)) || (field != "check_ids" && !rawString(value)) {
				return fmt.Errorf("contextctl validator result is missing a required field")
			}
		}
		for _, field := range []string{"candidate_bundle_digest", "accepted_bundle_digest"} {
			if value, ok := result[field]; ok {
				var digest string
				if !rawString(value) || json.Unmarshal(value, &digest) != nil || !digestPattern.MatchString(digest) {
					return fmt.Errorf("contextctl validator digest has an invalid value")
				}
			}
		}
		for _, field := range []string{"source_id", "source_role"} {
			if value, ok := result[field]; ok && !rawString(value) {
				return fmt.Errorf("contextctl validator source field has an invalid value")
			}
		}
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing JSON token")
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return fmt.Errorf("duplicate JSON object key")
			}
			seen[key] = true
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

func rawArray(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return len(value) > 0 && value[0] == '['
}
func rawObject(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return len(value) > 0 && value[0] == '{'
}
func rawString(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return len(value) > 1 && value[0] == '"'
}
func rawBoolean(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return bytes.Equal(value, []byte("true")) || bytes.Equal(value, []byte("false"))
}

func compatibleContract(value string) bool {
	major, minor, ok := contractVersion(value)
	return ok && major == 2 && minor == 0
}

func contractVersion(value string) (int, int, bool) {
	if !semverPattern.MatchString(value) {
		return 0, 0, false
	}
	parts := strings.SplitN(value, ".", 3)
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	return major, minor, majorErr == nil && minorErr == nil
}

func validExitVerdict(exitCode int, verdict Verdict) bool {
	switch exitCode {
	case 0:
		return verdict == VerdictPass
	case 20:
		return verdict == VerdictFail
	case 21:
		return verdict == VerdictHold
	case 22:
		return verdict == VerdictIncomplete
	default:
		return false
	}
}

func safeDiagnosticPath(value string) bool {
	if value == "." {
		return true
	}
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\\\x00\r\n") || filepath.IsAbs(value) || regexp.MustCompile(`^[A-Za-z]:[\\/]`).MatchString(value) {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == ".." {
			return false
		}
	}
	return true
}

func unique(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func within(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

var errOutputLimit = errors.New("output limit exceeded")

type limitedBuffer struct {
	data  []byte
	limit int
	err   error
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	remaining := b.limit - len(b.data)
	if len(value) > remaining {
		if remaining > 0 {
			b.data = append(b.data, value[:remaining]...)
		}
		b.err = errOutputLimit
		return len(value), b.err
	}
	b.data = append(b.data, value...)
	return len(value), nil
}

func (b *limitedBuffer) Bytes() []byte { return b.data }
func (b *limitedBuffer) Len() int      { return len(b.data) }
