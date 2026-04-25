<!-- handoff:task:e3e7eb4a-e7e1-4c9b-ac3f-0101ed7e6f1f -->
# RuSwitch: Dynamic Tray Flags + Configurable Hotkeys + Last Word Conversion

Branch: main
Mode: fast
Created: 2026-04-25

## Settings

- [x] Testing: no (do not add test-writing tasks)
- [x] Logging: verbose (use `log.Printf` for events; `vlog` for fine-grained traces consistent with `buffer.go`/`replacer_darwin.go`)
- [x] Docs: no (no doc tasks)

## Scope Summary

Implement three macOS-only features in the existing Go + Objective-C tray app:

1. Dynamic tray icon: 🇷🇺 when current keyboard input source is Russian, 🇺🇸 otherwise. Updates instantly via `NSDistributedNotificationCenter` observer for `NSTextInputContextKeyboardSelectionDidChangeNotification`.
2. Configurable hotkey to convert the last typed word (default `Cmd+Shift+Z`) using current `Buffer` contents; toggles back if pressed again with no buffer change.
3. Hotkeys are configurable via `~/Library/Application Support/RuSwitch/config.yaml` (`hotkeys.convert_selection`, `hotkeys.convert_last_word`).
4. Remove pause/resume concept; replace tray with "Автоконвертация" toggle backed by a new `autoConvertEnabled` atomic var.

Files to modify: `config.go`, `buffer.go`, `main.go`, `tray_darwin.go`, `tray_darwin.m`.
Files NOT modified: `detector.go`, `dict.go`, `keymap.go`, `hook_darwin.go`, `replacer_darwin.go`, `exceptions.go`, `rollback.go`, `appid_darwin.go`, `logging.go`, `types.go`.

Existing call sites that need attention:
- [x] `main.go:252` — buffer callback uses `isTrayEnabled()` (replaced with `isAutoConvertEnabled()`)
- [x] `main.go:302` — `onKeyEvent` early return uses `isTrayEnabled()` (replaced with `isAutoConvertEnabled()`)
- [x] `main.go:307-313` — hardcoded `Cmd+Shift+X` (`macX` + Cmd + Shift) for `convertSelectedText` (replaced with parsed `selectionHotkey`)
- [x] `main.go:21-31` — local consts `macX`, `macZ`, `kCGEventFlagMaskShift` (`macX` and `kCGEventFlagMaskShift` removed; `macZ` retained for the Cmd+Z undo branch; `macC`/`macV` also removed as unused)
- [x] `tray_darwin.go:23-56` — `trayEnabled`, `goTrayToggle`, `isTrayEnabled` (replaced with `autoConvertEnabled`/`goAutoConvertToggle`/`isAutoConvertEnabled`)
- [x] `tray_darwin.m:25-65` — `createTray`, `updateTray`, plus the duplicate menu setup inside `ensureApp` (collapsed into single `ensureApp` definition; old `createTray`/`updateTray` removed; new `updateAutoConvertMenu` added)

## Tasks

### Phase 1 — Config: parsed hotkeys + auto-convert flag

<!-- parallel: none (Tasks 1-4 modify same file sequentially) -->
- [x] **1. Extend `Config` in `config.go` with `HotkeyConfig` raw strings + `AutoConvert`.**
  - [ ] Add struct `HotkeyConfig` with fields `ConvertSelection string \`yaml:"convert_selection"\`` and `ConvertLastWord string \`yaml:"convert_last_word"\``.
  - [ ] Add field `Hotkeys HotkeyConfig \`yaml:"hotkeys"\`` to `Config`.
  - [ ] Add field `AutoConvert bool \`yaml:"auto_convert"\`` to `Config` (default `true` so existing users keep current behavior).
  - [ ] In `DefaultConfig()` set `AutoConvert: true` and `Hotkeys: HotkeyConfig{ConvertSelection: "Cmd+Shift+X", ConvertLastWord: "Cmd+Shift+Z"}`.
  - [ ] After `yaml.Unmarshal` in `LoadConfig`, fill empty `Hotkeys.ConvertSelection` / `Hotkeys.ConvertLastWord` with defaults (so old config files without the `hotkeys` block still get them).
  - [ ] Log via `log.Printf` when defaults are applied.

