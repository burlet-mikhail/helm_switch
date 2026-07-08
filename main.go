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
	"unicode"
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

// capsHandling is a single-flight guard for handleCapsLock: 1 while a Caps Lock
// conversion is in progress, 0 otherwise. Prevents overlapping presses from
// interleaving keystroke synthesis.
//
// Note: the manual hotkey no longer keeps a "toggle-back" state machine. Layout
// conversion is a bijection, so pressing Caps Lock again simply re-converts the
// word at the caret straight back to the original — the revert is automatic.
var capsHandling int32

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

// containsCyrillic reports whether s holds at least one Cyrillic letter. Used
// as the direction heuristic for conversions: any Cyrillic → the text was typed
// in QWERTY while RU was active, so convert RU→QWERTY; otherwise QWERTY→RU.
func containsCyrillic(s string) bool {
	for _, r := range s {
		if (r >= 'а' && r <= 'я') || (r >= 'А' && r <= 'Я') || r == 'ё' || r == 'Ё' {
			return true
		}
	}
	return false
}

// convertByHeuristic converts s in whichever direction containsCyrillic implies.
func convertByHeuristic(s string) string {
	if containsCyrillic(s) {
		return RussianToQWERTY(s)
	}
	return QWERTYToRussian(s)
}

// searchAppBundleIDs are apps whose text fields show inline autocomplete that
// must be defeated with extra backspaces during a last-word conversion.
var searchAppBundleIDs = map[string]bool{
	"com.apple.systempreferences":   true,
	"com.apple.Spotlight":           true,
	"com.raycast.macos":             true,
	"com.runningwithcrayons.Alfred": true,
}

// isSearchApp reports whether app is one of the inline-autocomplete search apps.
func isSearchApp(app string) bool { return searchAppBundleIDs[app] }

// isNavigationKey reports whether keycode is an arrow / Home / End / Page key.
// These move the caret or extend a selection, so the typing buffer no longer
// reflects what is at the cursor and must be invalidated.
func isNavigationKey(keycode uint16) bool {
	switch keycode {
	case 0x7B, 0x7C, 0x7D, 0x7E, // ← → ↓ ↑
		0x73, 0x77, // Home, End
		0x74, 0x79: // Page Up, Page Down
		return true
	}
	return false
}

