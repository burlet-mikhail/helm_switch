# RuSwitch: Dynamic Tray Flags + Configurable Hotkeys + Last Word Conversion

**Date:** 2026-04-25
**Status:** Approved

## Summary

Three features for macOS:

1. Dynamic flag icons in the system tray (🇷🇺/🇺🇸) reflecting the current keyboard layout
2. Conversion of the last typed word by a configurable hotkey (toggle behavior)
3. Configurable hotkeys via `config.yaml`

Additionally: removal of the "pause" concept, replaced by an "Auto-convert on/off" toggle in the tray menu.

---

## 1. Dynamic Tray Flags

### Current behavior

- Tray shows `⚡` (active) or `💤` (paused) as text emoji
- Pause/resume toggles all functionality

### New behavior

- Tray shows 🇷🇺 when the current input source is Russian, 🇺🇸 otherwise
- Icon updates instantly via `NSDistributedNotificationCenter` subscription to `NSTextInputContextKeyboardSelectionDidChangeNotification`
- No pause concept — only auto-convert toggle

### Implementation

**`tray_darwin.m`:**

- On init: subscribe to `NSTextInputContextKeyboardSelectionDidChangeNotification` via `[[NSDistributedNotificationCenter defaultCenter] addObserver:...]`
- Notification handler: call `TISCopyCurrentKeyboardInputSource()`, check if ID contains "Russian" -> set `statusItem.button.title` to 🇷🇺 or 🇺🇸
- Initial icon: check layout at startup, set the correct flag immediately

**Tray menu:**

- "Автоконвертация" — toggle item with checkmark (NSOnState/NSOffState). Calls `goAutoConvertToggle()` which flips `autoConvertEnabled` atomic variable
- Separator
- "Выйти" — calls `goTrayQuit()` as before

### Removed

- `trayEnabled` int32 — removed entirely
- `isTrayEnabled()` — removed
- `goTrayToggle()` — replaced by `goAutoConvertToggle()`
- `createTray()` function — unused (initialization is in `ensureApp`)
- `updateTray(int enabled)` — replaced by layout-change notification handler

---

## 2. Last Word Conversion by Hotkey

### Behavior

- Default hotkey: `Cmd+Shift+Z` (configurable)
- Takes the current buffer content (last typed word) via new `Buffer.LastWord()` method
- If the word contains Cyrillic — converts RU -> QWERTY; otherwise QWERTY -> RU (same heuristic as `convertSelectedText`)
- Deletes the word with backspaces, types the converted version
- Switches system layout after conversion
- **Toggle:** if pressed again without typing anything new — converts back to original
- Works regardless of auto-convert setting

### Implementation

**`buffer.go`:**

- Add `LastWord() string` — returns current buffer content as string without flushing

**`main.go`:**

- New struct `lastWordConversion` with fields: `original string`, `converted string`, `active bool` (tracks toggle state)
- In `onKeyEvent`: check configured hotkey for `convert_last_word`. On match:
  - If `lastWordConversion.active` and buffer hasn't changed: convert back (toggle)
  - Else: get `buf.LastWord()`, convert, replace via backspaces + sendChar
- Any regular keystroke resets `lastWordConversion.active = false`

### Edge cases

- Empty buffer: no-op (ignore hotkey)
- Single character: convert anyway (user intent is clear)
- Hotkey works even when auto-convert is off

---

## 3. Configurable Hotkeys

### Config format

```yaml
enabled: true
primary_language: ru
min_word_length: 2
excluded_apps:
  - idea

hotkeys:
  convert_selection: "Cmd+Shift+X"
  convert_last_word: "Cmd+Shift+Z"
```

### Defaults

If `hotkeys` section is absent, defaults are:
- `convert_selection`: `Cmd+Shift+X`
- `convert_last_word`: `Cmd+Shift+Z`

### Parsing

**`config.go`:**

- New struct `HotkeyConfig` with fields `ConvertSelection string` and `ConvertLastWord string`
- New struct `Hotkey` (parsed): `KeyCode uint16`, `Modifiers int64` (CGEventFlags bitmask)
- Parse function: split by `+`, last token = key, preceding tokens = modifiers
- Modifier mapping: `Cmd` -> `kCGEventFlagMaskCommand` (1<<20), `Shift` -> `kCGEventFlagMaskShift` (1<<17), `Ctrl` -> `kCGEventFlagMaskControl` (1<<18), `Alt` -> `kCGEventFlagMaskAlternate` (1<<19)
- Key mapping: `A`-`Z` mapped to macOS virtual keycodes (0x00=A, 0x0B=B, etc.), plus special keys
- Parsing happens at startup; invalid hotkey = log warning + use default

**`main.go`:**

- `onKeyEvent` checks `keycode` and `flags` against parsed `Hotkey` structs instead of hardcoded constants

---

## 4. Changes to Existing Code

### Files modified

| File | Changes |
|------|---------|
| `tray_darwin.go` | Remove `trayEnabled`, `isTrayEnabled()`, `goTrayToggle()`. Add `autoConvertEnabled`, `goAutoConvertToggle()`, `isAutoConvertEnabled()` |
| `tray_darwin.m` | Rewrite menu (auto-convert toggle + quit). Add NSDistributedNotification observer for layout changes. Update icon to flags |
| `config.go` | Add `HotkeyConfig` struct, `Hotkey` parsed struct, hotkey parsing logic, `AutoConvert` field |
| `main.go` | Add last-word conversion handler, use parsed hotkeys instead of hardcoded constants, replace `isTrayEnabled()` checks with `isAutoConvertEnabled()` |
| `buffer.go` | Add `LastWord() string` method |

### Files NOT modified

| File | Reason |
|------|--------|
| `detector.go` | Detection logic unchanged |
| `dict.go` | Dictionary unchanged |
| `keymap.go` | Conversion maps unchanged |
| `hook_darwin.go` | Event tap unchanged |
| `replacer_darwin.go` | Low-level input unchanged |
| `exceptions.go` | Exception store unchanged |
| `rollback.go` | Rollback tracker unchanged |

### Removed concepts

- Pause/resume (trayEnabled) — replaced by auto-convert toggle
- Hardcoded hotkey constants for Cmd+Shift+X — replaced by configurable hotkeys

### New concepts

- `autoConvertEnabled` — controls only auto-correction, not manual hotkeys
- `lastWordConversion` — toggle state for repeated last-word conversion
- `Hotkey` — parsed hotkey with keycode + modifier mask

---

## 5. Testing

- **Hotkey parsing:** unit tests for `ParseHotkey("Cmd+Shift+X")` -> correct keycode + flags
- **LastWord:** unit test for `Buffer.LastWord()` returning buffer without flushing
- **Toggle logic:** unit test for last-word conversion toggle behavior
- **Existing tests:** must continue passing (no changes to detection/dictionary logic)
- **Manual testing:** build `.app`, verify tray flags change on layout switch, verify hotkeys work
