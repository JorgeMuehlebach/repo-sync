//go:build linux

package background

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/JorgeMuehlebach/repo-sync/internal/atomicfile"
)

type controller struct {
	program  Program
	config   Config
	unitPath string
}

func New(program Program, cfg Config) (Controller, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return &controller{
		program:  program,
		config:   cfg,
		unitPath: filepath.Join(configDir, "systemd", "user", cfg.Name+".service"),
	}, nil
}

func (c *controller) Install() error {
	if err := atomicfile.Write(c.unitPath, []byte(renderUnit(c.config)), 0o644); err != nil {
		return err
	}
	return systemctl("daemon-reload")
}

func (c *controller) Uninstall() error {
	if _, err := c.Status(); err != nil {
		return err
	}
	_ = systemctl("disable", "--now", c.config.Name+".service")
	if err := os.Remove(c.unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return systemctl("daemon-reload")
}

func (c *controller) Start() error {
	return systemctl("enable", "--now", c.config.Name+".service")
}

func (c *controller) Stop() error {
	return systemctl("disable", "--now", c.config.Name+".service")
}

func (c *controller) Run() error {
	if err := c.program.Start(); err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case <-signals:
	case <-c.program.Done():
	}
	return c.program.Stop()
}

func (c *controller) Status() (Status, error) {
	if _, err := os.Stat(c.unitPath); errors.Is(err, os.ErrNotExist) {
		return StatusUnknown, ErrNotInstalled
	} else if err != nil {
		return StatusUnknown, err
	}
	cmd := exec.Command("systemctl", "--user", "is-active", c.config.Name+".service")
	output, err := cmd.CombinedOutput()
	if err == nil && strings.TrimSpace(string(output)) == "active" {
		return StatusRunning, nil
	}
	return StatusStopped, nil
}

func renderUnit(cfg Config) string {
	arguments := []string{systemdQuote(cfg.Executable)}
	for _, argument := range cfg.Arguments {
		arguments = append(arguments, systemdQuote(argument))
	}
	return fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=30

[Install]
WantedBy=default.target
`, strings.ReplaceAll(cfg.Description, "\n", " "), strings.Join(arguments, " "))
}

func systemdQuote(value string) string {
	return strconv.Quote(strings.ReplaceAll(value, "%", "%%"))
}

func systemctl(args ...string) error {
	commandArgs := append([]string{"--user"}, args...)
	output, err := exec.Command("systemctl", commandArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %s", strings.Join(commandArgs, " "), strings.TrimSpace(string(output)))
	}
	return nil
}
