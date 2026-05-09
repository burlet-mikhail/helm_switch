package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func init() {
	// macOS requires NSApp and status bar to run on the actual OS main thread.
	runtime.LockOSThread()
}

const (
	macBackspace = 0x33
	macReturn    = 0x24
	macEnter     = 0x4C // numpad enter
	macZ         = 0x06 // Z key (still used by the Cmd+Z undo handler)
)

// lastReplace stores the last replacement for undo
type undoState struct {
	mu        sync.Mutex
	original  string // what was on screen before replacement (QWERTY text)
	replaced  string // what we typed instead
	timestamp time.Time
}

var undo undoState

func (u *undoState) Save(original, replaced string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.original = original
	u.replaced = replaced
	u.timestamp = time.Now()
}

func (u *undoState) Get() (original, replaced string, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.original == "" || time.Since(u.timestamp) > 5*time.Second {
		return "", "", false
	}
	orig, repl := u.original, u.replaced
	u.original = "" // consume — one undo only
	return orig, repl, true
}

// lastWordState tracks the most recent convert_last_word conversion so a
// repeated press of the hotkey can toggle back to the original word. The
// toggle window is invalidated by any non-modifier real keystroke (regular
// chars and backspace) — see Task 10 callsites in onKeyEvent.
type lastWordState struct {
	mu        sync.Mutex
	original  string // word as the user typed it (pre-conversion)
	converted string // what we typed instead
	active    bool   // true if the next hotkey press should toggle back
}

var lastWord lastWordState

// Set records a fresh conversion and arms the toggle-back window.
func (l *lastWordState) Set(original, converted string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.original = original
	l.converted = converted
	l.active = true
}

// Reset clears the toggle-back window. Called by the toggle-back path after
// reverting and by any real keystroke handler that signals "buffer changed".
func (l *lastWordState) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.original = ""
	l.converted = ""
	l.active = false
}

// Snapshot returns the current state under the mutex so callers can act on a
// consistent view without holding the lock during long operations.
func (l *lastWordState) Snapshot() (original, converted string, active bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.original, l.converted, l.active
}

// looksLikeContext returns true if the word looks like a URL, email, file path,
// or identifier that should NOT be auto-converted. Heuristics are conservative —
// we'd rather miss a conversion than mangle a URL.
func looksLikeContext(word string) bool {
	if word == "" {
		return false
	}
	hasDigit := false
	dots := 0
	for _, r := range word {
		switch r {
		case '@', '/', '\\', ':':
			return true // definitely URL, email, path, or namespaced identifier
		case '_':
			return true // snake_case identifier (common in code)
		case '-':
			// Could be a hyphenated word. Only skip if multiple hyphens or mixed with dots.
			dots++
		case '.':
			dots++
		}
		if r >= '0' && r <= '9' {
			hasDigit = true
		}
	}
	// "word.word" or "word.word.word" — likely URL/domain/filename
	if dots >= 1 && hasDigit {
		return true
	}
	if dots >= 2 {
		return true
	}
	return false
}

// convertSelectedText: copy selected text, convert QWERTY↔Cyrillic, paste back.
// Triggered by Cmd+Shift+X. Runs in a goroutine because it involves keystroke
// synthesis and clipboard I/O that should not block the event tap callback.
func convertSelectedText(detector *Detector) {
	// Save current clipboard so we can restore it afterwards.
	savedClipboard := readClipboard()

	// Copy selection into clipboard and wait for the OS to populate it.
	atomic.StoreInt32(&replacing, 1)
	sendCopy()
	time.Sleep(100 * time.Millisecond)

	selected := readClipboard()
	if selected == "" || selected == savedClipboard {
		// Nothing selected, or copy didn't update the clipboard.
		atomic.StoreInt32(&replacing, 0)
		if savedClipboard != "" {
			writeClipboard(savedClipboard)
		}
		log.Printf("Manual convert: no selection detected")
		return
	}

	// Convert the entire selection in whichever direction fits.
	// Heuristic: if it contains Cyrillic letters, convert RU→QWERTY; else QWERTY→RU.
	var converted string
	hasCyrillic := false
	for _, r := range selected {
		if r >= 'а' && r <= 'я' || r >= 'А' && r <= 'Я' || r == 'ё' || r == 'Ё' {
			hasCyrillic = true
			break
		}
	}
	if hasCyrillic {
		converted = RussianToQWERTY(selected)
	} else {
		converted = QWERTYToRussian(selected)
	}

	// Put converted text into clipboard and paste it over the selection.
	writeClipboard(converted)
	time.Sleep(30 * time.Millisecond)
	sendPaste()
	time.Sleep(150 * time.Millisecond)

	// Restore original clipboard so we don't pollute the user's copy/paste state.
	if savedClipboard != "" {
		writeClipboard(savedClipboard)
	}

	// Switch system layout too — user intent is obvious.
	switchLang()
	time.Sleep(30 * time.Millisecond)
	atomic.StoreInt32(&replacing, 0)

	log.Printf("Manual convert: %q → %q", selected, converted)
}

