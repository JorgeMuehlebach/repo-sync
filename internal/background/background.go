package background

import "errors"

var ErrNotInstalled = errors.New("background service is not installed")

type Status int

const (
	StatusUnknown Status = iota
	StatusRunning
	StatusStopped
)

type Program interface {
	Start() error
	Stop() error
	Done() <-chan struct{}
}

type Config struct {
	Name         string
	DisplayName  string
	Description  string
	Executable   string
	Arguments    []string
	LogDirectory string
}

type Controller interface {
	Install() error
	Uninstall() error
	Start() error
	Stop() error
	Run() error
	Status() (Status, error)
}
