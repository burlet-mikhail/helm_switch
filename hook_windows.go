//go:build windows

package main

import (
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procSetWindowsHook           = user32.NewProc("SetWindowsHookExW")
	procCallNextHook             = user32.NewProc("CallNextHookEx")
	procGetMessage               = user32.NewProc("GetMessageW")
	procUnhookWindows            = user32.NewProc("UnhookWindowsHookEx")
	procToUnicodeEx              = user32.NewProc("ToUnicodeEx")
	procGetKeyboardState         = user32.NewProc("GetKeyboardState")
	procGetKeyboardLayout        = user32.NewProc("GetKeyboardLayout")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
)

const (
	WH_KEYBOARD_LL = 13
	WM_KEYDOWN     = 0x0100
	VK_BACK        = 0x08
	VK_RETURN      = 0x0D
	VK_Z           = 0x5A
)

type KBDLLHOOKSTRUCT struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type MSG struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

var (
	hookHandle uintptr
	hookMu     sync.Mutex
)

// onKeyEvent is set by main() — called synchronously from hook callback.
// Returns true to suppress the event (Windows: not supported yet, returns
// 0 anyway). Signature mirrors hook_darwin.go so main.go compiles on both
// platforms without build tags.
var onKeyEvent func(keycode uint16, char rune, flags int64) bool

// Mirrors of the macOS constants used by main.go. The Caps Lock → F18
// HID-level remap is macOS-only; on Windows the Caps Lock hotkey path
// simply never fires (no remap, no F18 events).
//
// IMPORTANT: f18KeyCode must be a sentinel value that no real Windows VK
// can produce. The macOS virtual keycode for F18 is 0x4F, but on Windows
// 0x4F is VK_O — every "O" keystroke would otherwise match the F18
// dispatch in main.go's onKeyEvent (since the Windows hook always passes
// flags=0, the `flags&anyRealModifierMask == 0` guard would fire on every
// plain "O" press) and spawn handleCapsLock → convertLastWordFromBuffer,
// mangling user input. Windows VK codes are documented as 0x00–0xFE, so
// 0xFFFF can never collide with a real key.
const (
	f18KeyCode          uint16 = 0xFFFF
	anyRealModifierMask int64  = (1 << 17) | (1 << 18) | (1 << 19) | (1 << 20)
)

// Windows key event masks (placeholder for compat with macOS code)
const kCGEventFlagMaskCommand = 0

func startHook() error {
	go func() {
		callback := syscall.NewCallback(func(nCode int, wParam uintptr, lParam uintptr) uintptr {
			if nCode >= 0 && wParam == WM_KEYDOWN {
				if atomic.LoadInt32(&replacing) == 1 {
					ret, _, _ := procCallNextHook.Call(hookHandle, uintptr(nCode), wParam, lParam)
					return ret
				}

				kb := (*KBDLLHOOKSTRUCT)(unsafe.Pointer(lParam))
				ch := vkToUnicode(kb.VkCode, kb.ScanCode)

				if onKeyEvent != nil {
					// Windows: can't easily suppress via LL hook callback, so always pass through.
					// Enter interception is limited compared to macOS.
					// Caps Lock as a hotkey is not implemented on Windows
					// (no hidutil-equivalent HID-level remap is wired up).
					onKeyEvent(uint16(kb.VkCode), ch, 0)
				}
			}
			ret, _, _ := procCallNextHook.Call(hookHandle, uintptr(nCode), wParam, lParam)
			return ret
		})

		h, _, _ := procSetWindowsHook.Call(WH_KEYBOARD_LL, callback, 0, 0)
		hookHandle = h

		var msg MSG
		for {
			procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	return nil
}

func vkToUnicode(vkCode, scanCode uint32) rune {
	var keyState [256]byte
	procGetKeyboardState.Call(uintptr(unsafe.Pointer(&keyState[0])))

	hwnd, _, _ := procGetForegroundWindow.Call()
	tid, _, _ := procGetWindowThreadProcessId.Call(hwnd, 0)
	hkl, _, _ := procGetKeyboardLayout.Call(tid)

	var buf [4]uint16
	ret, _, _ := procToUnicodeEx.Call(
		uintptr(vkCode),
		uintptr(scanCode),
		uintptr(unsafe.Pointer(&keyState[0])),
		uintptr(unsafe.Pointer(&buf[0])),
		4, 0,
		hkl,
	)
	if int32(ret) > 0 {
		return rune(buf[0])
	}
	return 0
}
