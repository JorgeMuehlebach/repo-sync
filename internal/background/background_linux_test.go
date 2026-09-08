//go:build linux

package background

import (
	"strings"
	"testing"
)

func TestRenderUnitUsesUserDefaultTarget(t *testing.T) {
	unit := renderUnit(Config{
		Name:        "repo-sync",
		Description: "Repository sync",
		Executable:  "/home/user/repo sync",
		Arguments:   []string{"run"},
	})
	for _, expected := range []string{
		`ExecStart="/home/user/repo sync" "run"`,
		"WantedBy=default.target",
		"Restart=on-failure",
	} {
		if !strings.Contains(unit, expected) {
			t.Fatalf("unit does not contain %q:\n%s", expected, unit)
		}
	}
}
