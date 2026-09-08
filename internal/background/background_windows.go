//go:build windows

package background

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
)

type controller struct {
	program Program
	config  Config
}

func New(program Program, cfg Config) (Controller, error) {
	return &controller{program: program, config: cfg}, nil
}

func (c *controller) Install() error {
	command := quoteWindowsArgument(c.config.Executable)
	for _, arg := range c.config.Arguments {
		command += " " + quoteWindowsArgument(arg)
	}
	return runTask("/Create", "/SC", "ONLOGON", "/RL", "LIMITED", "/TN", c.config.DisplayName, "/TR", command, "/F")
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

func quoteWindowsArgument(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
