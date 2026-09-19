package validation

import (
	"context"
	"io"
	"os"
	"strings"
)

type SystemExecutor struct{}

func (SystemExecutor) Run(ctx context.Context, executable string, args []string, stdout, stderr io.Writer) (int, error) {
	var environment []string
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if unsafeInheritedEnvironment(name) {
			continue
		}
		environment = append(environment, value)
	}
	environment = append(environment, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_NO_LAZY_FETCH=1")
	return runIsolatedProcess(ctx, executable, args, environment, stdout, stderr)
}

func unsafeInheritedEnvironment(name string) bool {
	name = strings.ToUpper(name)
	if strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "GCM_") || strings.HasPrefix(name, "DYLD_") {
		return true
	}
	switch name {
	case "LD_PRELOAD", "LD_LIBRARY_PATH", "BASH_ENV", "ENV", "CDPATH", "PYTHONPATH", "PYTHONHOME", "RUBYOPT", "PERL5OPT":
		return true
	default:
		return false
	}
}