- [x] **2. Add parsed `Hotkey` type and `ParseHotkey` to `config.go`.**
  - [ ] Define `type Hotkey struct { KeyCode uint16; Modifiers int64 }`.
  - [ ] Define a private modifier map (string → CGEventFlags bitmask): `Cmd`/`Command`/`Cmd` → `1<<20`, `Shift` → `1<<17`, `Ctrl`/`Control` → `1<<18`, `Alt`/`Option`/`Opt` → `1<<19`. Match case-insensitively.
  - [ ] Define a private key map covering A–Z (macOS virtual keycodes: A=0x00, B=0x0B, C=0x08, D=0x02, E=0x0E, F=0x03, G=0x05, H=0x04, I=0x22, J=0x26, K=0x28, L=0x25, M=0x2E, N=0x2D, O=0x1F, P=0x23, Q=0x0C, R=0x0F, S=0x01, T=0x11, U=0x20, V=0x09, W=0x0D, X=0x07, Y=0x10, Z=0x06) plus 0–9 and basic specials (`Space`=0x31, `Return`/`Enter`=0x24).
  - [ ] `func ParseHotkey(spec string) (Hotkey, error)`:
    - [ ] Trim spaces, split on `+`.
    - [ ] Last token = key (case-insensitive lookup in key map).
    - [ ] Preceding tokens = modifiers OR’d into `Modifiers`.
    - [ ] Return `(Hotkey{}, error)` if any token is unknown or the spec is empty.

- [x] **3. Add helper `ParsedHotkeys()` on `*Config` that returns parsed hotkeys with defaults on error.**
  - [ ] `func (c *Config) ParsedHotkeys() (selection, lastWord Hotkey)`:
    - [ ] Try `ParseHotkey(c.Hotkeys.ConvertSelection)`. On error: `log.Printf("hotkey parse warning: convert_selection=%q invalid (%v); using default Cmd+Shift+X", spec, err)` and fall back to a hard-coded `Hotkey{KeyCode: 0x07, Modifiers: (1<<20)|(1<<17)}`.
    - [ ] Same pattern for `convert_last_word`, default `Hotkey{KeyCode: 0x06, Modifiers: (1<<20)|(1<<17)}`.

- [x] **4. Confirm config persistence still round-trips.**
  - [x] Verify `SaveConfig` already serializes new fields (yaml.Marshal handles them via struct tags — no code change needed beyond the struct edits in Task 1).

### Phase 2 — Buffer: expose last word

- [x] **5. Add `Buffer.LastWord() string` to `buffer.go`.**
  - [x] Method returns `string(b.chars)` under `b.mu` without mutating `b.chars`.
  - [x] Returns `""` when `len(b.chars) == 0`.
  - [x] Do NOT call `b.onWord`. Do NOT clear.

### Phase 3 — main.go: parsed hotkeys + last-word handler

- [x] **6. Wire parsed hotkeys into `main()` after `LoadConfig`.**
  - [ ] After the existing `cfg, err := LoadConfig()` block (around `main.go:207`), call `selectionHotkey, lastWordHotkey := cfg.ParsedHotkeys()`.
  - [ ] Log resolved hotkeys: `log.Printf("Hotkeys: convert_selection=%s convert_last_word=%s", cfg.Hotkeys.ConvertSelection, cfg.Hotkeys.ConvertLastWord)`.

- [x] **7. Add `lastWordConversion` state struct in `main.go`.**
  - [ ] Define near the top of `main.go` (next to `undoState`):
    ```go
    type lastWordState struct {
        mu        sync.Mutex
        original  string // word as the user typed it (pre-conversion)
        converted string // what we typed instead
        active    bool  // true if the next hotkey press should toggle back
    }
    var lastWord lastWordState
    ```
  - [ ] Add helper methods `Set(original, converted string)`, `Reset()`, `Snapshot() (original, converted string, active bool)`. All methods must lock `mu`.
  - [ ] Note on toggle invalidation: because the event tap is suppressed while `replacing == 1` (see `hook_darwin.go:79`), the synthetic backspaces and typed characters during conversion do NOT update `buf`. We therefore detect "buffer changed since conversion" by `Reset()`-ing `lastWord.active` from the regular-key and backspace paths in `onKeyEvent` (Task 10) — NOT by comparing `buf.LastWord()` to a snapshot.

