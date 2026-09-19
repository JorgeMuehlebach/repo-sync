//go:build darwin

package notify

func newPlatformNotifier(runner CommandRunner) Notifier {
	return commandNotifier{
		runner:     runner,
		executable: "osascript",
		arguments: func(message Message) []string {
			return []string{
				"-e",
				"on run argv\n display notification (item 2 of argv) with title (item 1 of argv)\nend run",
				"--",
				message.Title,
				message.Body,
			}
		},
	}
}
