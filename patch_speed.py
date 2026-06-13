with open('main.go', 'r') as f:
    code = f.read()

target1 = """		// Delete old text with a slow, reliable loop to prevent dropped events
		for i := 0; i < deleteCount; i++ {
			sendBackspaceKey()
			time.Sleep(30 * time.Millisecond)
		}"""

replacement1 = """		// Delete old text
		// In C, sendBackspaceKey already takes 15ms (usleep). We just add 5ms here.
		for i := 0; i < deleteCount; i++ {
			sendBackspaceKey()
			time.Sleep(5 * time.Millisecond)
		}"""

code = code.replace(target1, replacement1)

target2 = """					for i := 0; i < deleteCount; i++ {
						sendBackspaceKey()
						time.Sleep(30 * time.Millisecond)
					}"""

replacement2 = """					for i := 0; i < deleteCount; i++ {
						sendBackspaceKey()
						time.Sleep(5 * time.Millisecond)
					}"""

code = code.replace(target2, replacement2)

with open('main.go', 'w') as f:
    f.write(code)
