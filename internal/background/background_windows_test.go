//go:build windows

package background

import (
	"strings"
	"testing"
)

func TestQuoteWindowsArgument(t *testing.T) {
	if got, want := quoteWindowsArgument(`C:\Program Files\repo-sync.exe`), `"C:\Program Files\repo-sync.exe"`; got != want {
		t.Fatalf("quoteWindowsArgument() = %q, want %q", got, want)
	}
}

func TestQuoteWindowsArgumentHandlesTrailingSlashAndQuotes(t *testing.T) {
	tests := map[string]string{
		"":                    `""`,
		`C:\path\`:            `C:\path\`,
		`C:\path with space\`: `"C:\path with space\\"`,
		`value"quoted`:        `value\"quoted`,
	}
	for input, want := range tests {
		if got := quoteWindowsArgument(input); got != want {
			t.Errorf("quoteWindowsArgument(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestQuotePowerShellLiteral(t *testing.T) {
	if got, want := quotePowerShellLiteral(`C:\Users\O'Brien\repo-sync.exe`), `'C:\Users\O''Brien\repo-sync.exe'`; got != want {
		t.Fatalf("quotePowerShellLiteral() = %q, want %q", got, want)
	}
}

func TestRegisterTaskScriptUsesCurrentUserInteractiveTask(t *testing.T) {
	script := registerTaskScript(Config{
		DisplayName: "Repo Sync",
		Description: "Synchronize repositories.",
		Executable:  `C:\Program Files\repo-sync\repo-sync.exe`,
		Arguments:   []string{"run"},
	})
	for _, fragment := range []string{
		`New-ScheduledTaskAction -Execute 'C:\Program Files\repo-sync\repo-sync.exe' -Argument 'run'`,
		`New-ScheduledTaskTrigger -AtLogOn -User $userId`,
		`New-ScheduledTaskPrincipal -UserId $userId -LogonType Interactive -RunLevel Limited`,
		`New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries`,
		`Register-ScheduledTask -TaskName 'Repo Sync'`,
	} {
		if !strings.Contains(script, fragment) {
			t.Errorf("registerTaskScript() missing %q in %q", fragment, script)
		}
	}
}
