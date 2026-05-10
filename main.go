package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
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

// USB HID usage codes used by `hidutil property --set`.
const (
	hidUsageCapsLock uint64 = 0x700000039
	hidUsageF18      uint64 = 0x70000006D
)

// hidutil lifecycle state. We remember the user's pre-existing
// `UserKeyMapping` so shutdown can restore it verbatim. `capsLockRemapped`
// is an int32 (not bool) so atomic CAS can guarantee `restoreCapsLock` runs
// at most once even when called concurrently from multiple shutdown paths
// (`defer` on main, the signal-handler goroutine, the tray-quit goroutine,
// and the fatal-error fast path before `log.Fatalf`). Without atomic CAS
// `go test -race` would flag this — and a double `hidutil --set` would race
// with the user's other key mappings.
//
// `originalUserKeyMapping` is only mutated inside `remapCapsLockToF18` —
// which runs once on the main goroutine before any other goroutine is
// spawned — and only read inside `restoreCapsLock`. After the CAS in
// `restoreCapsLock` claims exclusivity, this read is safe.
var (
	originalUserKeyMapping []map[string]uint64
	capsLockRemapped       int32 // 0 = no remap installed, 1 = installed
)

// isCapsLockSrc returns true when v is the HID usage code for Caps Lock.
func isCapsLockSrc(v uint64) bool { return v == hidUsageCapsLock }

// parseHidutilUserKeyMapping parses the property-list-style output produced
// by `hidutil property --get "UserKeyMapping"`. Each entry is a dictionary
// block containing two integer fields, `HIDKeyboardModifierMappingSrc` and
// `HIDKeyboardModifierMappingDst`. macOS prints these in either decimal
// (e.g., `30064771129`) or hex (`0x700000039`) depending on the macOS
// version, so both forms are accepted (see `parseHidutilNumber`).
//
// The function is forgiving — individually malformed blocks are skipped
// rather than aborting the whole parse. If parsing returns zero entries
// from non-empty output the caller refuses to install the remap so a
// future format change does not cause shutdown to wipe the user's mapping.
func parseHidutilUserKeyMapping(output string) []map[string]uint64 {
	// Capture group accepts both `0x...` (case-insensitive) hex and plain
	// decimal forms. macOS variants of hidutil print one or the other
	// depending on the platform/version.
	srcRe := regexp.MustCompile(`HIDKeyboardModifierMappingSrc\s*=\s*(0[xX][0-9a-fA-F]+|\d+)`)
	dstRe := regexp.MustCompile(`HIDKeyboardModifierMappingDst\s*=\s*(0[xX][0-9a-fA-F]+|\d+)`)

	// Split on '}' so each block contains at most one Src/Dst pair.
	blocks := strings.Split(output, "}")
	var entries []map[string]uint64
	for _, b := range blocks {
		sm := srcRe.FindStringSubmatch(b)
		dm := dstRe.FindStringSubmatch(b)
		if sm == nil || dm == nil {
			continue
		}
		src, err1 := parseHidutilNumber(sm[1])
		dst, err2 := parseHidutilNumber(dm[1])
		if err1 != nil || err2 != nil {
			continue
		}
		entries = append(entries, map[string]uint64{
			"HIDKeyboardModifierMappingSrc": src,
			"HIDKeyboardModifierMappingDst": dst,
		})
	}
	return entries
}

// parseHidutilNumber turns `"30064771129"` or `"0x700000039"` into a uint64.
// Explicit base selection avoids `ParseUint(_, 0, _)`'s "leading 0 →
// octal" surprise, which would silently mis-parse a future hidutil
// formatting change.
func parseHidutilNumber(s string) (uint64, error) {
	if len(s) > 2 && (s[:2] == "0x" || s[:2] == "0X") {
		return strconv.ParseUint(s[2:], 16, 64)
	}
	return strconv.ParseUint(s, 10, 64)
}