// handleCapsLock is the single Caps Lock entry point. It reads what is ACTUALLY
// at the cursor via the Accessibility API instead of trusting the keystroke
// buffer, so it behaves correctly even after the user clicks, moves the caret,
// or selects with the mouse:
//
//  1. If there is a real selection (read via AX) → convert it in place.
//  2. Otherwise select the word left of the caret (Option+Shift+Left), read the
//     real selected text, and convert that.
//
// Because layout conversion is a bijection, pressing Caps Lock again simply
// re-converts the word back — no explicit toggle/undo state is needed.
//
// Runs in its own goroutine (spawned from onKeyEvent) because it synthesises
// keystrokes and must not block the event tap.
func handleCapsLock(buf *Buffer) {
	// Single-flight: a press while a previous conversion is still running would
	// interleave keystroke synthesis and corrupt both. Drop the overlapping
	// press. (The C-side autorepeat filter already drops held-key repeats; this
	// guards genuine rapid double-presses.)
	if !atomic.CompareAndSwapInt32(&capsHandling, 0, 1) {
		vlog("[caps] handler already running, dropping press")
		return
	}
	defer atomic.StoreInt32(&capsHandling, 0)

	// Snapshot the typing buffer BEFORE we clear it — the JetBrains terminal
	// fallback below has no other way to know what the user just typed.
	// bufferedFlushed is true when the word was already terminated by a
	// non-punctuation boundary (usually a space), so the caret sits AFTER that
	// boundary. The terminal fallback needs this to send one extra backspace
	// and retype the trailing space.
	bufferedWord := buf.LastWord()
	bufferedFlushed := buf.IsBufferEmpty()

	// Invoking the manual hotkey rewrites text at the caret, so the typing
	// buffer no longer matches the document. Clear it so auto-convert-on-space
	// doesn't later act on a stale word.
	buf.Clear()

	// Suppress the buffer/auto-convert machinery for the whole operation so our
	// own synthetic keystrokes (Option+Shift+Left, paste) don't re-enter.
	atomic.StoreInt32(&replacing, 1)
	defer atomic.StoreInt32(&replacing, 0)

	// --- Step 1: convert an EXISTING selection, if there is one. -------------
	// We must establish whether a selection exists BEFORE doing any
	// Option+Shift+Left, because that gesture shrinks an existing selection by a
	// word instead of selecting a fresh one (the "converted all but the last
	// word" bug). axSelectionLength tells us reliably even when the app won't
	// hand us the selected string.
	selLen := axSelectionLength()
	app := FrontmostAppID()
	role, subrole, desc, id := axFocusedInfo()
	log.Printf("[caps] start app=%q selLen=%d role=%q subrole=%q desc=%q id=%q",
		app, selLen, role, subrole, desc, id)

	// JetBrains IDEs (IntelliJ, PhpStorm, GoLand, …) embed JediTerm as their
	// terminal panel and expose it through AX as a plain "AXTextArea" — while
	// their actual code editor and plugin fields use the "JavaAxIgnore" role.
	// Sending Option+Shift+Left into JediTerm doesn't select a word: the OS
	// forwards it as ^[[1;4D, which the shell prints as "[D" garbage. Use the
	// typing buffer instead — what the user just typed is exactly what we want
	// to convert — and overwrite it by sending backspaces + the corrected text.
	if strings.HasPrefix(app, "com.jetbrains.") && role == "AXTextArea" {
		convertLastWordFromBuffer(bufferedWord, bufferedFlushed, "JetBrains terminal panel")
		return
	}

	if sel := axSelectedText(); sel != "" {
		// Native app exposed the selection directly.
		log.Printf("[caps] AX read direct selection: %q", sel)
		convertSelectionInPlace(sel)
		return
	}
	// Probe the clipboard for an existing selection. We do this even when
	// selLen == 0 because "JavaAxIgnore" roles (JetBrains plugin fields) hide
	// selections entirely: selLen reads 0 and axSelectedText is empty even
	// during a real selection. Falling straight into Step 2's
	// Option+Shift+Left would then EXTEND the existing selection into the
	// previous word instead of converting only what the user selected.
	// changeCount-based copy detection makes this safe: if there's no
	// selection, Cmd+C changes nothing and we simply move on. Skip it only
	// when AX is known unavailable (selLen == -1) — the same guard as before.
	if selLen >= 0 {
		sel := copySelectionViaClipboard()
		log.Printf("[caps] clipboard probe (selLen=%d) returned %q", selLen, sel)
		if sel != "" {
			convertSelectionInPlace(sel)
			return
		}
	}

	// --- Step 2: no selection — convert the token left of the caret. ---------
	// Safe to select now: we've confirmed there is no selection to disturb.
	var word string
	if selLen == 0 {
		// AX read works here → grow the selection over the FULL whitespace-
		// delimited token (so layout punctuation inside a word, e.g. the comma
		// in "hf,jnfkj", doesn't split it). Convert whatever ends up selected.
		selectTokenLeftOfCaret()
		word = axSelectedText()
		log.Printf("[caps] AX path selected: %q", word)
		// JetBrains IDEs (IntelliJ, PhpStorm, etc.) expose
		// kAXSelectedTextRangeAttribute (so selLen reads 0 correctly) but
		// refuse to return the selected text through kAXSelectedText — even
		// after we genuinely extend the selection. Fall back to the clipboard,
		// which DOES see the real selection.
		if strings.TrimSpace(word) == "" {
			if clip := copySelectionViaClipboard(); clip != "" {
				word = clip
				log.Printf("[caps] AX path empty, clipboard fallback returned: %q", word)
			}
		}
	} else {
		// No AX read (browser/Electron): fall back to a single word selection.
		// Layout-punctuation-containing tokens may be only partly converted here.
		sendOptionShiftLeft()
		time.Sleep(40 * time.Millisecond)
		word = copySelectionViaClipboard()
		log.Printf("[caps] non-AX path (Opt+Shift+Left) selected: %q", word)
	}

	if strings.TrimSpace(word) == "" {
		// Nothing convertible at the caret (empty field, leading whitespace,
		// punctuation). Leave the harmless selection as-is.
		log.Printf("[caps] no convertible word at caret, no-op")
		return
	}
	convertSelectionInPlace(word)
}

