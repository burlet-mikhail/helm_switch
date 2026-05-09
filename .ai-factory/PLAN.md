<!-- handoff:task:ff20b79e-a1a1-49d6-8172-38383f25f3fa -->
# Plan: Caps Lock as Hardcoded Conversion Hotkey

**Mode:** fast  
**Tests:** false (no new test scaffolding)  
**Docs:** false  
**Platform scope:** darwin (macOS). Windows hook only needs a signature-compat shim.

---

## 1. Goal

Replace the two configurable hotkeys (`convert_selection`, `convert_last_word`) with a single hardcoded **Caps Lock** key. Behavior is context-aware:

- [x] Selection present → convert selected text (current `convertSelectedText` flow).
- [x] No selection → convert last typed word (current `convert_last_word` inline flow in `main.go`).

Caps Lock arrives as `kCGEventFlagsChanged` (type 12), keycode `0x39`. The current event tap in `hook_darwin.go` only listens to `kCGEventKeyDown` and must be widened. The CGO callback signature must gain an `eventType` parameter so Go-side code can branch on it.

All hotkey config plumbing (`HotkeyConfig`, `ParseHotkey`, `ParsedHotkeys`, `modifierMap`, `keyMap`, default hotkey constants, `Config.Hotkeys`, `hotkeyFlag*` modifier constants) is deleted.

---

## 2. Files touched

| File | Change |
|---|---|
| `hook_darwin.go` | Extend event mask; handle `kCGEventFlagsChanged`; widen `goKeyCallback` and `onKeyEvent` signatures; add Caps Lock constants. |
| `hook_windows.go` | Mirror the new `onKeyEvent` signature so the package compiles on Windows. (Caps Lock not implemented there.) |
| `main.go` | Add `eventType` param to `onKeyEvent`; add Caps Lock branch; extract last-word logic into a function; add `handleCapsLock`; remove old hotkey checks, `ParsedHotkeys` call, debug log, and `allModsMask`. |
| `config.go` | Delete `HotkeyConfig`, `Config.Hotkeys`, `ParseHotkey`, `ParsedHotkeys`, `modifierMap`, `keyMap`, `hotkeyFlag*` constants, default hotkey constants, `LoadConfig` backfill. |
| `replacer_darwin.go` | No code change (reuses `sendCopy`, `sendPaste`, `readClipboard`, `writeClipboard`, `replacing` flag). |
| `buffer.go`, `detector.go`, `dict.go`, `keymap.go`, `exceptions.go`, `rollback.go`, `tray_darwin.go`, `appid_darwin.go` | Untouched. |
| `README.md` | No changes required (no hotkey references found). |

User config at `~/Library/Application Support/RuSwitch/config.yaml` keeps any pre-existing `hotkeys:` section as a silently-ignored unknown field (`yaml.Unmarshal` is non-strict). On first save (`SaveConfig`) the file is rewritten without the `hotkeys` block.

---

## 3. Implementation checklist

<!-- parallel: none — sections 3.1–3.5 share function signatures and are executed sequentially in a single layer -->

### 3.1 `hook_darwin.go` — widen event tap and callback

- [x] In the C `createTap`, widen the event mask:
  ```c
  CGEventMask mask = (1 << kCGEventKeyDown) | (1 << kCGEventFlagsChanged);
  ```
- [x] In `eventCallback`, replace the early-return on non-keydown. Accept both `kCGEventKeyDown` and `kCGEventFlagsChanged`; pass the type through to Go:
  - [x] For `kCGEventFlagsChanged`, do **not** call `CGEventKeyboardGetUnicodeString` (no character data) — pass `ch = 0`. `CGEventGetIntegerValueField(event, kCGKeyboardEventKeycode)` and `CGEventGetFlags(event)` both work for flagschanged events, so the existing reads on lines 24–25 stay unchanged.
  - [x] Keep the `kCGEventTapDisabledByTimeout / kCGEventTapDisabledByUserInput` branch as-is (re-enables the tap).
  - [x] Any other type → `return event`.
- [x] Update the C extern declaration and Go `//export goKeyCallback` to take a new `int64_t eventType` first parameter:
  ```c
  extern int goKeyCallback(int64_t eventType, int64_t keycode, UniChar character, int64_t flags);
  ```
- [x] Update the Go `goKeyCallback` body to forward `eventType` to `onKeyEvent`. Keep the `replacing == 1` short-circuit (returns 0) at the very top so Caps Lock is also bypassed during our own paste/type sequences.
- [x] Change the Go `onKeyEvent` variable declaration to:
  ```go
  var onKeyEvent func(eventType int64, keycode uint16, char rune, flags int64) bool
  ```
