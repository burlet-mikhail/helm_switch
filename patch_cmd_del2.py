with open('replacer_darwin.go', 'r') as f:
    code = f.read()

target = """void sendCmdBackspace(void) {
    CGEventRef down = CGEventCreateKeyboardEvent(NULL, 0x33, true);
    CGEventRef up   = CGEventCreateKeyboardEvent(NULL, 0x33, false);
    CGEventSetFlags(down, kCGEventFlagMaskCommand);
    CGEventSetFlags(up, kCGEventFlagMaskCommand);
    CGEventPost(kCGHIDEventTap, down);
    usleep(15000); // 15ms
    CGEventPost(kCGHIDEventTap, up);
    CFRelease(down);
    CFRelease(up);
}"""

replacement = """void sendCmdBackspace(void) {
    CGEventRef cmdDown = CGEventCreateKeyboardEvent(NULL, 0x37, true);
    CGEventPost(kCGHIDEventTap, cmdDown);
    usleep(15000);
    
    CGEventRef bsDown = CGEventCreateKeyboardEvent(NULL, 0x33, true);
    CGEventSetFlags(bsDown, kCGEventFlagMaskCommand);
    CGEventPost(kCGHIDEventTap, bsDown);
    usleep(15000);
    
    CGEventRef bsUp = CGEventCreateKeyboardEvent(NULL, 0x33, false);
    CGEventSetFlags(bsUp, kCGEventFlagMaskCommand);
    CGEventPost(kCGHIDEventTap, bsUp);
    usleep(15000);
    
    CGEventRef cmdUp = CGEventCreateKeyboardEvent(NULL, 0x37, false);
    CGEventPost(kCGHIDEventTap, cmdUp);
    usleep(15000);
    
    CFRelease(cmdDown);
    CFRelease(bsDown);
    CFRelease(bsUp);
    CFRelease(cmdUp);
}"""

code = code.replace(target, replacement)

with open('replacer_darwin.go', 'w') as f:
    f.write(code)
