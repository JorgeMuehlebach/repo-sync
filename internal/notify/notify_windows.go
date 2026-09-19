//go:build windows

package notify

func newPlatformNotifier(runner CommandRunner) Notifier {
	const script = `$title=$args[0];$body=$args[1];` +
		`[Windows.UI.Notifications.ToastNotificationManager,Windows.UI.Notifications,ContentType=WindowsRuntime]>$null;` +
		`$template=[Windows.UI.Notifications.ToastTemplateType]::ToastText02;` +
		`$xml=[Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent($template);` +
		`$text=$xml.GetElementsByTagName('text');$text.Item(0).AppendChild($xml.CreateTextNode($title))>$null;` +
		`$text.Item(1).AppendChild($xml.CreateTextNode($body))>$null;` +
		`$toast=[Windows.UI.Notifications.ToastNotification]::new($xml);` +
		`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('Repo Sync').Show($toast)`
	return commandNotifier{
		runner:     runner,
		executable: "powershell",
		arguments: func(message Message) []string {
			return []string{"-NoProfile", "-NonInteractive", "-Command", script, message.Title, message.Body}
		},
	}
}
