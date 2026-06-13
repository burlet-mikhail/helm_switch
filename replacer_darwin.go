//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation -framework Carbon -framework AppKit -framework ApplicationServices

#include <CoreGraphics/CoreGraphics.h>
#include <Carbon/Carbon.h>
#include <AppKit/AppKit.h>
#include <ApplicationServices/ApplicationServices.h>
#include <dispatch/dispatch.h>

// readClipboardString returns a strdup'd UTF-8 string from the clipboard, or NULL.
// Caller must free the returned pointer.
static const char* readClipboardString(void) {
    NSPasteboard* pb = [NSPasteboard generalPasteboard];
    NSString* s = [pb stringForType:NSPasteboardTypeString];
    if (s == nil) return NULL;
    return strdup([s UTF8String]);
}

// writeClipboardString sets the clipboard to the given UTF-8 string.
static void writeClipboardString(const char* utf8) {
    NSPasteboard* pb = [NSPasteboard generalPasteboard];
    [pb clearContents];
    NSString* s = [NSString stringWithUTF8String:utf8];
    [pb setString:s forType:NSPasteboardTypeString];
}

// clipboardChangeCount returns the pasteboard's changeCount, which increments
// on every write (including the OS-side write performed by a real copy). Diffing
// it across a synthetic Cmd+C is the canonical way to detect whether a copy
// actually happened — unlike string comparison, it correctly reports a copy
// even when the selected text is identical to the previous clipboard contents.
static long clipboardChangeCount(void) {
    return (long)[[NSPasteboard generalPasteboard] changeCount];
}

// --- Accessibility (AX) selection helpers ---------------------------------

// axCopyFocusedElement returns the focused UI element, or NULL. Caller releases.
static AXUIElementRef axCopyFocusedElement(void) {
    AXUIElementRef sys = AXUIElementCreateSystemWide();
    if (!sys) return NULL;
    CFTypeRef focused = NULL;
    AXError e = AXUIElementCopyAttributeValue(sys, kAXFocusedUIElementAttribute, &focused);
    CFRelease(sys);
    if (e != kAXErrorSuccess || !focused) return NULL;
    return (AXUIElementRef)focused;
}

// axSelectedText returns a malloc'd UTF-8 copy of the focused element's selected
// text, or NULL when there is no selection / AX is unavailable. Caller frees.
static char* axSelectedText(void) {
    AXUIElementRef el = axCopyFocusedElement();
    if (!el) return NULL;
    CFTypeRef sel = NULL;
    AXError e = AXUIElementCopyAttributeValue(el, kAXSelectedTextAttribute, &sel);
    CFRelease(el);
    if (e != kAXErrorSuccess || !sel) return NULL;
    if (CFGetTypeID(sel) != CFStringGetTypeID()) { CFRelease(sel); return NULL; }
    CFStringRef s = (CFStringRef)sel;
    CFIndex len = CFStringGetLength(s);
    if (len == 0) { CFRelease(sel); return NULL; }
    CFIndex maxBytes = CFStringGetMaximumSizeForEncoding(len, kCFStringEncodingUTF8) + 1;
    char* buf = (char*)malloc(maxBytes);
    if (!buf) { CFRelease(sel); return NULL; }
    Boolean ok = CFStringGetCString(s, buf, maxBytes, kCFStringEncodingUTF8);
    CFRelease(sel);
    if (!ok) { free(buf); return NULL; }
    return buf;
}

// axSelectionLength returns the length of the focused element's selected text
// range: 0 = no selection, >0 = selection of that many characters, -1 = AX
// cannot provide it (app unsupported). This works in more apps than reading the
// selected string, so it's used to decide whether a selection EXISTS before we
// risk an Option+Shift+Left that would otherwise shrink an existing selection.
static long axSelectionLength(void) {
    AXUIElementRef el = axCopyFocusedElement();
    if (!el) return -1;
    CFTypeRef val = NULL;
    AXError e = AXUIElementCopyAttributeValue(el, kAXSelectedTextRangeAttribute, &val);
    CFRelease(el);
    if (e != kAXErrorSuccess || !val) return -1;
    if (CFGetTypeID(val) != AXValueGetTypeID()) { CFRelease(val); return -1; }
    CFRange r = {0, 0};
    Boolean ok = AXValueGetValue((AXValueRef)val, kAXValueCFRangeType, &r);
    CFRelease(val);
    if (!ok) return -1;
    return (long)r.length;
}

// axSetSelectedText replaces the focused element's selected text with utf8 via
// the Accessibility API — no clipboard, no synthetic keys. Returns 1 on success.
static int axSetSelectedText(const char* utf8) {
    AXUIElementRef el = axCopyFocusedElement();
    if (!el) return 0;
    CFStringRef s = CFStringCreateWithCString(NULL, utf8, kCFStringEncodingUTF8);
    if (!s) { CFRelease(el); return 0; }
    AXError e = AXUIElementSetAttributeValue(el, kAXSelectedTextAttribute, s);
    CFRelease(s);
    CFRelease(el);
    return (e == kAXErrorSuccess) ? 1 : 0;
}

