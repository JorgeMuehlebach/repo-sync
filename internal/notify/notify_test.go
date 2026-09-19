package notify

import (
	"context"
	"errors"
	"testing"
)

type fakeRunner struct {
	missing bool
	fail    bool
	args    []string
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	if f.missing {
		return "", errors.New("missing")
	}
	return "/trusted/" + name, nil
}

func (f *fakeRunner) Run(_ context.Context, executable string, args ...string) error {
	f.args = append([]string{executable}, args...)
	if f.fail {
		return errors.New("failed")
	}
	return nil
}

func TestNativeNotifierReportsDeliveryWithoutOutput(t *testing.T) {
	for name, runner := range map[string]*fakeRunner{
		"sent":        {},
		"unavailable": {missing: true},
		"failed":      {fail: true},
	} {
		t.Run(name, func(t *testing.T) {
			got := NewNativeWithRunner(runner).Notify(context.Background(), Message{Title: "Repo Sync", Body: "repository failed"})
			want := map[string]Delivery{"sent": DeliverySent, "unavailable": DeliveryUnavailable, "failed": DeliveryFailed}[name]
			if got != want {
				t.Fatalf("Notify() = %q, want %q", got, want)
			}
		})
	}
}
