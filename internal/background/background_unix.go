//go:build darwin

package background

import (
	"errors"
	"os"
	"os/signal"
	"syscall"

	kservice "github.com/kardianos/service"
)

type controller struct {
	service kservice.Service
	program Program
}

type serviceAdapter struct {
	program Program
}

func New(program Program, cfg Config) (Controller, error) {
	service, err := kservice.New(serviceAdapter{program: program}, &kservice.Config{
		Name:        cfg.Name,
		DisplayName: cfg.DisplayName,
		Description: cfg.Description,
		Executable:  cfg.Executable,
		Arguments:   cfg.Arguments,
		Option: kservice.KeyValue{
			"UserService":  true,
			"RunAtLoad":    true,
			"KeepAlive":    false,
			"LogOutput":    true,
			"LogDirectory": cfg.LogDirectory,
		},
	})
	if err != nil {
		return nil, err
	}
	return &controller{service: service, program: program}, nil
}

func (a serviceAdapter) Start(_ kservice.Service) error { return a.program.Start() }
func (a serviceAdapter) Stop(_ kservice.Service) error  { return a.program.Stop() }

func (c *controller) Install() error   { return mapError(c.service.Install()) }
func (c *controller) Uninstall() error { return mapError(c.service.Uninstall()) }
func (c *controller) Start() error {
	// launchctl load rejects a job that is still registered after its process
	// exited. Unload first so starting an installed, stopped job is reliable.
	_ = c.service.Stop()
	return mapError(c.service.Start())
}
func (c *controller) Stop() error {
	err := c.service.Stop()
	if err == nil {
		return nil
	}
	status, statusErr := c.service.Status()
	if statusErr == nil && status == kservice.StatusStopped {
		return nil
	}
	return mapError(err)
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
	status, err := c.service.Status()
	if err != nil {
		return StatusUnknown, mapError(err)
	}
	switch status {
	case kservice.StatusRunning:
		return StatusRunning, nil
	case kservice.StatusStopped:
		return StatusStopped, nil
	default:
		return StatusUnknown, nil
	}
}

func mapError(err error) error {
	if errors.Is(err, kservice.ErrNotInstalled) {
		return ErrNotInstalled
	}
	return err
}
