//go:build windows

package background

import "testing"

func TestQuoteWindowsArgument(t *testing.T) {
	if got, want := quoteWindowsArgument(`C:\Program Files\repo-sync.exe`), `"C:\Program Files\repo-sync.exe"`; got != want {
		t.Fatalf("quoteWindowsArgument() = %q, want %q", got, want)
	}
}
