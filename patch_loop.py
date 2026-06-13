with open('main.go', 'r') as f:
    code = f.read()

target = """		if isSearchApp {
			// Defeat macOS inline autocomplete via comma trick.
			// Commas clear the highlight safely.
			sendChar(',')
			time.Sleep(25 * time.Millisecond)
			deleteCount++ // delete the comma
		}"""

replacement = """		if isSearchApp {
			// Defeat macOS inline autocomplete by just sending a massive amount of backspaces!
			// This guarantees we delete the highlight AND the word.
			deleteCount += 5
		}"""

code = code.replace(target, replacement)


target2 = """					if isSearchApp {
						sendChar(',')
						time.Sleep(25 * time.Millisecond)
						deleteCount++
					}"""

replacement2 = """					if isSearchApp {
						deleteCount += 5
					}"""

code = code.replace(target2, replacement2)

with open('main.go', 'w') as f:
    f.write(code)
