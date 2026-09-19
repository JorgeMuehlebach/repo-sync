package notify

import (
	"context"
)

type Delivery string

const (
	DeliverySent        Delivery = "sent"
	DeliveryUnavailable Delivery = "unavailable"
	DeliveryFailed      Delivery = "failed"
)

type Message struct {
	Title string
	Body  string
}

type Notifier interface {
	Notify(context.Context, Message) Delivery
}

type CommandRunner interface {
	LookPath(string) (string, error)
	Run(context.Context, string, ...string) error
}

type SystemCommandRunner struct{}

func (SystemCommandRunner) LookPath(name string) (string, error) { return platformLookPath(name) }

func (SystemCommandRunner) Run(ctx context.Context, executable string, args ...string) error {
	return platformRunCommand(ctx, executable, args...)
}

func NewNative() Notifier { return newPlatformNotifier(SystemCommandRunner{}) }

func NewNativeWithRunner(runner CommandRunner) Notifier { return newPlatformNotifier(runner) }

type commandNotifier struct {
	runner     CommandRunner
	executable string
	arguments  func(Message) []string
}

func (n commandNotifier) Notify(ctx context.Context, message Message) Delivery {
	executable, err := n.runner.LookPath(n.executable)
	if err != nil {
		return DeliveryUnavailable
	}
	if err := n.runner.Run(ctx, executable, n.arguments(message)...); err != nil {
		return DeliveryFailed
	}
	return DeliverySent
}