- [x] **8. Replace hardcoded `Cmd+Shift+X` selection-hotkey check in `onKeyEvent`.**
  - [ ] In `main.go:307-313`, replace the literal `keycode == macX && Cmd && Shift` test with:
    ```go
    if keycode == selectionHotkey.KeyCode && (flags & selectionHotkey.Modifiers) == selectionHotkey.Modifiers {
        log.Printf("Manual convert hotkey (%s)", cfg.Hotkeys.ConvertSelection)
        go convertSelectedText(detector)
        return true
    }
    ```
  - [ ] Keep the `selectionHotkey` capture inside the `onKeyEvent` closure.
  - [ ] Note: the mask compare must use exact-match-of-required-bits (`flags & mods == mods`); do NOT require equality with the full flags word, because macOS sets device-dependent extra bits.

- [x] **9. Implement `convert_last_word` handler in `onKeyEvent`.**
  - [ ] Place the new branch BEFORE the `if !cfg.Enabled || !isAutoConvertEnabled()` early return at the top of `onKeyEvent`, so the hotkey works even when auto-convert is off.
  - [ ] Place it BEFORE the existing `Cmd+Z` undo handler so a Cmd+Shift+Z press is consumed here and never reaches the undo branch.
  - [ ] Match condition (using the parsed `lastWordHotkey` captured by the closure):
    - [ ] `keycode == lastWordHotkey.KeyCode`
    - [ ] `(flags & lastWordHotkey.Modifiers) == lastWordHotkey.Modifiers` (all required modifiers present)
    - [ ] AND `(flags & ^lastWordHotkey.Modifiers & (kCGEventFlagMaskCommand|kCGEventFlagMaskShift|(1<<18)|(1<<19))) == 0` — no extra modifier from {Cmd, Shift, Ctrl, Alt}, so plain `Cmd+Z` (no Shift) still falls through to the undo branch.
  - [ ] On match:
    1. Snapshot state under `lastWord.mu`: `origPrev, convPrev, active := lastWord.Snapshot()`. Take `current := buf.LastWord()`.
    2. **Toggle-back path** — if `active`:
       - [ ] Run the replacement goroutine using `convPrev` as "what is on screen" and `origPrev` as "what to type":
         - [ ] `atomic.StoreInt32(&replacing, 1)`
         - [ ] `buf.Clear()`
         - [ ] For each rune in `convPrev`: `sendBackspaceKey()`, sleep 5ms.
         - [ ] sleep 10ms.
         - [ ] For each rune in `origPrev`: `sendChar(ch)`, sleep 5ms.
         - [ ] `switchLang()`, sleep 30ms.
         - [ ] `lastWord.Reset()` — toggling-back ends the toggle window; a subsequent Cmd+Shift+Z must re-convert from the buffer.
         - [ ] `atomic.StoreInt32(&replacing, 0)`
         - [ ] `log.Printf("convert_last_word: revert %q → %q", convPrev, origPrev)`
       - [ ] Return `true` (suppress).
    3. **Fresh-convert path** — if `!active`:
       - [ ] If `current == ""`: `log.Printf("convert_last_word: empty buffer, no-op")` and return `true`.
       - [ ] Direction heuristic (mirror `convertSelectedText`):
         ```go
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
         ```
       - [ ] Goroutine (mirrors the Cmd+Z undo goroutine pattern):
         - [ ] `atomic.StoreInt32(&replacing, 1)`
         - [ ] `buf.Clear()`
         - [ ] For each rune in `current`: `sendBackspaceKey()`, sleep 5ms.
         - [ ] sleep 10ms.
         - [ ] For each rune in `converted`: `sendChar(ch)`, sleep 5ms.
         - [ ] `switchLang()`, sleep 30ms.
         - [ ] `lastWord.Set(current, converted)` (this also sets `active = true`).
         - [ ] `atomic.StoreInt32(&replacing, 0)`
         - [ ] `log.Printf("convert_last_word: %q → %q", current, converted)`
       - [ ] Return `true` (suppress).
  - [ ] Concurrency note: the synthetic keystrokes inside the goroutine are produced while `replacing == 1`, so `goKeyCallback` short-circuits (see `hook_darwin.go:79`) and `buf` does NOT receive them. This is exactly why we rely on Task 10's reset-on-real-keystroke to invalidate the toggle window, rather than comparing buffer snapshots.