// looksLikeEmptyHidutilList returns true when `output` is one of the known
// "no entries" shapes that `hidutil property --get UserKeyMapping` can
// emit: `()`, `(\n)`, `(null)`, or any of these wrapped in arbitrary
// whitespace. Used to distinguish a legitimate empty list from an output
// shape we failed to parse (which should disable the remap rather than
// risk wiping the user's mapping).
func looksLikeEmptyHidutilList(output string) bool {
	t := strings.TrimSpace(output)
	if t == "" {
		return true
	}
	// Strip a single outer "(" / ")" wrapper plus any whitespace inside.
	if strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
		inner := strings.TrimSpace(t[1 : len(t)-1])
		if inner == "" || strings.EqualFold(inner, "null") {
			return true
		}
	}
	return false
}

// encodeUserKeyMappingPayload builds the JSON payload that `hidutil property
// --set` expects. Each map is emitted with integer values so hidutil
// interprets them as numbers.
func encodeUserKeyMappingPayload(entries []map[string]uint64) (string, error) {
	if entries == nil {
		entries = []map[string]uint64{}
	}
	payload := map[string]interface{}{
		"UserKeyMapping": entries,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// remapCapsLockToF18 installs an HID-level remap from Caps Lock (USB HID
// usage 0x700000039) to F18 (0x70000006D). Pre-existing entries in the
// system's `UserKeyMapping` are preserved — we read them first, drop any
// stale Caps Lock entry, append our own, and write the merged list back.
//
// Returns nil and logs a warning when `hidutil` is not on PATH so the rest
// of the app keeps running. Returns the underlying error only on a real
// invocation failure (e.g., permission denied).
func remapCapsLockToF18() error {
	if _, err := exec.LookPath("hidutil"); err != nil {
		log.Printf("WARN [hidutil] not found on PATH (%v) — Caps Lock hotkey disabled", err)
		return nil
	}

	// Step 1: read the existing UserKeyMapping so we can merge instead of clobber.
	// We compute the snapshot in a local first and only commit it to package
	// state once `--set` succeeds — that way a failed install leaves
	// `originalUserKeyMapping` untouched (and `restoreCapsLock` is a no-op).
	getCmd := exec.Command("hidutil", "property", "--get", "UserKeyMapping")
	getOut, getErr := getCmd.Output()

	var snapshot []map[string]uint64
	switch {
	case getErr != nil:
		// `hidutil` is on PATH but the get call failed (e.g., the property
		// hasn't been set yet). Fall back to "no prior mapping" — `snapshot`
		// stays nil and we still attempt the install below.
		var notFound *exec.Error
		if errors.As(getErr, &notFound) {
			log.Printf("WARN [hidutil] not runnable (%v) — Caps Lock hotkey disabled", getErr)
			return nil
		}
		log.Printf("WARN [hidutil] --get UserKeyMapping failed: %v (treating as empty)", getErr)
	default:
		snapshot = parseHidutilUserKeyMapping(string(getOut))
		// Guard against a parser breakage on a future macOS version
		// silently dropping the user's mapping at restore time. If we
		// extracted zero entries from output that clearly isn't an empty
		// list, refuse to install the remap — otherwise shutdown would
		// wipe the user's UserKeyMapping clean. We treat as "empty" the
		// known shapes printed by macOS variants: `()`, `(\n)`, with any
		// surrounding whitespace, plus `(null)`.
		if len(snapshot) == 0 && !looksLikeEmptyHidutilList(string(getOut)) {
			log.Printf("WARN [hidutil] could not parse any UserKeyMapping entries from output (%q); "+
				"refusing to install remap so we don't clobber user mappings on shutdown",
				strings.TrimSpace(string(getOut)))
			return nil
		}
		vlog("[hidutil] read existing UserKeyMapping: %d entr(y|ies)", len(snapshot))
	}

	// Step 2: build the merged list. Drop any pre-existing Caps Lock entry
	// (we'll override it) and append ours.
	merged := make([]map[string]uint64, 0, len(snapshot)+1)
	for _, e := range snapshot {
		if isCapsLockSrc(e["HIDKeyboardModifierMappingSrc"]) {
			continue
		}
		merged = append(merged, e)
	}
	merged = append(merged, map[string]uint64{
		"HIDKeyboardModifierMappingSrc": hidUsageCapsLock,
		"HIDKeyboardModifierMappingDst": hidUsageF18,
	})

	payload, err := encodeUserKeyMappingPayload(merged)
	if err != nil {
		return fmt.Errorf("[hidutil] encode payload: %w", err)
	}

	// Step 3: install the remap.
	setCmd := exec.Command("hidutil", "property", "--set", payload)
	if out, err := setCmd.CombinedOutput(); err != nil {
		log.Printf("WARN [hidutil] remap failed: %v (output: %s)", err, strings.TrimSpace(string(out)))
		return err
	}

	// Commit the snapshot to package state ONLY now that `--set` succeeded.
	// Atomic store of `capsLockRemapped` makes the `restoreCapsLock` CAS
	// happen-before-correct: any goroutine that observes `capsLockRemapped
	// == 1` is guaranteed to also see the assignment to
	// `originalUserKeyMapping` made on this line.
	originalUserKeyMapping = snapshot
	atomic.StoreInt32(&capsLockRemapped, 1)
	vlog("[hidutil] CapsLock → F18 remap installed; merged payload: %s", payload)
	log.Printf("[hidutil] Caps Lock remapped to F18. To reset manually run: "+
		"hidutil property --set '{\"UserKeyMapping\":[]}' (or restore: %s)",
		describeMappingForLog(originalUserKeyMapping))
	return nil
}

// describeMappingForLog renders the saved mapping in a human-readable shape
// for the startup log so a user can recover manually after a crash.
func describeMappingForLog(entries []map[string]uint64) string {
	if len(entries) == 0 {
		return "no prior entries"
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf("0x%x→0x%x",
			e["HIDKeyboardModifierMappingSrc"], e["HIDKeyboardModifierMappingDst"]))
	}
	return strings.Join(parts, ", ")
}

// restoreCapsLock undoes the remap installed by remapCapsLockToF18. Safe
// to call concurrently from any combination of: the `defer` in `main`,
// the SIGINT/SIGTERM handler goroutine, the tray-Quit Cocoa callback, and
// the fatal-error fast path before `log.Fatalf`. The atomic CAS makes the
// hidutil `--set` run at most once even under contention.
//
// If the user had pre-existing entries, those are written back verbatim.
// If they had none, we clear the list. Logs but never panics — runs from
// shutdown paths where panicking would be catastrophic.
func restoreCapsLock() {
	// Claim exclusivity: only the first caller proceeds; subsequent calls
	// are no-ops. This pairs with `atomic.StoreInt32(&capsLockRemapped, 1)`
	// in remapCapsLockToF18, so a successful CAS here is also guaranteed
	// to see the originalUserKeyMapping assignment from that function.
	if !atomic.CompareAndSwapInt32(&capsLockRemapped, 1, 0) {
		return
	}

	payload, err := encodeUserKeyMappingPayload(originalUserKeyMapping)
	if err != nil {
		log.Printf("WARN [hidutil] restore: encode payload failed: %v", err)
		return
	}

	cmd := exec.Command("hidutil", "property", "--set", payload)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("WARN [hidutil] restore failed: %v (output: %s)", err, strings.TrimSpace(string(out)))
		return
	}
	vlog("[hidutil] CapsLock mapping restored: %s", payload)
}

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

