//go:build windows

package main

import (
	"sync/atomic"
	"time"
	"unsafe"
)

var (
	procSendInput              = user32.NewProc("SendInput")
	procActivateKeyboardLayout = user32.NewProc("ActivateKeyboardLayout")
	procGetKeyboardLayoutList  = user32.NewProc("GetKeyboardLayoutList")
	procPostMessage            = user32.NewProc("PostMessageW")
)

const (
	INPUT_KEYBOARD            = 1
	KEYEVENTF_KEYUP           = 0x0002
	KEYEVENTF_UNICODE         = 0x0004
	WM_INPUTLANGCHANGEREQUEST = 0x0050
	HWND_BROADCAST            = 0xFFFF
	HKL_NEXT                  = 1
)

type INPUT struct {
	Type uint32
	Ki   KEYBDINPUT
	_    [8]byte // padding
}

type KEYBDINPUT struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

var replacing int32

func replaceText(buf *Buffer, deleteChars int, newText string) {
	atomic.StoreInt32(&replacing, 1)
	buf.Clear()

	// Send backspaces
	for i := 0; i < deleteChars; i++ {
		sendKey(VK_BACK, 0)
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)

	// Send corrected text as unicode
	for _, ch := range newText {
		sendUnicode(ch)
		time.Sleep(5 * time.Millisecond)
	}

	// Switch keyboard layout
	switchLayoutWindows()
	time.Sleep(30 * time.Millisecond)

	atomic.StoreInt32(&replacing, 0)
}

func sendKey(vk uint16, flags uint32) {
	inputs := [2]INPUT{
		{Type: INPUT_KEYBOARD, Ki: KEYBDINPUT{WVk: vk, DwFlags: flags}},
		{Type: INPUT_KEYBOARD, Ki: KEYBDINPUT{WVk: vk, DwFlags: flags | KEYEVENTF_KEYUP}},
	}
	procSendInput.Call(2, uintptr(unsafe.Pointer(&inputs[0])), unsafe.Sizeof(inputs[0]))
}

func sendUnicode(ch rune) {
	inputs := [2]INPUT{
		{Type: INPUT_KEYBOARD, Ki: KEYBDINPUT{WScan: uint16(ch), DwFlags: KEYEVENTF_UNICODE}},
		{Type: INPUT_KEYBOARD, Ki: KEYBDINPUT{WScan: uint16(ch), DwFlags: KEYEVENTF_UNICODE | KEYEVENTF_KEYUP}},
	}
	procSendInput.Call(2, uintptr(unsafe.Pointer(&inputs[0])), unsafe.Sizeof(inputs[0]))
}

func switchLayoutWindows() {
	procActivateKeyboardLayout.Call(HKL_NEXT, 0)
}

// Go wrappers to match macOS API — used by main.go
func sendBackspaceKey() { sendKey(VK_BACK, 0) }
func sendChar(ch rune)  { sendUnicode(ch) }
func switchLang()       { switchLayoutWindows() }
func sendEnter()        { sendKey(VK_RETURN, 0) }

// IsRussianLayout — stub for Windows (layout detection via GetKeyboardLayout)
func IsRussianLayout() bool {
	hwnd, _, _ := procGetForegroundWindow.Call()
	tid, _, _ := procGetWindowThreadProcessId.Call(hwnd, 0)
	hkl, _, _ := procGetKeyboardLayout.Call(tid)
	// Russian LCID = 0x0419
	return (hkl & 0xFFFF) == 0x0419
}

// FrontmostAppID — stub for Windows (returns empty for now)
func FrontmostAppID() string {
	return ""
}

// Tray stubs for Windows (TODO: implement Win32 tray icon)
func startTray()                   {}
func runAppLoop()                  { select {} }
func isAutoConvertEnabled() bool   { return true }
func setAutoConvertEnabled(_ bool) {}

// installFrontmostObserver — no-op on Windows (FrontmostAppID uses direct call)
func installFrontmostObserver() {}

// Clipboard stubs for Windows (TODO: implement via OpenClipboard/SetClipboardData)
func readClipboard() string   { return "" }
func writeClipboard(s string) {}
func sendCopy()               { /* TODO: send Ctrl+C via SendInput */ }
func sendPaste()              { /* TODO: send Ctrl+V via SendInput */ }
