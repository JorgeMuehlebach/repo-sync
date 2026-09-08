//go:build windows

package background

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

type controller struct {
	program Program
	config  Config
}

func New(program Program, cfg Config) (Controller, error) {
	return &controller{program: program, config: cfg}, nil
}

func (c *controller) Install() error {
	return runPowerShell(registerTaskScript(c.config))
}

func (c *controller) Uninstall() error {
	if _, err := c.Status(); err != nil {
		return err
	}
	return runTask("/Delete", "/TN", c.config.DisplayName, "/F")
}

func (c *controller) Start() error {
	if err := runTask("/Change", "/TN", c.config.DisplayName, "/ENABLE"); err != nil {
		return err
	}
	return runTask("/Run", "/TN", c.config.DisplayName)
}

func (c *controller) Stop() error {
	_ = runTask("/End", "/TN", c.config.DisplayName)
	return runTask("/Change", "/TN", c.config.DisplayName, "/DISABLE")
}

func (c *controller) Run() error {
	if err := c.program.Start(); err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	select {
	case <-signals:
	case <-c.program.Done():
	}
	return c.program.Stop()
}

func (c *controller) Status() (Status, error) {
	if err := runTask("/Query", "/TN", c.config.DisplayName); err != nil {
		return StatusUnknown, ErrNotInstalled
	}
	script := fmt.Sprintf("(Get-ScheduledTask -TaskName '%s').State", strings.ReplaceAll(c.config.DisplayName, "'", "''"))
	output, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return StatusUnknown, nil
	}
	if strings.EqualFold(strings.TrimSpace(string(output)), "Running") {
		return StatusRunning, nil
	}
	return StatusStopped, nil
}

func runTask(args ...string) error {
	output, err := exec.Command("schtasks", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return nil
}

func runPowerShell(script string) error {
	output, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("register current-user scheduled task: %s", message)
	}
	return nil
}

func registerTaskScript(config Config) string {
	action := "$action = New-ScheduledTaskAction -Execute " + quotePowerShellLiteral(config.Executable)
	if len(config.Arguments) > 0 {
		arguments := make([]string, len(config.Arguments))
		for index, argument := range config.Arguments {
			arguments[index] = quoteWindowsArgument(argument)
		}
		action += " -Argument " + quotePowerShellLiteral(strings.Join(arguments, " "))
	}
	return strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		"$userId = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name",
		action,
		"$trigger = New-ScheduledTaskTrigger -AtLogOn -User $userId",
		"$principal = New-ScheduledTaskPrincipal -UserId $userId -LogonType Interactive -RunLevel Limited",
		"$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero)" +
			" -MultipleInstances IgnoreNew -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries",
		"Register-ScheduledTask -TaskName " + quotePowerShellLiteral(config.DisplayName) +
			" -Action $action -Trigger $trigger -Principal $principal -Settings $settings" +
			" -Description " + quotePowerShellLiteral(config.Description) + " -Force | Out-Null",
	}, "; ")
}

func quoteWindowsArgument(value string) string {
	return syscall.EscapeArg(value)
}

func quotePowerShellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
