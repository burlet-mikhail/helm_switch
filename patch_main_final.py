with open('main.go', 'r') as f:
    code = f.read()

target = """		app := FrontmostAppID()
		log.Printf("Executing conversion in app: %q", app)
		isSearchApp := (app == "com.apple.systempreferences" || app == "com.apple.Spotlight" || app == "com.raycast.macos" || app == "com.runningwithcrayons.Alfred")

		if isSearchApp {
			log.Printf("Executing Cmd+Backspace for search app: %q", app)
			sendCmdBackspace()
			time.Sleep(50 * time.Millisecond)
		} else {
			deleteCount := len([]rune(current))
			if isFlushed {
				deleteCount++ // delete the trailing space too
			}
			for i := 0; i < deleteCount; i++ {
				sendBackspaceKey()
				time.Sleep(15 * time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond)
		}"""

replacement = """		app := FrontmostAppID()
		isSearchApp := (app == "com.apple.systempreferences" || app == "com.apple.Spotlight" || app == "com.raycast.macos" || app == "com.runningwithcrayons.Alfred")

		deleteCount := len([]rune(current))
		if isFlushed {
			deleteCount++ // delete the trailing space too
		}

		if isSearchApp {
			// Defeat macOS inline autocomplete via comma trick.
			// Commas clear the highlight safely.
			sendChar(',')
			time.Sleep(25 * time.Millisecond)
			deleteCount++ // delete the comma
		}

		// Delete old text with a slow, reliable loop to prevent dropped events
		for i := 0; i < deleteCount; i++ {
			sendBackspaceKey()
			time.Sleep(30 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)"""

code = code.replace(target, replacement)


target2 = """					app := FrontmostAppID()
					isSearchApp := (app == "com.apple.systempreferences" || app == "com.apple.Spotlight" || app == "com.raycast.macos" || app == "com.runningwithcrayons.Alfred")

					if isSearchApp {
						log.Printf("Executing Cmd+Backspace for search app: %q", app)
						sendCmdBackspace()
						time.Sleep(50 * time.Millisecond)
					} else {
						wordRunes := []rune(word)
						for i := 0; i < len(wordRunes); i++ {
							sendBackspaceKey()
							time.Sleep(15 * time.Millisecond)
						}
						time.Sleep(20 * time.Millisecond)
					}"""

replacement2 = """					app := FrontmostAppID()
					isSearchApp := (app == "com.apple.systempreferences" || app == "com.apple.Spotlight" || app == "com.raycast.macos" || app == "com.runningwithcrayons.Alfred")

					deleteCount := len([]rune(word))
					if isSearchApp {
						sendChar(',')
						time.Sleep(25 * time.Millisecond)
						deleteCount++
					}

					for i := 0; i < deleteCount; i++ {
						sendBackspaceKey()
						time.Sleep(30 * time.Millisecond)
					}
					time.Sleep(20 * time.Millisecond)"""
					
code = code.replace(target2, replacement2)

with open('main.go', 'w') as f:
    f.write(code)