// finishSelectionConvert: convert an already-copied selection and paste it
// back. Caller is responsible for: saving the original clipboard, raising the
// `replacing` flag, sending Cmd+C, and reading `selected` from the clipboard.
// This function takes over from "we know what's selected" and runs the
// convert + paste + restore-clipboard + switchLang + clear-replacing tail.
//
// Splitting the flow this way lets handleCapsLock reuse the single Cmd+C it
// already sends to detect "selection vs. no selection" instead of double-
// copying.
func finishSelectionConvert(detector *Detector, savedClipboard, selected string) {
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

// convertLastWordFromBuffer reads the last typed word from buf (which may be
// either the in-progress word or the last flushed word if a space/boundary
// was already typed) and replaces it with the layout-converted version. Arms
// the toggle-back window so a subsequent Caps Lock press without intervening
// real keystrokes reverts the conversion.
//
// Called from handleCapsLock when no selection was detected.
func convertLastWordFromBuffer(buf *Buffer) {
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
			// Toggling back ends the toggle window — a subsequent press must
			// re-convert from the buffer, not bounce.
			lastWord.Reset()
			atomic.StoreInt32(&replacing, 0)
			log.Printf("convert_last_word: revert %q → %q", convPrev, origPrev)
		}()
		return
	}

	// Fresh-convert path.
	if current == "" {
		log.Printf("convert_last_word: empty buffer, no-op")
		return
	}

	// If the current buffer is empty, LastWord() returned the last flushed
	// word — meaning a space/boundary was already typed after it. We need to
	// delete that trailing space too.
	isFlushed := buf.IsBufferEmpty()

	// Direction heuristic mirrors finishSelectionConvert: any Cyrillic → assume
	// RU was typed in QWERTY layout, convert RU→QWERTY; otherwise QWERTY→RU.
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
		// Arm the toggle window. The next Caps Lock press (with no real
		// keystroke in between) will revert this.
		lastWord.Set(current, converted)
		atomic.StoreInt32(&replacing, 0)
		log.Printf("convert_last_word: %q → %q (flushed=%v)", current, converted, isFlushed)
	}()
}

