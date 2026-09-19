//go:build linux

package notify

func newPlatformNotifier(runner CommandRunner) Notifier {
	return commandNotifier{
		runner:     runner,
		executable: "notify-send",
		arguments: func(message Message) []string {
			return []string{"--app-name=Repo Sync", message.Title, message.Body}
		},
	}
}
