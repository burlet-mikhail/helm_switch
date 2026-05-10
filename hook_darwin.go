//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation -framework Carbon -framework ApplicationServices

#include <CoreGraphics/CoreGraphics.h>
#include <Carbon/Carbon.h>
#include <ApplicationServices/ApplicationServices.h>

// goKeyCallback returns 1 to suppress the event, 0 to pass through.
// Only fires for kCGEventKeyDown — Caps Lock is delivered as F18 (keycode
// 0x4F) thanks to the hidutil remap installed at startup, so we no longer
// need the flags-changed path that previously listened for Caps Lock.
extern int goKeyCallback(int64_t keycode, UniChar character, int64_t flags);

// f18VirtualKeyCode mirrors `f18KeyCode` on the Go side — kept inline here
// so the C autorepeat-skip branch doesn't need to call into Go.
#define F18_VIRTUAL_KEYCODE 0x4F

static CGEventRef eventCallback(CGEventTapProxy proxy, CGEventType type, CGEventRef event, void *refcon) {
    if (type == kCGEventTapDisabledByTimeout || type == kCGEventTapDisabledByUserInput) {
        CGEventTapEnable((CFMachPortRef)refcon, true);
        return event;
    }

    if (type != kCGEventKeyDown) return event;

    CGKeyCode keycode = (CGKeyCode)CGEventGetIntegerValueField(event, kCGKeyboardEventKeycode);
    CGEventFlags flags = CGEventGetFlags(event);

    // Drop autorepeat events for F18 (our remapped Caps Lock). Holding the
    // key down would otherwise trigger a clipboard probe storm. We still
    // suppress the event so downstream apps don't see a stream of F18
    // keystrokes either. Other keys keep autorepeat working as normal.
    int64_t autorepeat = CGEventGetIntegerValueField(event, kCGKeyboardEventAutorepeat);
    if (autorepeat && keycode == F18_VIRTUAL_KEYCODE) {
        return NULL;
    }

    UniChar ch = 0;
    UniChar chars[4];
    UniCharCount len = 0;
    CGEventKeyboardGetUnicodeString(event, 4, &len, chars);
    if (len > 0) ch = chars[0];

    int suppress = goKeyCallback((int64_t)keycode, ch, (int64_t)flags);
    if (suppress) return NULL;
    return event;
}

static bool checkAccessibility(void) {
    return AXIsProcessTrusted();
}

static void promptAccessibility(void) {
    NSDictionary *opts = @{(__bridge id)kAXTrustedCheckOptionPrompt: @YES};
    AXIsProcessTrustedWithOptions((__bridge CFDictionaryRef)opts);
}

static CFMachPortRef createTap(void) {
    // Caps Lock arrives as F18 (a normal key-down event) after the hidutil
    // remap — we no longer need the kCGEventFlagsChanged branch. Reverting
    // the mask to keyDown-only also keeps the tap from waking up for every
    // Shift/Cmd/Option transition.
    CGEventMask mask = (1 << kCGEventKeyDown);
    CFMachPortRef tap = CGEventTapCreate(
        kCGSessionEventTap,
        kCGHeadInsertEventTap,
        kCGEventTapOptionDefault,
        mask,
        eventCallback,
        NULL
    );
    return tap;
}

static int isTapNull(CFMachPortRef p) { return p == NULL; }
*/
import "C"

import (
	"errors"
	"sync/atomic"
	"time"
)

const (
	kCGEventFlagMaskCommand = 1 << 20
	// f18KeyCode is the macOS virtual keycode for F18. Caps Lock is remapped
	// to F18 at HID level by `remapCapsLockToF18` in main.go, so the event
	// tap receives normal key-down events with this keycode whenever the
	// physical Caps Lock key is pressed.
	f18KeyCode uint16 = 0x4F // 79
	// anyRealModifierMask combines the bits set when Shift, Control, Option
	// (Alt), or Command is held. Used to gate the F18 hotkey so that
	// e.g. Cmd+CapsLock (now Cmd+F18) passes through to the system instead
	// of triggering our converter.
	anyRealModifierMask int64 = (1 << 17) | (1 << 18) | (1 << 19) | (1 << 20)
)

// onKeyEvent is set by main() — called synchronously from CGEventTap
// callback. Returns true to suppress the event. The C-side callback only
// dispatches kCGEventKeyDown, so this is always invoked for a real
// key-down (autorepeat F18 events are filtered upstream in C).
var onKeyEvent func(keycode uint16, char rune, flags int64) bool

//export goKeyCallback
func goKeyCallback(keycode C.int64_t, character C.UniChar, flags C.int64_t) C.int {
	// Bail out early during our own paste/type sequences so synthetic events
	// issued by the OS in response to our input do not re-enter the handler.
	if atomic.LoadInt32(&replacing) == 1 {
		return 0
	}

	if onKeyEvent != nil {
		if onKeyEvent(uint16(keycode), rune(character), int64(flags)) {
			return 1 // suppress
		}
	}
	return 0
}

func startHook() error {
	if !bool(C.checkAccessibility()) {
		C.promptAccessibility()
		return errors.New("grant Accessibility in System Settings > Privacy & Security > Accessibility, then restart")
	}

	tap := C.createTap()
	if C.isTapNull(tap) != 0 {
		return errors.New("failed to create event tap")
	}

	src := C.CFMachPortCreateRunLoopSource(C.kCFAllocatorDefault, tap, 0)

	go func() {
		rl := C.CFRunLoopGetCurrent()
		C.CFRunLoopAddSource(rl, src, C.kCFRunLoopCommonModes)
		C.CGEventTapEnable(tap, C.bool(true))
		C.CFRunLoopRun()
	}()

	// Give the run loop a moment to start
	time.Sleep(50 * time.Millisecond)
	return nil
}
