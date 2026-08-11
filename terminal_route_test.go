package main

import "testing"

// TestRouteForCaps pins down where the Caps Lock handler is allowed to
// synthesise a word selection. Option+Shift+Left reaches a shell as the escape
// sequence ESC[1;4D — with zsh in vi mode that deletes to end of line and drops
// the shell into command mode — so every app that can put a terminal under the
// caret must be routed away from the selection gesture.
func TestRouteForCaps(t *testing.T) {
	tests := []struct {
		name string
		app  string
		role string
		want capsRoute
	}{
		// VS Code and its forks: no usable AX data, terminal panel indistinguishable.
		{"vscode", "com.microsoft.VSCode", "", capsRouteBufferOrSelection},
		{"vscode insiders", "com.microsoft.VSCodeInsiders", "", capsRouteBufferOrSelection},
		{"cursor", "com.todesktop.230313mzl4w4u92", "", capsRouteBufferOrSelection},
		{"windsurf", "com.exafunction.windsurf", "", capsRouteBufferOrSelection},
		{"antigravity", "com.google.antigravity-ide", "", capsRouteBufferOrSelection},
		{"vscodium", "com.vscodium", "", capsRouteBufferOrSelection},

		// Standalone terminal emulators: every text entry is a shell.
		{"terminal.app", "com.apple.Terminal", "AXTextArea", capsRouteBufferOnly},
		{"iterm2", "com.googlecode.iterm2", "AXTextArea", capsRouteBufferOnly},
		{"ghostty", "com.mitchellh.ghostty", "", capsRouteBufferOnly},
		{"warp", "dev.warp.Warp-Stable", "", capsRouteBufferOnly},

		// JetBrains: the role tells the JediTerm panel from everything else.
		{"jetbrains terminal panel", "com.jetbrains.PhpStorm", "AXTextArea", capsRouteBufferOnly},
		{"jetbrains editor", "com.jetbrains.PhpStorm", "JavaAxIgnore", capsRouteSelection},

		// Ordinary apps keep the selection gesture — it converts text the user
		// never typed through our hook, which the buffer cannot do.
		{"chrome", "com.google.Chrome", "", capsRouteSelection},
		{"telegram", "ru.keepcoder.Telegram", "AXTextField", capsRouteSelection},
		{"finder", "com.apple.finder", "AXTextField", capsRouteSelection},
		{"chatgpt", "com.openai.chat", "AXTextArea", capsRouteSelection},
		{"unknown app", "", "", capsRouteSelection},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeForCaps(tt.app, tt.role); got != tt.want {
				t.Errorf("routeForCaps(%q, %q) = %d, want %d", tt.app, tt.role, got, tt.want)
			}
		})
	}
}

// TestBufferMatchesApp covers the guard that stops a word typed in one app from
// being backspaced away in another — the terminal routes send blind backspaces,
// so a stale buffer would eat characters the user never typed there.
func TestBufferMatchesApp(t *testing.T) {
	saved := lastTypedApp.Load()
	t.Cleanup(func() { lastTypedApp.Store(saved) })

	lastTypedApp.Store(nil)
	if bufferMatchesApp("com.microsoft.VSCode") {
		t.Error("no keystroke recorded yet, buffer must not be trusted")
	}

	other := "ru.keepcoder.Telegram"
	lastTypedApp.Store(&other)
	if bufferMatchesApp("com.microsoft.VSCode") {
		t.Error("buffer typed in Telegram must not be trusted in VS Code")
	}
	if !bufferMatchesApp(other) {
		t.Error("buffer must be trusted in the app it was typed in")
	}
}
