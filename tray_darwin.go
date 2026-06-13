//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework Carbon

void ensureApp(void);
void runNSApp(void);
void removeTray(void);
void updateAutoConvertMenu(int enabled);
*/
import "C"

import (
	"log"
	"os"
	"sync/atomic"
)

// autoConvertEnabled controls the buffer-callback auto-conversion path
// (set to 0 to suppress automatic word fixes). It does NOT gate the manual
// convert_selection / convert_last_word hotkeys — those keep working
// regardless. Initialized to 1 (on); synced to cfg.AutoConvert at startup
// via setAutoConvertEnabled.
var autoConvertEnabled int32 = 1

// isAutoConvertEnabled reports whether the buffer-callback auto-conversion
// path should run. Used by the buffer callback and the early-return guard
// in onKeyEvent.
func isAutoConvertEnabled() bool {
	return atomic.LoadInt32(&autoConvertEnabled) == 1
}

// setAutoConvertEnabled sets the in-memory flag and refreshes the tray
// menu checkmark. Safe to call before the tray menu exists — the C side
// guards against a nil menu item.
func setAutoConvertEnabled(enabled bool) {
	if enabled {
		atomic.StoreInt32(&autoConvertEnabled, 1)
		C.updateAutoConvertMenu(1)
	} else {
		atomic.StoreInt32(&autoConvertEnabled, 0)
		C.updateAutoConvertMenu(0)
	}
}

//export goAutoConvertToggle
func goAutoConvertToggle() {
	if atomic.LoadInt32(&autoConvertEnabled) == 1 {
		atomic.StoreInt32(&autoConvertEnabled, 0)
		C.updateAutoConvertMenu(0)
		log.Println("Auto-convert: disabled")
	} else {
		atomic.StoreInt32(&autoConvertEnabled, 1)
		C.updateAutoConvertMenu(1)
		log.Println("Auto-convert: enabled")
	}

	if cfg, err := LoadConfig(); err == nil {
		cfg.AutoConvert = (atomic.LoadInt32(&autoConvertEnabled) == 1)
		if err := SaveConfig(cfg); err != nil {
			log.Printf("Failed to save config: %v", err)
		}
	}
}

//export goTrayQuit
func goTrayQuit() {
	log.Println("Quit from tray")
	// os.Exit skips deferred functions, so restore Caps Lock explicitly
	// here. restoreCapsLock is idempotent — safe even if the signal handler
	// or main() defer has already run.
	restoreCapsLock()
	C.removeTray()
	os.Exit(0)
}

func startTray() {
	C.ensureApp()
}

// runAppLoop runs NSApp run loop — blocks forever, must be called from main goroutine
func runAppLoop() {
	C.runNSApp()
}
