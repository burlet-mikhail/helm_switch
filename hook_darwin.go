//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation -framework Carbon -framework ApplicationServices

#include <CoreGraphics/CoreGraphics.h>
#include <Carbon/Carbon.h>
#include <ApplicationServices/ApplicationServices.h>

// goKeyCallback returns 1 to suppress the event, 0 to pass through.
// eventType is the CGEventType (10 = key down, 12 = flags changed) so the Go
// side can branch — Caps Lock arrives as kCGEventFlagsChanged with no
// character data.
extern int goKeyCallback(int64_t eventType, int64_t keycode, UniChar character, int64_t flags);

static CGEventRef eventCallback(CGEventTapProxy proxy, CGEventType type, CGEventRef event, void *refcon) {
    if (type == kCGEventTapDisabledByTimeout || type == kCGEventTapDisabledByUserInput) {
        CGEventTapEnable((CFMachPortRef)refcon, true);
        return event;
    }

    if (type != kCGEventKeyDown && type != kCGEventFlagsChanged) return event;

    CGKeyCode keycode = (CGKeyCode)CGEventGetIntegerValueField(event, kCGKeyboardEventKeycode);
    CGEventFlags flags = CGEventGetFlags(event);

    UniChar ch = 0;
    if (type == kCGEventKeyDown) {
        UniChar chars[4];
        UniCharCount len = 0;
        CGEventKeyboardGetUnicodeString(event, 4, &len, chars);
        if (len > 0) ch = chars[0];
    }
    // For kCGEventFlagsChanged there is no Unicode payload — keep ch = 0.

    int suppress = goKeyCallback((int64_t)type, (int64_t)keycode, ch, (int64_t)flags);
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
    CGEventMask mask = (1 << kCGEventKeyDown) | (1 << kCGEventFlagsChanged);
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
	kCGEventFlagMaskCommand        = 1 << 20
	kCGEventTypeKeyDown      int64 = 10
	kCGEventTypeFlagsChanged int64 = 12
	capsLockKeyCode          uint16 = 0x39
	// NX_ALPHASHIFTMASK / kCGEventFlagMaskAlphaShift — set when Caps Lock is "on".
	capsLockMask int64 = 1 << 16
	// Combined real-modifier bits (shift|control|alt|command). Used to reject
	// e.g. Cmd+CapsLock so OS-level CapsLock+modifier shortcuts still pass.
	anyModifierMask int64 = (1 << 17) | (1 << 18) | (1 << 19) | (1 << 20)
)

// onKeyEvent is set by main() — called synchronously from CGEventTap callback.
// Returns true to suppress the event. eventType is the CGEventType (10 for
// key down, 12 for flags changed); flagschanged events carry no character
// payload (char == 0).
var onKeyEvent func(eventType int64, keycode uint16, char rune, flags int64) bool

//export goKeyCallback
func goKeyCallback(eventType C.int64_t, keycode C.int64_t, character C.UniChar, flags C.int64_t) C.int {
	// Bail out early during our own paste/type sequences so Caps Lock (and any
	// other event) issued by the OS in response to our synthesised input does
	// not re-enter the handler.
	if atomic.LoadInt32(&replacing) == 1 {
		return 0
	}

	if onKeyEvent != nil {
		if onKeyEvent(int64(eventType), uint16(keycode), rune(character), int64(flags)) {
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
