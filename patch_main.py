with open('main.go', 'r') as f:
    code = f.read()

target_convert = """		// Defeat macOS inline autocomplete (Spotlight, System Settings, Safari).
		// When text is highlighted (prediction), the first Backspace deletes the highlight.
		// By typing a comma, we force macOS to replace the prediction with a single comma.
		// Then we add 1 to our deleteCount to delete that comma too!
		sendChar(',')
		time.Sleep(15 * time.Millisecond)
		deleteCount++

		for i := 0; i < deleteCount; i++ {
			sendBackspaceKey()
			time.Sleep(30 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)"""

replacement_convert = """		// Defeat macOS inline autocomplete (Spotlight, System Settings, Safari).
		isSearchApp := (app == "com.apple.systempreferences" || app == "com.apple.Spotlight" || app == "com.raycast.macos" || app == "com.runningwithcrayons.Alfred")

		if isSearchApp {
			sendOptionBackspace()
			time.Sleep(50 * time.Millisecond)
		} else {
			for i := 0; i < deleteCount; i++ {
				sendBackspaceKey()
				time.Sleep(30 * time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond)
		}"""

code = code.replace(target_convert, replacement_convert)

target_enter = """					deleteCount := len([]rune(word))
					// Defeat macOS inline autocomplete unconditionally.
					sendChar(',')
					time.Sleep(15 * time.Millisecond)
					deleteCount++

					for i := 0; i < deleteCount; i++ {
						sendBackspaceKey()
						time.Sleep(30 * time.Millisecond)
					}
					time.Sleep(20 * time.Millisecond)"""

replacement_enter = """					isSearchApp := (app == "com.apple.systempreferences" || app == "com.apple.Spotlight" || app == "com.raycast.macos" || app == "com.runningwithcrayons.Alfred")

					if isSearchApp {
						sendOptionBackspace()
						time.Sleep(50 * time.Millisecond)
					} else {
					    deleteCount := len([]rune(word))
						for i := 0; i < deleteCount; i++ {
							sendBackspaceKey()
							time.Sleep(30 * time.Millisecond)
						}
						time.Sleep(20 * time.Millisecond)
					}"""
					
code = code.replace(target_enter, replacement_enter)

with open('main.go', 'w') as f:
    f.write(code)