- [x] **10. Reset `lastWord.active` on any other real keystroke.**
  - [ ] In `onKeyEvent`, on the regular-char path (around `main.go:443`, just before `buf.Add(char)`), call `lastWord.Reset()`.
  - [ ] Also call `lastWord.Reset()` inside the `if keycode == macBackspace` branch so editing invalidates the toggle window.
  - [ ] Do NOT reset inside the `convert_last_word` handler itself — the handler manages its own state via `Set`/`Reset`.
  - [ ] Do NOT reset for modifier-only events: the existing guard `if char == 0 || char == 0x08 { return false }` will skip those before we reach the reset point on the char path; for backspace the reset is still correct because backspace is an explicit edit.

### Phase 4 — Tray: replace pause with auto-convert toggle

- [x] **11. Rewrite `tray_darwin.go` state to `autoConvertEnabled`.**
  - [ ] Remove: `trayEnabled`, `isTrayEnabled()`, `goTrayToggle()`, the (unused) Go-side `createTray` declaration in the cgo header, and `updateTray`.
  - [ ] Add: `var autoConvertEnabled int32 = 1` (initialized to `1`, will be synced to config in Task 12).
  - [ ] Add: `//export goAutoConvertToggle` that flips the atomic var, calls a new C function `updateAutoConvertMenu(int)` to refresh the checkmark, and logs `"Auto-convert: enabled/disabled"`.
  - [ ] Add: `func isAutoConvertEnabled() bool { return atomic.LoadInt32(&autoConvertEnabled) == 1 }`.
  - [ ] Update the cgo header block to declare only the C symbols still in use: `void ensureApp(void); void runNSApp(void); void removeTray(void); void updateAutoConvertMenu(int);`.
  - [ ] Keep `goTrayQuit` and `func startTray()` / `runAppLoop()` as-is.

- [x] **12. Initialize `autoConvertEnabled` from `cfg.AutoConvert` at startup.**
  - [ ] In `main.go`, right after reading config and before `startTray()`, call a new helper `setAutoConvertEnabled(cfg.AutoConvert)` defined in `tray_darwin.go` that sets the atomic var and calls `C.updateAutoConvertMenu(...)` (safe even before menu exists — guarded inside the C function).

- [x] **13. Replace remaining `isTrayEnabled()` callsites in `main.go`.**
  - [ ] `main.go:252` (buffer onWord callback): replace `!isTrayEnabled()` with `!isAutoConvertEnabled()`.
  - [ ] `main.go:302` (onKeyEvent early return): replace `!isTrayEnabled()` with `!isAutoConvertEnabled()`.
  - [ ] Confirm no other references via Grep before finishing this task.

### Phase 5 — Tray Objective-C: dynamic flags + auto-convert menu

- [x] **14. Replace tray menu in `tray_darwin.m` `ensureApp` with auto-convert toggle.**
  - [ ] Remove the `⏸ Приостановить` menu item.
  - [ ] Add `NSMenuItem *autoConvertItem = [[NSMenuItem alloc] initWithTitle:@"Автоконвертация" action:@selector(autoConvertAction:) keyEquivalent:@""];` with `target = delegate`.
  - [ ] Set initial `state` based on a new C-level mirror variable `static int autoConvertOn = 1;` (set via `updateAutoConvertMenu`).
  - [ ] Keep separator and `Выйти` item (selector `quitAction:` → `goTrayQuit()`).
  - [ ] Expose `extern void goAutoConvertToggle(void);` and remove `extern void goTrayToggle(void);`.
  - [ ] Add new selector method on `TrayDelegate`: `- (void)autoConvertAction:(id)sender { goAutoConvertToggle(); }`.
  - [ ] Remove the now-unused `createTray` function and its `extern` exposure (the build still works because `tray_darwin.go` no longer declares it).