// sendOptionShiftLeft selects the word to the left of the caret
// (Option+Shift+Left = "select previous word" on macOS).
static void sendOptionShiftLeft(void) {
    CGEventRef down = CGEventCreateKeyboardEvent(NULL, 0x7B, true);
    CGEventRef up   = CGEventCreateKeyboardEvent(NULL, 0x7B, false);
    CGEventFlags f = kCGEventFlagMaskAlternate | kCGEventFlagMaskShift;
    CGEventSetFlags(down, f);
    CGEventSetFlags(up, f);
    CGEventPost(kCGHIDEventTap, down);
    usleep(15000);
    CGEventPost(kCGHIDEventTap, up);
    CFRelease(down);
    CFRelease(up);
}

// sendCmdC sends Cmd+C (copy)
static void sendCmdC(void) {
    CGEventRef down = CGEventCreateKeyboardEvent(NULL, 0x08, true);
    CGEventSetFlags(down, kCGEventFlagMaskCommand);
    CGEventRef up = CGEventCreateKeyboardEvent(NULL, 0x08, false);
    CGEventSetFlags(up, kCGEventFlagMaskCommand);
    CGEventPost(kCGHIDEventTap, down);
    usleep(15000);
    CGEventPost(kCGHIDEventTap, up);
    CFRelease(down);
    CFRelease(up);
}

// sendCmdV sends Cmd+V (paste)
static void sendCmdV(void) {
    CGEventRef down = CGEventCreateKeyboardEvent(NULL, 0x09, true);
    CGEventSetFlags(down, kCGEventFlagMaskCommand);
    CGEventRef up = CGEventCreateKeyboardEvent(NULL, 0x09, false);
    CGEventSetFlags(up, kCGEventFlagMaskCommand);
    CGEventPost(kCGHIDEventTap, down);
    usleep(15000);
    CGEventPost(kCGHIDEventTap, up);
    CFRelease(down);
    CFRelease(up);
}

void sendBackspace(void) {
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
}

void sendCmdBackspace(void) {
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
}

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
}

// physicalKeycode returns the macOS virtual keycode for common ASCII chars.
// For Cyrillic and other chars we use 0 (overridden by the unicode string).
// Using the correct keycode for ASCII punctuation prevents apps from
// re-interpreting the event using the current keyboard layout.
static CGKeyCode physicalKeycode(UniChar ch) {
    switch (ch) {
        case ' ':  return 0x31; // kVK_Space
        case ',':  return 0x2B; // kVK_ANSI_Comma
        case '.':  return 0x2F; // kVK_ANSI_Period
        case ';':  return 0x29; // kVK_ANSI_Semicolon
        case '\'': return 0x27; // kVK_ANSI_Quote
        case '[':  return 0x21; // kVK_ANSI_LeftBracket
        case ']':  return 0x1E; // kVK_ANSI_RightBracket
        case '`':  return 0x32; // kVK_ANSI_Grave
        default:   return 0;
    }
}

void sendEnter(void) {
    CGEventRef down = CGEventCreateKeyboardEvent(NULL, 0x24, true);
    CGEventRef up   = CGEventCreateKeyboardEvent(NULL, 0x24, false);
    CGEventSetFlags(down, 0);
    CGEventSetFlags(up, 0);
    CGEventPost(kCGHIDEventTap, down);
    usleep(15000);
    CGEventPost(kCGHIDEventTap, up);
    CFRelease(down);
    CFRelease(up);
}

void sendUnichar(UniChar ch) {
    CGKeyCode kc = physicalKeycode(ch);
    CGEventRef down = CGEventCreateKeyboardEvent(NULL, kc, true);
    CGEventRef up   = CGEventCreateKeyboardEvent(NULL, kc, false);
    CGEventSetFlags(down, 0);
    CGEventSetFlags(up, 0);
    UniChar c[1] = {ch};
    CGEventKeyboardSetUnicodeString(down, 1, c);
    CGEventKeyboardSetUnicodeString(up, 1, c);
    CGEventPost(kCGHIDEventTap, down);
    usleep(15000);
    CGEventPost(kCGHIDEventTap, up);
    CFRelease(down);
    CFRelease(up);
}