- [x] Add Caps Lock constants in the Go side of `hook_darwin.go` (alongside `kCGEventFlagMaskCommand`):
  ```go
  const (
      kCGEventFlagMaskCommand   = 1 << 20
      kCGEventTypeKeyDown       int64 = 10
      kCGEventTypeFlagsChanged  int64 = 12
      capsLockKeyCode           uint16 = 0x39
      capsLockMask              int64  = 1 << 16 // NX_ALPHASHIFTMASK
      anyModifierMask           int64  = (1 << 17) | (1 << 18) | (1 << 19) | (1 << 20) // shift|ctrl|alt|cmd
  )
  ```
  (Drop `kCGEventFlagMaskCommand`'s old standalone block; merge.)

### 3.2 `hook_windows.go` — keep package compiling

- [x] Update the `onKeyEvent` variable declaration (currently at line 58) to match the new signature (eventType ignored on Windows):
  ```go
  var onKeyEvent func(eventType int64, keycode uint16, char rune, flags int64) bool
  ```
- [x] In the keyboard-hook callback (line 78: `onKeyEvent(uint16(kb.VkCode), ch, 0)`), pass a constant `0` for `eventType` as the first argument. Document inline that flagschanged interception is not implemented on Windows. *(Used `kCGEventTypeKeyDown` constant rather than literal `0` for clarity; eventType branch in main.go is dead on Windows because we always pass key-down.)*
- [x] Leave the `const kCGEventFlagMaskCommand = 0` shim (line 61) in place — `main.go`'s Cmd+Z branch still references it, and Windows treats it as a no-op match.
- [x] **Added (not in plan):** Mirror Caps Lock constants (`kCGEventTypeFlagsChanged`, `capsLockKeyCode`, `capsLockMask`, `anyModifierMask`) on Windows so `main.go` (which has no build tag) compiles. The constants are dead code on Windows because the hook callback always passes `kCGEventTypeKeyDown`.

### 3.3 `main.go` — Caps Lock handler + cleanup

- [x] Update `onKeyEvent` assignment to the new signature: `func(eventType int64, keycode uint16, char rune, flags int64) bool`.
- [x] **Delete** these blocks/lines from `main()`:
  - [x] The `selectionHotkey, lastWordHotkey := cfg.ParsedHotkeys()` call and the `log.Printf("Hotkeys: ...")` line that follows it.
  - [x] The `const allModsMask = ...` declaration above `onKeyEvent`.
  - [x] The DEBUG `log.Printf("DEBUG keyevent: ...", lastWordHotkey..., selectionHotkey...)` block at the top of `onKeyEvent`.
  - [x] The whole `if keycode == lastWordHotkey.KeyCode && ...` block (the convert-last-word inline body).
  - [x] The whole `if keycode == selectionHotkey.KeyCode && ...` block (the convert-selection hotkey body).
- [x] **Extract** the convert-last-word logic into a top-level package function `convertLastWordFromBuffer(buf *Buffer)` that contains the body previously inlined in `onKeyEvent` (snapshot, toggle-back, fresh-convert, `lastWord.Set` arming, goroutine, etc.). Call sites: `handleCapsLock` (no-selection branch).
- [x] **Add** a top-level package function `handleCapsLock(buf *Buffer, detector *Detector)`:
  ```go
  func handleCapsLock(buf *Buffer, detector *Detector) {
      savedClipboard := readClipboard()

      atomic.StoreInt32(&replacing, 1)
      sendCopy()
      time.Sleep(100 * time.Millisecond)

      selected := readClipboard()

      if selected != "" && selected != savedClipboard {
          // Selection path: mirror convertSelectedText body, but reuse the
          // already-issued Cmd+C so we do not double-copy. Inline the
          // convert + paste + restore-clipboard + switchLang + replacing=0.
          // (See 3.4 — convertSelectedText is refactored to accept a
          // pre-copied selection.)
          finishSelectionConvert(detector, savedClipboard, selected)
          return
      }

      // No selection: restore clipboard, drop the replacing flag, then
      // run the last-word path.
      atomic.StoreInt32(&replacing, 0)
      if savedClipboard != "" {
          writeClipboard(savedClipboard)
      }
      convertLastWordFromBuffer(buf)
  }
  ```
- [x] **Add the Caps Lock branch** at the very top of `onKeyEvent` (before the `cfg.Enabled` / `auto-convert` gate, before Cmd+Z, mirroring the previous "checked before gate" hotkey behavior):
  ```go
  if eventType == kCGEventTypeFlagsChanged {
      if keycode == capsLockKeyCode &&
          (flags & ^capsLockMask & anyModifierMask) == 0 {
          // Plain Caps Lock — no other modifier held.
          go handleCapsLock(buf, detector)
          return true // suppress the toggle so the OS layout/case state is not flipped
      }
      // Any other flagschanged event (Shift, Cmd, Option, etc.) — ignore.
      return false
  }
  ```
  Notes:
  - [x] Closure already captures `buf` and `detector` (defined earlier in `main()`).
  - [x] We fire on every flagschanged with keycode `0x39` regardless of whether the AlphaShift bit is currently set or cleared — kCGEventFlagsChanged emits exactly one event per physical Caps Lock press, so this gives one conversion per press.
  - [x] The combined-modifier guard `(flags & ^capsLockMask & anyModifierMask) == 0` rejects e.g. Cmd+CapsLock so the user can still reach OS-level CapsLock+modifier shortcuts.
- [x] After the Caps Lock branch, leave the rest of `onKeyEvent` (Cmd+Z, backspace, regular char ingestion, Enter-flush) **unchanged**, except for the new function signature.
- [x] Confirm `lastWord.Reset()` is **not** called from the Caps Lock branch (the existing reset on backspace and on regular char in the keydown branch still invalidates the toggle window correctly).

### 3.4 `main.go` — refactor `convertSelectedText`

The existing `convertSelectedText(detector *Detector)` performs save-clipboard → Cmd+C → wait → read → convert → paste → restore. `handleCapsLock` already does the save+copy+wait+read steps, so split out the post-copy half:

- [x] Add a new function `finishSelectionConvert(detector *Detector, savedClipboard, selected string)` containing the body from `convertSelectedText` starting at "Convert the entire selection in whichever direction fits..." (heuristic, `writeClipboard(converted)`, `sendPaste()`, `writeClipboard(savedClipboard)`, `switchLang()`, `replacing = 0`, log).
- [x] Either delete `convertSelectedText` entirely (it had only one caller — the now-removed `selectionHotkey` block) or keep it as a thin wrapper that does the copy then calls `finishSelectionConvert`. **Delete it** — no other callers remain.

### 3.5 `config.go` — strip hotkey machinery

- [x] Delete the `HotkeyConfig` struct (lines around 13–18).
- [x] Delete the `Hotkeys HotkeyConfig` field from `Config`.
- [x] Delete `defaultHotkeyConvertSelection` and `defaultHotkeyConvertLastWord` constants.
- [x] Delete the `hotkeyFlagShift / hotkeyFlagControl / hotkeyFlagAlt / hotkeyFlagCommand` constant block (no remaining users — `kCGEventFlagMaskCommand` lives in `hook_darwin.go`; `anyModifierMask` lives in `hook_darwin.go`).
- [x] Delete the `Hotkey` struct, `modifierMap`, `keyMap`, `ParseHotkey`, and `ParsedHotkeys`.
- [x] In `DefaultConfig()`, remove the `Hotkeys: HotkeyConfig{...}` initializer.
- [x] In `LoadConfig()`, remove the post-Unmarshal backfill block (`if cfg.Hotkeys.ConvertSelection == "" { ... }` and the `ConvertLastWord` equivalent) and the two log lines.
- [x] Verify `gopkg.in/yaml.v3` is still imported (it is — used by Marshal/Unmarshal). *(Also dropped now-unused `fmt` and `log` imports.)*

### 3.6 User config migration

- [x] No active code change. Document inline (comment near `LoadConfig`) that pre-existing `hotkeys:` keys in the on-disk config are silently ignored by `yaml.Unmarshal` (non-strict mode) and will be omitted on next `SaveConfig` write.
- [x] No change to `.ai-factory/config.yaml` (that's the AI-Factory tooling config, unrelated to the app).

### 3.7 Tests

- [x] Search confirms no test file references `ParseHotkey`, `ParsedHotkeys`, `HotkeyConfig`, `hotkeyFlag*`, `modifierMap`, or `keyMap` (`grep` over `*_test.go` returned zero hits in `detect_test.go`, `exceptions_test.go`, `integration_test.go`, `rollback_test.go`, `shifted_test.go`). No deletions needed. *(Re-verified post-implementation: zero hits across all `.go` files.)*
- [x] No new tests are added under `tests: false`.

---

## 4. Edge cases (verify in code)

| Case | Expected behavior | Where enforced |
|---|---|---|
| Caps Lock held (auto-repeat) | Fires once. | OS only emits one `kCGEventFlagsChanged` per physical press; no extra logic. |
| Cmd+CapsLock or Shift+CapsLock | No conversion; pass-through. | `(flags & ^capsLockMask & anyModifierMask) == 0` guard in `onKeyEvent`. |
| Other modifier key (Shift/Cmd/Option) flagschanged | Ignored. | `keycode == capsLockKeyCode` guard. |
| Empty buffer + no selection | Cmd+C is sent (harmless), clipboard restored, `convertLastWordFromBuffer` no-ops on empty `LastWord()`. | Existing `current == ""` branch. |
| Selection in terminal that ignores Cmd+C | Clipboard unchanged → falls through to last-word path. | `selected == savedClipboard` branch in `handleCapsLock`. |
| Excluded app | Caps Lock conversion still works (matches old hotkey behavior — runs before the `cfg.Enabled / IsAppExcluded` gate). | Branch placed at top of `onKeyEvent`. |
| Toggle-back (second press without typing) | Works for last-word; not for selection (no toggle state set in selection path). | Existing `lastWord.active` semantics; selection path never calls `lastWord.Set`. |
| Re-entry during our own Cmd+C / Cmd+V | `goKeyCallback` short-circuits when `replacing == 1`. | Already in `hook_darwin.go`. `handleCapsLock` raises the flag before `sendCopy` and lowers it on the no-selection branch before calling `convertLastWordFromBuffer` (which raises it itself in its goroutine). |

---

## 5. Risks and mitigations

1. **100 ms latency on every Caps Lock press** (Cmd+C round trip). Acceptable for a manual hotkey; measurable but not painful. Mitigation: reduce sleep to 50 ms after manual smoke testing if reliable.
2. **Cmd+C side effects in IDEs** (some apps copy current line on empty selection — that would falsely trigger the selection path). Mitigation: tested manually; if problematic, compare `selected` byte length / hash against `savedClipboard` more strictly. For now: documented risk.
3. **Caps Lock LED toggling** despite event suppression — hardware-driven and not always controllable from the event tap. Document recommendation: System Settings → Keyboard → Modifier Keys → set Caps Lock to "No Action".
4. **Compile breakage on Windows** if the `onKeyEvent` signature drifts. Mitigation: update `hook_windows.go` in the same commit (3.2).
5. **Race between `replacing` flag toggles** in `handleCapsLock` and the goroutine inside `convertLastWordFromBuffer`. Window: `handleCapsLock` runs in its own goroutine, sets `replacing = 1`, sends Cmd+C, sleeps 100 ms, reads clipboard. On the no-selection branch it sets `replacing = 0` then calls `convertLastWordFromBuffer`, which spawns *another* goroutine that re-raises `replacing = 1`. Between the store-0 and the goroutine's store-1 there is a tiny window where a stray real keystroke would not be filtered. Mitigation: this is the same pattern as the existing `convertSelectedText` flow today; if it ever proves problematic, raise/lower the flag once around the *entire* flow instead of toggling between sub-steps.

---

## 6. Manual test checklist (run on macOS, dev build)

- [ ] Type a wrong-layout Russian word in QWERTY → press Caps Lock → word converted to Cyrillic, layout switched.
- [ ] Press Caps Lock again immediately (no other key) → word reverts to original (toggle).
- [ ] Type a word, press space, press Caps Lock → last flushed word is converted (with the trailing space rebuilt).
- [ ] Select a paragraph in TextEdit → Caps Lock → entire selection converted in place; clipboard restored.
- [ ] Open IDE listed in `excluded_apps` → Caps Lock → conversion still works (manual hotkey bypasses the gate).
- [ ] Empty document, no selection → Caps Lock → no-op, no errors in `~/Library/Logs` or `${TMPDIR}/ruswitch.log`.
- [ ] Cmd+CapsLock → no conversion fires; passes through.
- [ ] Hold Caps Lock for 1 s → exactly one conversion runs.
- [ ] After conversion, type a normal letter → toggle window invalidated (verified by next Caps Lock re-converting from current buffer, not bouncing to the previous original).
- [ ] Pre-existing user `~/Library/Application Support/RuSwitch/config.yaml` with `hotkeys:` block → app starts cleanly, log shows no parse error, file is rewritten without `hotkeys` after any config change.

---

## 7. Build / verification

- [ ] `go build ./...` on macOS — must succeed with the new CGO callback signature.
- [ ] `go vet ./...` — must pass.
- [ ] `go test ./...` — existing tests must still pass (no test referenced removed symbols).
- [ ] `make` (or the project's existing build target) produces a working `.app` bundle.

---

## 8. Rollback note

If Caps Lock proves unworkable in the field (LED issues, IDE Cmd+C side effects), the smallest safe rollback is to reintroduce `convertSelectedText` as the body of a hardcoded Cmd+Shift+X check in `onKeyEvent` and re-add a hardcoded Ctrl+A check for last-word — *without* reintroducing the full `HotkeyConfig` plumbing. Keep the simplification.
