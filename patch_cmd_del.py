with open('replacer_darwin.go', 'r') as f:
    code = f.read()

target = """void sendOptionBackspace(void) {"""
replacement = """void sendCmdBackspace(void) {
    CGEventRef down = CGEventCreateKeyboardEvent(NULL, 0x33, true);
    CGEventRef up   = CGEventCreateKeyboardEvent(NULL, 0x33, false);
    CGEventSetFlags(down, kCGEventFlagMaskCommand);
    CGEventSetFlags(up, kCGEventFlagMaskCommand);
    CGEventPost(kCGHIDEventTap, down);
    usleep(15000); // 15ms
    CGEventPost(kCGHIDEventTap, up);
    CFRelease(down);
    CFRelease(up);
}

void sendOptionBackspace(void) {"""
code = code.replace(target, replacement)

code = code.replace("func sendOptionBackspace() { C.sendOptionBackspace() }", "func sendOptionBackspace() { C.sendOptionBackspace() }\nfunc sendCmdBackspace() { C.sendCmdBackspace() }")

with open('replacer_darwin.go', 'w') as f:
    f.write(code)

with open('main.go', 'r') as f:
    code = f.read()

code = code.replace("sendOptionBackspace()", "log.Printf(\"Executing Cmd+Backspace for search app: %q\", app)\n\t\t\tsendCmdBackspace()")

with open('main.go', 'w') as f:
    f.write(code)