// convertLastWordFromBuffer handles the Caps Lock hotkey in apps where
// selection-via-keystroke is unsafe (e.g. JetBrains' JediTerm terminal panel,
// where Option+Shift+Left becomes a shell escape sequence). We don't try to
// read the document — we use the typing buffer the hook has been accumulating.
// The buffer reflects what the user just typed, so we delete that many
// characters with backspace and retype the converted text.
//
// Caveat: if the user clicked away or moved the caret after typing, the buffer
// no longer matches what's at the cursor. That's the same trade-off as the
// auto-convert path and acceptable for a manual hotkey.
func convertLastWordFromBuffer(word string, hasTrailingSpace bool, reasonForLog string) {
	if strings.TrimSpace(word) == "" {
		log.Printf("[caps] %s: empty typing buffer, no-op", reasonForLog)
		return
	}
	converted := convertByHeuristic(word)
	if converted == word {
		log.Printf("[caps] %s: %q unchanged, skipping", reasonForLog, word)
		return
	}

	backspaces := len([]rune(word))
	if hasTrailingSpace {
		// The word was already flushed by a space (or another non-punctuation
		// boundary), so the caret sits AFTER that boundary char. Eat it too,
		// then retype it after the converted word so we don't shift the line.
		backspaces++
	}
	for i := 0; i < backspaces; i++ {
		sendBackspaceKey()
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	for _, ch := range converted {
		sendChar(ch)
		time.Sleep(5 * time.Millisecond)
	}
	if hasTrailingSpace {
		sendChar(' ')
		time.Sleep(5 * time.Millisecond)
	}
	switchLang()
	time.Sleep(30 * time.Millisecond)
	log.Printf("[caps] %s: %q → %q (trailingSpace=%v)", reasonForLog, word, converted, hasTrailingSpace)
}

// selectTokenLeftOfCaret grows the selection leftward over a whole
// whitespace-delimited token, starting from the caret. Plain Option+Shift+Left
// ("select previous word") stops at punctuation, which splits layout-converted
// words like "hf,jnfkj" (the comma is б). We keep extending word-by-word while
// each new chunk is glued to the previous one (no whitespace between them), and
// stop — retracting one step — as soon as an extension crosses real whitespace.
//
// Requires a working AX selected-text read (caller gates on axSelectionLength).
// Leaves the selection in place for the caller to read and convert.
func selectTokenLeftOfCaret() {
	prev := ""
	for i := 0; i < 16; i++ {
		sendOptionShiftLeft()
		time.Sleep(15 * time.Millisecond)
		sel := axSelectedText()
		if sel == prev {
			break // selection stopped growing → reached start of field/line
		}
		if prev != "" && strings.HasSuffix(sel, prev) {
			prefix := sel[:len(sel)-len(prev)]
			if strings.TrimRightFunc(prefix, unicode.IsSpace) != prefix {
				// The newly added chunk is separated from the token by
				// whitespace → we crossed a word boundary. Undo this extension.
				sendOptionShiftRight()
				time.Sleep(15 * time.Millisecond)
				break
			}
		}
		prev = sel
	}
}

// convertSelectionInPlace converts `selected` and writes the result back over
// the current selection by pasting. The caller guarantees a real selection
// exists (either pre-existing, or just made via Option+Shift+Left), so the
// paste replaces it rather than inserting.
//
// NOTE: we deliberately do NOT use AXUIElementSetAttributeValue(kAXSelectedText)
// here — many apps (browsers, Electron, some native fields) return success from
// that call without actually mutating the text, which silently dropped the
// conversion ("selects but doesn't translate"). Pasting over the selection
// works uniformly across apps.
func convertSelectionInPlace(selected string) {
	converted := convertByHeuristic(selected)
	if converted == selected {
		// No convertible characters — don't disturb the user's text/selection.
		log.Printf("[caps] %q unchanged, skipping", selected)
		return
	}

	pasteOverSelection(converted)
	switchLang()
	time.Sleep(30 * time.Millisecond)
	log.Printf("[caps] convert: %q → %q", selected, converted)
}

// pasteOverSelection replaces the current selection with text via the clipboard,
// restoring the previous clipboard contents afterwards. Safe only when a real
// selection exists (otherwise it inserts at the caret).
func pasteOverSelection(text string) {
	saved := readClipboard()
	writeClipboard(text)
	time.Sleep(30 * time.Millisecond)
	sendPaste()
	// Wait for the app to consume the paste before restoring the clipboard;
	// restoring too early makes the app paste the OLD contents instead.
	time.Sleep(200 * time.Millisecond)
	writeClipboard(saved)
}

// copySelectionViaClipboard copies the current selection (Cmd+C) and returns it,
// detecting the copy via the pasteboard changeCount and restoring the previous
// clipboard. Used as an AX-read fallback right after we have created a real
// selection, so there is no "empty Cmd+C copies the line" false positive.
func copySelectionViaClipboard() string {
	saved := readClipboard()
	before := clipboardChangeCount()
	sendCopy()
	// Poll up to ~400ms for the copy to land. Long enough for slow Electron
	// fields (Discord, Slack, some browser inputs) to report a real selection,
	// short enough not to stall noticeably when Cmd+C copies nothing.
	for i := 0; i < 40; i++ {
		time.Sleep(10 * time.Millisecond)
		if clipboardChangeCount() != before {
			selected := readClipboard()
			writeClipboard(saved) // restore — we only wanted to read
			return selected
		}
	}
	return ""
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

	log.Println("Helm Switch starting...")

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
		log.Printf("FrontmostAppID=%q", app)
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
			go handleCapsLock(buf)
			return true // suppress so no other app sees F18
		}

		// --- Buffer accumulation feeds the auto-convert-on-boundary feature ---
		// (The Caps Lock manual hotkey reads the document via Accessibility and
		// no longer depends on this buffer.)

		// Backspace
		if keycode == macBackspace {
			buf.Backspace()
			if tracker != nil {
				tracker.ObserveKey(KeyObservation{Kind: KeyKindBackspace})
			}
			return false
		}

		// Navigation gestures (arrows, Home/End, Page Up/Down) move the caret,
		// so the typing buffer no longer reflects the word at the cursor.
		// Invalidate it so auto-convert doesn't fire across a cursor jump. Also
		// keeps arrow-key function chars out of the buffer.
		if isNavigationKey(keycode) {
			buf.Clear()
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
				log.Printf("FrontmostAppID=%q", app)
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

					deleteCount := len([]rune(word))
					if isSearchApp(FrontmostAppID()) {
						deleteCount += 5
					}

					for i := 0; i < deleteCount; i++ {
						sendBackspaceKey()
						time.Sleep(5 * time.Millisecond)
					}
					time.Sleep(20 * time.Millisecond)

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
					log.Printf("FrontmostAppID=%q", app)
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
			// All other Cmd+key combos (Cmd+A select-all, Cmd+←/→ navigation,
			// Cmd+C/V, …) change the selection or caret position, so the typing
			// buffer no longer reflects the word at the cursor. Invalidate it so
			// auto-convert doesn't fire across the change. Pass the shortcut
			// through to the app untouched.
			buf.Clear()
			return false
		}

		// Regular char (no Command modifier) — accumulate in buffer.
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

	// A mouse click repositions the caret or starts a new selection, so the
	// typing buffer no longer reflects the word at the cursor. Clear it so
	// auto-convert doesn't fire across the jump. Wired here because the C event
	// tap already reports left-mouse-down via goMouseCallback.
	onMouseEvent = func() {
		buf.Clear()
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

	log.Println("Helm Switch ready")

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
