package views

import "testing"

// Active sessions name the browser and system instead of the raw user agent,
// including browsers whose user agents name other browsers.
func TestBrowserName(t *testing.T) {
	for agent, want := range map[string]string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:135.0) Gecko/20100101 Firefox/135.0":                                                       "Firefox on Windows",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36 Edg/133.0.0.0":          "Edge on Windows",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3 Safari/605.1.15":                  "Safari on macOS",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_3 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/133.0 Mobile/15E148 Safari/604.1": "Chrome on iPhone",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Mobile Safari/537.36":                  "Chrome on Android",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36 OPR/117.0.0.0":                    "Opera on Linux",
		"Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36":                         "Chrome on ChromeOS",
		"Mozilla/5.0 (X11; Linux x86_64)": "A browser on Linux",
		"curl/8.5.0":                      "Unknown browser",
		"":                                "Unknown browser",
	} {
		if got := BrowserName(agent); got != want {
			t.Errorf("%q: got %q, want %q", agent, got, want)
		}
	}
}
