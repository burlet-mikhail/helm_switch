import re

with open('replacer_darwin.go', 'r') as f:
    code = f.read()

# Insert usleep(15000); after CGEventPost(kCGHIDEventTap, down);
# but ONLY for the keydown events (which don't have kCGEventFlagMaskCommand etc wait, all of them).
code = code.replace('CGEventPost(kCGHIDEventTap, down);\n    CGEventPost(kCGHIDEventTap, up);', 'CGEventPost(kCGHIDEventTap, down);\n    usleep(15000);\n    CGEventPost(kCGHIDEventTap, up);')

# Now add sendOptionBackspace after sendBackspace
target = """void sendBackspace(void) {
    CGEventRef down = CGEventCreateKeyboardEvent(NULL, 0x33, true);
    CGEventRef up   = CGEventCreateKeyboardEvent(NULL, 0x33, false);
    // Explicitly clear modifier flags so a preceding Cmd+C (sendCmdC) doesn't
    // leave Command "stuck" — CGEventCreateKeyboardEvent inherits the current
    // system modifier state, which can include stale bits from our own
    // synthetic Cmd+C/V events.
    CGEventSetFlags(down, 0);
    CGEventSetFlags(up, 0);
    CGEventPost(kCGHIDEventTap, down);
    usleep(15000);
    CGEventPost(kCGHIDEventTap, up);
    CFRelease(down);
    CFRelease(up);
}"""

replacement = target + """

void sendOptionBackspace(void) {
    CGEventRef down = CGEventCreateKeyboardEvent(NULL, 0x33, true);
    CGEventRef up   = CGEventCreateKeyboardEvent(NULL, 0x33, false);
    CGEventSetFlags(down, kCGEventFlagMaskAlternate);
    CGEventSetFlags(up, kCGEventFlagMaskAlternate);
    CGEventPost(kCGHIDEventTap, down);
    usleep(15000);
    CGEventPost(kCGHIDEventTap, up);
    CFRelease(down);
    CFRelease(up);
}"""

code = code.replace(target, replacement)

# Add export
code = code.replace('func sendBackspaceKey() { C.sendBackspace() }', 'func sendBackspaceKey() { C.sendBackspace() }\nfunc sendOptionBackspace() { C.sendOptionBackspace() }')

with open('replacer_darwin.go', 'w') as f:
    f.write(code)