// handleCapsLock is the single Caps Lock entry point. Priority order:
//
//  1. If the toggle-back window is armed → revert (last-word path).
//  2. If the user is mid-word (buffer.chars > 0) → convert last word
//     directly. No Cmd+C needed because you can't have a selection while
//     actively typing.
//  3. Otherwise (buffer empty, user finished typing or is selecting) →
//     probe for selection via Cmd+C. If found → convert selection.
//     If not found → fall back to converting the last flushed word.
//
// This ordering avoids sending Cmd+C when it's unnecessary (mid-typing),
// which prevents side effects in apps that react to Cmd+C without a
// selection (search bars, line-copy in IDEs, etc.).
//
// Runs in its own goroutine (spawned from onKeyEvent) because it involves
// keystroke synthesis and clipboard I/O that must not block the event tap.
func handleCapsLock(buf *Buffer, detector *Detector) {
	// Fast path 1: toggle-back is armed — always last-word path.
	_, _, toggleActive := lastWord.Snapshot()
	if toggleActive {
		convertLastWordFromBuffer(buf)
		return
	}

	// Fast path 2: user is mid-word (actively typing). A text selection
	// can't coexist with an active caret in the same text field, so skip
	// the clipboard probe.
	if !buf.IsBufferEmpty() {
		convertLastWordFromBuffer(buf)
		return
	}

	// Slow path: buffer is empty (user finished typing, or moved cursor/
	// selected text). Probe for a selection via Cmd+C → clipboard diff.
	savedClipboard := readClipboard()

	atomic.StoreInt32(&replacing, 1)
	sendCopy()
	time.Sleep(100 * time.Millisecond)

	selected := readClipboard()

	if selected != "" && selected != savedClipboard {
		// Selection found — convert it.
		finishSelectionConvert(detector, savedClipboard, selected)
		return
	}

	// No selection found. Restore clipboard, then fall back to converting
	// the last flushed word (if any).
	atomic.StoreInt32(&replacing, 0)
	if savedClipboard != "" {
		writeClipboard(savedClipboard)
	}
	convertLastWordFromBuffer(buf)
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

	// Set up key event handler — called synchronously from CGEventTap.
	// Caps Lock is remapped to F18 at HID level by `remapCapsLockToF18`, so
	// the event tap sees it as a normal kCGEventKeyDown with virtual keycode
	// 0x4F. Autorepeat F18 events are filtered in the C-side hook before
	// reaching us; the `flags == 0` guard below makes Cmd+CapsLock,
	// Shift+CapsLock, etc. pass through to the OS unchanged.
	onKeyEvent = func(keycode uint16, char rune, flags int64) bool {
		// Caps Lock (remapped to F18) is the single hardcoded conversion
		// hotkey. Checked BEFORE the cfg.Enabled / auto-convert gate so the
		// manual hotkey works even when auto-convert is off and even in
		// excluded apps. `flags & anyRealModifierMask` ensures Cmd+CapsLock
		// and friends pass through to the system normally.
		if keycode == f18KeyCode && (flags&anyRealModifierMask) == 0 {
			vlog("[hook] F18 keyDown received, flags=0x%x", flags)
			go handleCapsLock(buf, detector)
			return true // suppress so no other app sees F18
		}

		// --- Buffer accumulation: ALWAYS runs regardless of auto-convert ---
		// The buffer must track keystrokes even when auto-convert is off so
		// that the Caps Lock manual conversion (which is gated separately
		// above) has something to work with.

		// Backspace
		if keycode == macBackspace {
			lastWord.Reset()
			buf.Backspace()
			if tracker != nil {
				tracker.ObserveKey(KeyObservation{Kind: KeyKindBackspace})
			}
			return false
		}

		// Skip null chars and modifier-only events
		if char == 0 || char == 0x08 {
			return false
		}

		// Enter/Return flushes the buffer (so LastWord can find the word).
		if keycode == macReturn || keycode == macEnter || char == '\r' || char == '\n' {
			word := buf.FlushWord()

			// Auto-correction on Enter: only when enabled.
			if word != "" && cfg.Enabled && isAutoConvertEnabled() {
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

			if tracker != nil {
				tracker.ObserveKey(KeyObservation{Kind: KeyKindOther})
			}
			return false
		}

		// Command-modified keys (Cmd+C, Cmd+V, Cmd+Z, etc.) must NOT
		// accumulate in the buffer — they are shortcuts, not typed text.
		if (flags & kCGEventFlagMaskCommand) != 0 {
			// Cmd+Z — undo last replacement (only when auto-convert is active)
			if cfg.Enabled && isAutoConvertEnabled() && keycode == macZ {
				original, replaced, ok := undo.Get()
				if !ok {
					return false
				}
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
					for i := 0; i < len([]rune(replaced)); i++ {
						sendBackspaceKey()
						time.Sleep(5 * time.Millisecond)
					}
					time.Sleep(10 * time.Millisecond)
					for _, ch := range original {
						sendChar(ch)
						time.Sleep(5 * time.Millisecond)
					}
					switchLang()
					time.Sleep(30 * time.Millisecond)
					atomic.StoreInt32(&replacing, 0)
				}()
				return true
			}
			// All other Cmd+key combos: pass through without touching buffer.
			return false
		}

		// Regular char (no Command modifier) — accumulate in buffer.
		lastWord.Reset()
		buf.Add(char)
		if tracker != nil {
			res := tracker.ObserveKey(KeyObservation{Kind: KeyKindChar, Rune: char})
			if res.RollbackDetected {
				log.Printf("Learned exception (retype): %q in %q", res.Word, res.App)
			}
		}

		// Clear undo window on real keystrokes
		if cfg.Enabled && isAutoConvertEnabled() {
			undo.mu.Lock()
			undo.original = ""
			undo.mu.Unlock()
		}

		return false
	}

	// Install the HID-level Caps Lock → F18 remap before starting the hook.
	// Failures are non-fatal: the app still runs, the F18 path simply never
	// fires. `defer` covers the normal-return shutdown path; the signal
	// handler below covers Ctrl+C / SIGTERM (os.Exit skips defers).
	if err := remapCapsLockToF18(); err != nil {
		log.Printf("[hidutil] continuing without Caps Lock remap: %v", err)
	}
	defer restoreCapsLock()

	// Start keyboard hook
	err = startHook()
	if err != nil {
		// log.Fatalf calls os.Exit, which skips deferred functions —
		// restore the Caps Lock mapping explicitly before bailing.
		restoreCapsLock()
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

	// Handle signals in background. os.Exit skips deferred functions, so we
	// MUST call restoreCapsLock here explicitly — otherwise Ctrl+C leaves
	// the user's Caps Lock remapped to F18 until reboot. restoreCapsLock is
	// idempotent so the deferred call in the normal-return path is harmless.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		s := <-sig
		log.Printf("Shutting down (signal=%v) — restoring Caps Lock", s)
		restoreCapsLock()
		os.Exit(0)
	}()

	// Run NSApp loop on main thread
	runAppLoop()
}