func main() {
	// CLI flags for exceptions store management — handled before tray/hook init
	var (
		flagListExceptions  = flag.Bool("list-exceptions", false, "print learned exceptions and exit")
		flagForget          = flag.String("forget", "", "remove exceptions for a word and exit")
		flagForgetApp       = flag.String("forget-app", "", "remove all exceptions for an app bundle id and exit")
		flagClearExceptions = flag.Bool("clear-exceptions", false, "remove all exceptions and exit")
		flagVerbose         = flag.Bool("verbose", false, "enable verbose per-keystroke logging")
	)
	flag.Parse()
	setVerbose(*flagVerbose)

	if *flagListExceptions || *flagForget != "" || *flagForgetApp != "" || *flagClearExceptions {
		store, err := NewExceptionStore()
		if err != nil {
			log.Fatalf("exceptions store: %v", err)
		}
		switch {
		case *flagListExceptions:
			entries := store.List()
			if len(entries) == 0 {
				fmt.Println("(no exceptions)")
				return
			}
			for _, e := range entries {
				fmt.Printf("%-40s  %-30s  %d hits  added=%s\n",
					e.App, e.Word, e.HitCount, e.Added.Format("2006-01-02"))
			}
		case *flagForget != "":
			n, err := store.Forget(*flagForget)
			if err != nil {
				log.Fatalf("forget: %v", err)
			}
			fmt.Printf("forgot %d entries for word %q\n", n, *flagForget)
		case *flagForgetApp != "":
			n, err := store.ForgetApp(*flagForgetApp)
			if err != nil {
				log.Fatalf("forget-app: %v", err)
			}
			fmt.Printf("forgot %d entries for app %q\n", n, *flagForgetApp)
		case *flagClearExceptions:
			if err := store.Clear(); err != nil {
				log.Fatalf("clear: %v", err)
			}
			fmt.Println("exceptions cleared")
		}
		return
	}

	// Log to file so we can debug when running as .app bundle
	logPath := filepath.Join(os.TempDir(), "ruswitch.log")
	logFile, logErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if logErr == nil {
		log.SetOutput(logFile)
	}

	log.Println("RuSwitch starting...")

	// Load config
	cfg, err := LoadConfig()
	if err != nil {
		log.Printf("Config warning: %v", err)
	}
	if !cfg.Enabled {
		log.Println("Disabled in config, exiting")
		return
	}

	// Parse hotkeys once at startup. Invalid specs fall back to defaults
	// (Cmd+Shift+X / Cmd+Shift+Z) with a logged warning — handled inside
	// ParsedHotkeys.
	selectionHotkey, lastWordHotkey := cfg.ParsedHotkeys()
	log.Printf("Hotkeys: convert_selection=%s convert_last_word=%s",
		cfg.Hotkeys.ConvertSelection, cfg.Hotkeys.ConvertLastWord)

	// Exceptions store + rollback tracker — learning from user corrections.
	// Store failures are non-fatal: we fall back to no-learning mode.
	store, err := NewExceptionStore()
	if err != nil {
		log.Printf("Exceptions store warning: %v — running without learning", err)
	}
	tracker := NewRollbackTracker(store)

	// Load dictionaries
	ruDict, err := LoadDict("ru")
	if err != nil {
		log.Fatalf("Cannot load Russian dict: %v", err)
	}
	log.Printf("Russian dict: %d words", len(ruDict.words))

	enDict, err := LoadDict("en")
	if err != nil {
		log.Fatalf("Cannot load English dict: %v", err)
	}
	log.Printf("English dict: %d words", len(enDict.words))

	// Init detector
	detector := NewDetector(ruDict, enDict)

	// doReplace performs replacement, saves undo state, and arms the rollback tracker.
	doReplace := func(buf *Buffer, word string, corrected string, deleteChars int, newText string) {
		undo.Save(word, newText)
		if tracker != nil {
			tracker.OnConversion(word, newText, FrontmostAppID())
		}
		replaceText(buf, deleteChars, newText)
	}

	// Create buffer with word callback (for space and other non-Enter boundaries)
	var buf *Buffer
	buf = NewBuffer(func(word string) {
		if !cfg.Enabled || atomic.LoadInt32(&replacing) == 1 || !isAutoConvertEnabled() {
			return
		}

		// Respect minimum word length from config (defaults to 2).
		if cfg.MinWordLength > 0 && len([]rune(word)) < cfg.MinWordLength {
			return
		}

		// URLs, emails, file paths, identifiers — leave them alone.
		if looksLikeContext(word) {
			return
		}

		// Skip replacement in excluded apps (e.g. IDEs, terminals).
		app := FrontmostAppID()
		if cfg.IsAppExcluded(app) {
			return
		}

		// Exception check: user has previously corrected this word in this app.
		if store != nil && store.IsException(app, word) {
			log.Printf("Exception skip: %q in %q", word, app)
			return
		}

		wrong, corrected := detector.Check(word)
		if !wrong {
			return
		}

		if detector.trailingPunct != 0 {
			wordRunes := []rune(word)
			pureWordLen := len(wordRunes) - 1
			lastChar := wordRunes[len(wordRunes)-1]
			if universalPunct[lastChar] {
				log.Printf("Fix (trail %c, no space): %q → %q", detector.trailingPunct, word, corrected)
				doReplace(buf, word, corrected, pureWordLen+1, corrected+string(detector.trailingPunct))
			} else {
				log.Printf("Fix (trail %c): %q → %q", detector.trailingPunct, word, corrected)
				doReplace(buf, word, corrected, pureWordLen+2, corrected+string(detector.trailingPunct)+" ")
			}
		} else {
			log.Printf("Fix: %q → %q", word, corrected)
			doReplace(buf, word, corrected, len([]rune(word))+1, corrected+" ")
		}
	})

	// Modifier mask covering all four "real" modifiers — used by the
	// convert_last_word handler to ensure no extra modifier (besides those
	// required by the configured hotkey) is held. Without this, e.g. plain
	// Cmd+Z (no Shift) would be misclassified when the configured hotkey
	// is Cmd+Shift+Z.
	const allModsMask = hotkeyFlagCommand | hotkeyFlagShift | hotkeyFlagControl | hotkeyFlagAlt

	// Set up key event handler — called synchronously from CGEventTap
	onKeyEvent = func(keycode uint16, char rune, flags int64) bool {
		// Debug: log any key event with Command modifier
		if (flags & hotkeyFlagCommand) != 0 {
			log.Printf("DEBUG keyevent: keycode=0x%02X char=%d flags=0x%X | want_lw: kc=0x%02X mod=0x%X | want_sel: kc=0x%02X mod=0x%X",
				keycode, char, flags,
				lastWordHotkey.KeyCode, lastWordHotkey.Modifiers,
				selectionHotkey.KeyCode, selectionHotkey.Modifiers)
		}

		// convert_last_word hotkey is checked BEFORE the cfg.Enabled /
		// auto-convert gate (so it still works when auto-convert is off)
		// AND before the Cmd+Z undo branch (so Cmd+Shift+Z is consumed
		// here and never reaches undo).
		if keycode == lastWordHotkey.KeyCode &&
			(flags&lastWordHotkey.Modifiers) == lastWordHotkey.Modifiers &&
			(flags & ^lastWordHotkey.Modifiers & allModsMask) == 0 {

			origPrev, convPrev, active := lastWord.Snapshot()
			current := buf.LastWord()

			if active {
				// Toggle-back path: revert convPrev → origPrev.
				go func() {
					atomic.StoreInt32(&replacing, 1)
					buf.Clear()
					for range convPrev {
						sendBackspaceKey()
						time.Sleep(5 * time.Millisecond)
					}
					time.Sleep(10 * time.Millisecond)
					for _, ch := range origPrev {
						sendChar(ch)
						time.Sleep(5 * time.Millisecond)
					}
					switchLang()
					time.Sleep(30 * time.Millisecond)
					// Toggling back ends the toggle window — a subsequent
					// press must re-convert from the buffer, not bounce.
					lastWord.Reset()
					atomic.StoreInt32(&replacing, 0)
					log.Printf("convert_last_word: revert %q → %q", convPrev, origPrev)
				}()
				return true
			}

			// Fresh-convert path.
			if current == "" {
				log.Printf("convert_last_word: empty buffer, no-op")
				return true
			}

			// If the current buffer is empty, LastWord() returned the
			// last flushed word — meaning a space/boundary was already
			// typed after it. We need to delete that trailing space too.
			isFlushed := buf.IsBufferEmpty()

			// Direction heuristic mirrors convertSelectedText: any Cyrillic
			// → assume RU was typed in QWERTY layout, convert RU→QWERTY;
			// otherwise QWERTY→RU.
			hasCyrillic := false
			for _, r := range current {
				if (r >= 'а' && r <= 'я') || (r >= 'А' && r <= 'Я') || r == 'ё' || r == 'Ё' {
					hasCyrillic = true
					break
				}
			}
			var converted string
			if hasCyrillic {
				converted = RussianToQWERTY(current)
			} else {
				converted = QWERTYToRussian(current)
			}

			go func() {
				atomic.StoreInt32(&replacing, 1)
				buf.Clear()

				deleteCount := len([]rune(current))
				if isFlushed {
					deleteCount++ // delete the trailing space too
				}
				for i := 0; i < deleteCount; i++ {
					sendBackspaceKey()
					time.Sleep(5 * time.Millisecond)
				}
				time.Sleep(10 * time.Millisecond)
				for _, ch := range converted {
					sendChar(ch)
					time.Sleep(5 * time.Millisecond)
				}
				if isFlushed {
					// Re-type the space we deleted
					sendChar(' ')
					time.Sleep(5 * time.Millisecond)
				}
				switchLang()
				time.Sleep(30 * time.Millisecond)
				// Arm the toggle window. The next convert_last_word press
				// (with no real keystroke in between) will revert this.
				lastWord.Set(current, converted)
				atomic.StoreInt32(&replacing, 0)
				log.Printf("convert_last_word: %q → %q (flushed=%v)", current, converted, isFlushed)
			}()
			return true
		}

		// Configurable selection-conversion hotkey (default Cmd+Shift+X).
		// Checked BEFORE the auto-convert gate so it works even when
		// auto-convert is off.
		if keycode == selectionHotkey.KeyCode &&
			(flags&selectionHotkey.Modifiers) == selectionHotkey.Modifiers &&
			(flags & ^selectionHotkey.Modifiers & allModsMask) == 0 {
			log.Printf("Manual convert hotkey (%s)", cfg.Hotkeys.ConvertSelection)
			go convertSelectedText(detector)
			return true // suppress the hotkey
		}

		if !cfg.Enabled || !isAutoConvertEnabled() {
			return false
		}

		// Cmd+Z — undo last replacement (within 5 seconds)
		if keycode == macZ && (flags&kCGEventFlagMaskCommand) != 0 {
			original, replaced, ok := undo.Get()
			if !ok {
				return false // no recent replacement, let Cmd+Z pass to app
			}
			// Explicit user rejection — learn this as an exception.
			if store != nil {
				app := FrontmostAppID()
				if err := store.Add(app, original); err == nil {
					log.Printf("Learned exception (Cmd+Z): %q in %q", original, app)
				}
			}
			log.Printf("Undo: reverting %q → %q", replaced, original)
			go func() {
				atomic.StoreInt32(&replacing, 1)
				buf.Clear()

				// Delete the replaced text
				for i := 0; i < len([]rune(replaced)); i++ {
					sendBackspaceKey()
					time.Sleep(5 * time.Millisecond)
				}
				time.Sleep(10 * time.Millisecond)

				// Type original text back
				for _, ch := range original {
					sendChar(ch)
					time.Sleep(5 * time.Millisecond)
				}

				// Switch layout back
				switchLang()
				time.Sleep(30 * time.Millisecond)
				atomic.StoreInt32(&replacing, 0)
			}()
			return true // suppress Cmd+Z
		}

		// Any other key clears undo window (user moved on)
		if keycode != macBackspace && char != 0 {
			// Don't clear on modifier-only keys
			if (flags & kCGEventFlagMaskCommand) == 0 {
				undo.mu.Lock()
				undo.original = ""
				undo.mu.Unlock()
			}
		}

		// Backspace
		if keycode == macBackspace {
			// Editing invalidates the convert_last_word toggle window:
			// the buffer no longer matches what we last converted.
			lastWord.Reset()
			buf.Backspace()
			if tracker != nil {
				tracker.ObserveKey(KeyObservation{Kind: KeyKindBackspace})
			}
			return false
		}

		// Skip null chars
		if char == 0 || char == 0x08 {
			return false
		}

		// Enter/Return — check word BEFORE letting Enter through
		if keycode == macReturn || keycode == macEnter || char == '\r' || char == '\n' {
			word := buf.FlushWord()
			if word == "" {
				if tracker != nil {
					tracker.ObserveKey(KeyObservation{Kind: KeyKindOther})
				}
				return false
			}

			// Respect min word length, context filter, and excluded apps on Enter path too.
			if cfg.MinWordLength > 0 && len([]rune(word)) < cfg.MinWordLength {
				return false
			}
			if looksLikeContext(word) {
				return false
			}
			app := FrontmostAppID()
			if cfg.IsAppExcluded(app) {
				return false
			}
			if store != nil && store.IsException(app, word) {
				log.Printf("Exception skip (enter): %q in %q", word, app)
				return false
			}

			wrong, corrected := detector.Check(word)
			if !wrong {
				return false
			}

			go func() {
				log.Printf("Fix (enter): %q → %q", word, corrected)
				atomic.StoreInt32(&replacing, 1)
				buf.Clear()

				wordRunes := []rune(word)
				for i := 0; i < len(wordRunes); i++ {
					sendBackspaceKey()
					time.Sleep(5 * time.Millisecond)
				}
				time.Sleep(10 * time.Millisecond)

				newText := corrected
				for _, ch := range corrected {
					sendChar(ch)
					time.Sleep(5 * time.Millisecond)
				}

				undo.Save(word, newText)
				if tracker != nil {
					tracker.OnConversion(word, newText, FrontmostAppID())
				}

				switchLang()
				time.Sleep(30 * time.Millisecond)
				atomic.StoreInt32(&replacing, 0)

				time.Sleep(10 * time.Millisecond)
				sendEnter()
			}()
			return true
		}

		// Regular char — any real keystroke invalidates the
		// convert_last_word toggle window. The earlier guard skips
		// modifier-only events (char == 0) before reaching here, so this
		// only fires for actual typed characters.
		lastWord.Reset()
		buf.Add(char)
		if tracker != nil {
			res := tracker.ObserveKey(KeyObservation{Kind: KeyKindChar, Rune: char})
			if res.RollbackDetected {
				log.Printf("Learned exception (retype): %q in %q", res.Word, res.App)
			}
		}
		return false
	}

	// Start keyboard hook
	err = startHook()
	if err != nil {
		log.Fatalf("Hook error: %v", err)
	}
	log.Println("Keyboard hook started")

	// Sync the in-memory auto-convert flag with the persisted config value
	// before the tray (and thus the menu checkmark) is created.
	setAutoConvertEnabled(cfg.AutoConvert)

	// Start tray icon
	startTray()

	// Install NSWorkspace observer for thread-safe frontmost app detection
	// (must be called after startTray() initializes NSApplication).
	installFrontmostObserver()

	log.Println("RuSwitch ready")

	// Handle signals in background
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("Shutting down")
		os.Exit(0)
	}()

	// Run NSApp loop on main thread
	runAppLoop()
}
