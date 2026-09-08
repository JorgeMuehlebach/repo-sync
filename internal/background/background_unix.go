//go:build !windows

package background

import (
	"errors"

	kservice "github.com/kardianos/service"
)

type controller struct {
	service kservice.Service
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
	return &controller{service: service}, nil
}

func (a serviceAdapter) Start(_ kservice.Service) error { return a.program.Start() }
func (a serviceAdapter) Stop(_ kservice.Service) error  { return a.program.Stop() }

func (c *controller) Install() error   { return mapError(c.service.Install()) }
func (c *controller) Uninstall() error { return mapError(c.service.Uninstall()) }
func (c *controller) Start() error     { return mapError(c.service.Start()) }
func (c *controller) Stop() error      { return mapError(c.service.Stop()) }
func (c *controller) Run() error       { return mapError(c.service.Run()) }

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