- [x] **15. Implement `updateAutoConvertMenu(int enabled)` in `tray_darwin.m`.**
  - [ ] Stores `autoConvertOn = enabled` and, on the main queue, sets `autoConvertItem.state = enabled ? NSControlStateValueOn : NSControlStateValueOff`.
  - [ ] Guard against nil menu/item (called before `ensureApp`).
  - [ ] Keep the function callable from any thread via `dispatch_async(dispatch_get_main_queue(), ^{ ... })`.

- [x] **16. Replace the old "⚡/💤" tray icon with flag emoji + initial layout probe.**
  - [ ] In `ensureApp`, after creating `statusItem`, set the title using a new helper `setTrayTitleFromCurrentLayout()` that:
    - [ ] Calls `TISCopyCurrentKeyboardInputSource()`, reads `kTISPropertyInputSourceID`, checks for case-insensitive substring `Russian`.
    - [ ] Sets `statusItem.button.title = isRussian ? @"🇷🇺" : @"🇺🇸"`.
    - [ ] Releases the copied source.
  - [ ] Use a system emoji-capable font: `statusItem.button.font = [NSFont systemFontOfSize:14];` (already set; verify it renders the regional indicator pair as a flag — Apple Color Emoji fallback handles it).

- [x] **17. Add `NSDistributedNotificationCenter` observer for layout changes.**
  - [ ] Define a new class `LayoutObserver : NSObject` with method `- (void)layoutChanged:(NSNotification *)note { setTrayTitleFromCurrentLayout(); }`.
  - [ ] In `ensureApp`, instantiate a singleton `LayoutObserver *layoutObserver` and register:
    ```objc
    [[NSDistributedNotificationCenter defaultCenter]
        addObserver:layoutObserver
        selector:@selector(layoutChanged:)
        name:(NSString *)kTISNotifySelectedKeyboardInputSourceChanged
        object:nil];
    ```
  - [ ] If `kTISNotifySelectedKeyboardInputSourceChanged` is not exposed in the SDK headers being used, fall back to the literal string `@"AppleSelectedInputSourcesChangedNotification"` AND `@"NSTextInputContextKeyboardSelectionDidChangeNotification"` (register for both to be safe).
  - [ ] Ensure `setTrayTitleFromCurrentLayout()` always dispatches UI work to the main queue (`dispatch_async(dispatch_get_main_queue(), ^{ ... })`).

### Phase 6 — Sanity check

- [x] **18. Build smoke check.**
  - [x] Run `go build ./...` on macOS (informational here — current host is Linux). Cross-compile to darwin requires clang (not installed in this Linux env); verified `CGO_ENABLED=0 GOOS=windows go build ./...` succeeds, confirming `main.go` is consistent with the Windows stubs (added `setAutoConvertEnabled` / renamed `isTrayEnabled` → `isAutoConvertEnabled` in `replacer_windows.go`).
  - [x] Verified via `Grep`: no `trayEnabled`, `isTrayEnabled`, `goTrayToggle`, `createTray`, `updateTray`, or `kCGEventFlagMaskShift` references remain in `*.go` / `*.m`. `macX`/`macC`/`macV` const definitions removed; `macZ` retained (still used by Cmd+Z undo handler).
  - [ ] Manual verification on macOS (deferred — must be run by a human on macOS hardware):
    - [ ] Tray shows 🇷🇺 / 🇺🇸 reflecting layout; switching layouts via Caps Lock / Cmd+Space updates icon within ~50ms.
    - [ ] Cmd+Shift+X still converts selection.
    - [ ] Cmd+Shift+Z converts the last word; pressing again immediately reverts.
    - [ ] Pressing any letter or backspace after the conversion clears the toggle so a second Cmd+Shift+Z creates a fresh conversion (does not revert).
    - [ ] Tray menu shows "Автоконвертация" with checkmark; toggling stops the buffer-callback auto-conversion path but keeps both manual hotkeys functional.

## Commit Plan

- [ ] After Tasks 1–4: `feat(config): add hotkey config + auto-convert flag and parser`
- [ ] After Tasks 5–10: `feat(hotkey): configurable hotkeys and last-word toggle`
- [ ] After Tasks 11–13: `refactor(tray): replace pause with auto-convert toggle`
- [ ] After Tasks 14–17: `feat(tray): dynamic flag icon via input-source notification`
- [ ] After Task 18: `chore: smoke-check build and remove dead constants`