// Switch layout using TIS API directly (called from any thread)
void switchLayout(void) {
    CFDictionaryRef filter = CFDictionaryCreate(
        kCFAllocatorDefault,
        (const void *[]){kTISPropertyInputSourceCategory},
        (const void *[]){kTISCategoryKeyboardInputSource},
        1, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks
    );
    CFArrayRef sources = TISCreateInputSourceList(filter, false);
    CFRelease(filter);
    if (!sources) return;

    TISInputSourceRef current = TISCopyCurrentKeyboardInputSource();
    if (!current) { CFRelease(sources); return; }

    CFStringRef currentID = TISGetInputSourceProperty(current, kTISPropertyInputSourceID);
    CFIndex count = CFArrayGetCount(sources);

    // Collect only selectable keyboard sources
    TISInputSourceRef selectable[16];
    int nsel = 0;
    for (CFIndex i = 0; i < count && nsel < 16; i++) {
        TISInputSourceRef s = (TISInputSourceRef)CFArrayGetValueAtIndex(sources, i);
        CFBooleanRef canSelect = TISGetInputSourceProperty(s, kTISPropertyInputSourceIsSelectCapable);
        if (canSelect && CFBooleanGetValue(canSelect)) {
            selectable[nsel++] = s;
        }
    }

    // Find current index and pick next
    int curIdx = -1;
    for (int i = 0; i < nsel; i++) {
        CFStringRef sid = TISGetInputSourceProperty(selectable[i], kTISPropertyInputSourceID);
        if (sid && currentID && CFStringCompare(sid, currentID, 0) == kCFCompareEqualTo) {
            curIdx = i;
            break;
        }
    }

    if (curIdx >= 0 && nsel > 1) {
        int nextIdx = (curIdx + 1) % nsel;
        OSStatus err = TISSelectInputSource(selectable[nextIdx]);
        (void)err;
    }

    CFRelease(current);
    CFRelease(sources);
}
// Returns 1 if current input source contains "Russian", 0 otherwise
int isCurrentLayoutRussian(void) {
    TISInputSourceRef current = TISCopyCurrentKeyboardInputSource();
    if (!current) return 0;

    CFStringRef sourceID = TISGetInputSourceProperty(current, kTISPropertyInputSourceID);
    int result = 0;
    if (sourceID) {
        // Russian layouts typically have "Russian" in their ID
        CFRange range = CFStringFind(sourceID, CFSTR("Russian"), kCFCompareCaseInsensitive);
        if (range.location != kCFNotFound) result = 1;
    }
    CFRelease(current);
    return result;
}
*/
import "C"

import (
	"sync/atomic"
	"time"
	"unsafe"
)

// replacing guards against hook feedback loop
var replacing int32

// IsRussianLayout returns true if macOS is currently on Russian input source
func IsRussianLayout() bool {
	return C.isCurrentLayoutRussian() == 1
}

// Go wrappers for C functions — used by main.go for Enter handling
func sendBackspaceKey() { C.sendBackspace() }
func sendOptionBackspace() { C.sendOptionBackspace() }
func sendCmdBackspace() { C.sendCmdBackspace() }
func sendChar(ch rune)  { C.sendUnichar(C.UniChar(ch)) }
func switchLang()       { C.switchLayout() }
func sendEnter()        { C.sendEnter() }

// Clipboard + paste helpers for the manual-conversion hotkey (Cmd+Shift+X)
func readClipboard() string {
	cstr := C.readClipboardString()
	if cstr == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(cstr))
	return C.GoString(cstr)
}

func writeClipboard(s string) {
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	C.writeClipboardString(cs)
}

func sendCopy()  { C.sendCmdC() }
func sendPaste() { C.sendCmdV() }

// clipboardChangeCount returns the pasteboard's monotonically increasing change
// counter. Used by the selection probe to detect a real copy reliably.
func clipboardChangeCount() int64 { return int64(C.clipboardChangeCount()) }

// axSelectedText returns the focused element's selected text via the
// Accessibility API, or "" if there is no selection or AX is unavailable.
func axSelectedText() string {
	cstr := C.axSelectedText()
	if cstr == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(cstr))
	return C.GoString(cstr)
}

// axSelectionLength reports the length of the current selection: 0 = none,
// >0 = a selection of that many characters, -1 = AX cannot tell (app
// unsupported). Used to decide whether a selection already exists.
func axSelectionLength() int64 { return int64(C.axSelectionLength()) }

// axSetSelectedText replaces the focused element's selected text via the
// Accessibility API (no clipboard, no keystrokes). Returns false if the app
// does not support AX text mutation.
func axSetSelectedText(s string) bool {
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	return C.axSetSelectedText(cs) == 1
}

// sendOptionShiftLeft selects the word immediately left of the caret.
func sendOptionShiftLeft() { C.sendOptionShiftLeft() }

func replaceText(buf *Buffer, deleteChars int, newText string) {
	if !atomic.CompareAndSwapInt32(&replacing, 0, 1) {
		vlog("REPLACE SKIPPED (already replacing): %q", newText)
		return
	}
	vlog("REPLACE START: delete=%d text=%q", deleteChars, newText)
	buf.Clear()

	// Give OS time to process the space/boundary before sending backspaces
	time.Sleep(50 * time.Millisecond)

	if isSearchApp(FrontmostAppID()) {
		C.sendOptionBackspace()
		time.Sleep(50 * time.Millisecond)
	} else {
		// Delete old text (word + boundary char)
		for i := 0; i < deleteChars; i++ {
			C.sendBackspace()
			time.Sleep(15 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Type corrected text + space
	for _, ch := range newText {
		C.sendUnichar(C.UniChar(ch))
		time.Sleep(5 * time.Millisecond)
	}
	vlog("REPLACE TYPED: %q", newText)

	// Switch system layout after typing
	C.switchLayout()
	time.Sleep(30 * time.Millisecond)

	atomic.StoreInt32(&replacing, 0)
	vlog("REPLACE DONE")
}
