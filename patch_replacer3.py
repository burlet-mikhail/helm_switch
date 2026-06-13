with open('replacer_darwin.go', 'r') as f:
    code = f.read()

target = """	// Give OS time to process the space/boundary before sending backspaces
	time.Sleep(50 * time.Millisecond)

	app := FrontmostAppID()
	isSearchApp := (app == "com.apple.systempreferences" || app == "com.apple.Spotlight" || app == "com.raycast.macos" || app == "com.runningwithcrayons.Alfred")

	if isSearchApp {
		C.sendOptionBackspace()
		time.Sleep(50 * time.Millisecond)
	} else {
		// Delete old text (word + boundary char)
		for i := 0; i < deleteChars; i++ {
			C.sendBackspace()
			time.Sleep(15 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
	}"""

if target in code:
    pass # already applied!
else:
    print("Warning: could not find target!")

