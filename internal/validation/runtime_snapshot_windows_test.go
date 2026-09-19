//go:build windows

package validation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
)

func TestRunnerRetainsWindowsSnapshotHandlesThroughExecution(t *testing.T) {
	request := testRequest(t)
	executor := &windowsSnapshotLockExecutor{report: validReport(request.Runtime.SourceID, request.Tree)}
	if _, err := (Runner{Executor: executor}).Validate(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !executor.called {
		t.Fatal("snapshot executor was not called")
	}
}

type windowsSnapshotLockExecutor struct {
	report Report
	called bool
}

func (e *windowsSnapshotLockExecutor) Run(_ context.Context, executable string, args []string, stdout, _ io.Writer) (int, error) {
	e.called = true
	for _, path := range []string{executable, argumentValue(args, "--registry"), argumentValue(args, "--trust-state")} {
		if err := os.Rename(path, path+".replacement"); err == nil {
			return -1, fmt.Errorf("retained snapshot was replaceable during execution")
		}
	}
	data, err := json.Marshal(e.report)
	if err != nil {
		return -1, err
	}
	_, err = stdout.Write(data)
	return 0, err
}
