<!-- handoff:task:872cff12-94b3-4dc5-9acf-d2ebca2ec2fe -->
# Plan: Caps Lock as Hardcoded Conversion Hotkey

Created: 2026-05-09
Mode: fast
Branch: main

## Settings

- [ ] Testing: no
- [ ] Logging: verbose
- [ ] Docs: no
- [ ] Roadmap linkage: skipped

## Goal

Replace the configurable `convert_selection` and `convert_last_word` hotkeys with a single, hardcoded **Caps Lock** trigger that converts the current selection (when present) or the last typed word (when no selection exists). Because macOS does not deliver Caps Lock through `CGEventTap` reliably, remap Caps Lock to F18 at HID level via `hidutil` at startup and restore the original mapping at shutdown.

## High-Level Approach

1. At startup: invoke `hidutil property --set ...` to remap the Caps Lock HID usage (0x700000039) to F18 (0x70000006D). Preserve any pre-existing `UserKeyMapping` entries by reading them first and merging.
2. The CGEventTap then receives F18 (virtual keycode `0x4F = 79`) as a normal `kCGEventKeyDown` and can intercept and suppress it.
3. On F18 down with clean modifier flags: dispatch `handleCapsLock(buf, detector)` in a goroutine and suppress the event (return `true`).
4. `handleCapsLock` decides between selection-conversion and last-word-conversion using a clipboard-probe heuristic.
5. On shutdown (defer + signal handler for SIGINT/SIGTERM): restore the original `UserKeyMapping` (or clear it if there was none).

## Phase 1 — hidutil Lifecycle Wrappers (main.go)

<!-- sequential: all phases edit overlapping code in main.go / hook_darwin.go / hook_windows.go and must run as a single coordinated change -->

- [x] Add imports as needed (`os/exec`, `encoding/json`, `strings`, `os/signal`, `syscall`).
- [x] Add package-level state to remember the original mapping so shutdown can restore it:
  ```go
  var (
      originalUserKeyMapping []map[string]uint64 // nil if there was no prior mapping
      capsLockRemapped       bool
  )
  ```
- [x] Implement `func remapCapsLockToF18() error` in `main.go`:
  - [x] Run `hidutil property --get "UserKeyMapping"` and capture stdout.
  - [x] If `hidutil` is not found on PATH (`exec.LookPath` fails or `exec.Command` returns `errors.Is(err, exec.ErrNotFound)`): log a verbose `WARN` and return `nil` so the program keeps running.
  - [x] Parse the existing list with a small regex over `HIDKeyboardModifierMappingSrc`/`Dst` pairs.
  - [x] Save the parsed list into `originalUserKeyMapping` (may be empty / `nil`).
  - [x] Build the merged list: keep any existing entries whose `Src` is not Caps Lock (`0x700000039`), then append our entry.
  - [x] Encode merged list back into the JSON form `hidutil` accepts and run `hidutil property --set ...`.
  - [x] On success set `capsLockRemapped = true` and verbose-log the merged mapping.
  - [x] On failure: log `WARN [hidutil] remap failed: <err>` and return the error (caller chooses whether to continue).
- [x] Implement `func restoreCapsLock()` in `main.go`:
  - [x] If `!capsLockRemapped`, return immediately.
  - [x] If `originalUserKeyMapping` is non-empty: call `hidutil property --set` with the saved list.
  - [x] If it was empty/`nil`: call `hidutil property --set '{"UserKeyMapping":[]}'` to clear our entry.
  - [x] Log success/failure at verbose level. Never panic — this runs from `defer` and signal handlers.
- [x] Add a small helper `func isCapsLockSrc(v uint64) bool { return v == 0x700000039 }` to keep the merge logic readable.
- [x] Verbose log every step (read existing, merged result, set call, restore call) so debugging without a debugger is possible.

## Phase 2 — Hook Layer Changes

### 2a. hook_darwin.go

- [x] Revert the event mask back to keyDown only (`(1 << kCGEventKeyDown)`); drop any `kCGEventFlagsChanged` bit that was added for the previous Caps Lock attempt.
- [x] Remove the flags-changed handling branch from the C/Go callback. The callback now only handles `kCGEventKeyDown`.
- [x] Remove obsolete Caps Lock related constants (`capsLockKeyCode`, `capsLockMask`, `anyModifierMask`, `kCGEventTypeKeyDown`, `kCGEventTypeFlagsChanged`).
- [x] Add the new keycode constant near the top of the file: `const f18KeyCode uint16 = 0x4F // 79 — Caps Lock remapped via hidutil`.
- [x] In the keyDown branch, before the existing dispatch, dispatch to `handleCapsLock` when `keyCode == f18KeyCode && (flags & anyRealModifierMask) == 0` (Cmd/Shift/Ctrl/Alt clean) and `return true` to suppress.
- [x] Treat F18 key-repeat events as idempotent — filter them in C via `CGEventGetIntegerValueField(event, kCGKeyboardEventAutorepeat)` and return NULL early.
- [x] Verbose-log every F18 receipt (`vlog("[hook] F18 keyDown received, flags=0x%x", flags)`).

### 2b. hook_windows.go

- [x] Revert `onKeyEvent` signature to its pre-CapsLock-experiment form (no `eventType` param, just `(keycode uint16, char rune, flags int64) bool`).
- [x] Drop unused CG/CapsLock cross-compile constants (`kCGEventTypeKeyDown`, `kCGEventTypeFlagsChanged`, `capsLockKeyCode`, `capsLockMask`, `anyModifierMask`).
- [x] Add the constants needed for cross-platform `main.go` to compile: `f18KeyCode uint16 = 0xFFFF` (sentinel — must NOT be `0x4F`, which is `VK_O` on Windows and would make every "O" keypress trigger `handleCapsLock`) and `anyRealModifierMask`. Both are dead code on Windows but keep main.go build-tag-free.
- [x] Mental cross-compile check: hook_windows.go only references its own constants plus the new shared signatures.

## Phase 3 — handleCapsLock Dispatcher

Already implemented in `main.go` (alongside `finishSelectionConvert` and `convertLastWordFromBuffer`) by the previous iteration. This phase only re-verifies the dispatcher still matches the contract below; no code changes were needed.

- [x] Function skeleton:
  ```go
  func handleCapsLock(buf *Buffer, detector *LayoutDetector) {
      // 1. Guard against re-entry while we are already replacing.
      if !atomic.CompareAndSwapInt32(&replacing, 0, 1) {
          log.Debug("[capslock] busy, skipping")
          return
      }
      defer atomic.StoreInt32(&replacing, 0)

      // 2. Save current clipboard.
      savedClip, _ := readClipboard()

      // 3. Send Cmd+C to copy any current selection.
      sendCmdC()

      // 4. Wait ~100ms for the OS to populate the pasteboard.
      time.Sleep(100 * time.Millisecond)

      // 5. Read the (possibly new) clipboard.
      newClip, _ := readClipboard()

      if newClip != "" && newClip != savedClip {
          // Selection was present — convert it.
          converted := convertText(newClip, detector) // existing logic
          writeClipboard(converted)
          sendCmdV()
          // Optionally restore savedClip after a short delay; mirror existing
          // convert_selection behavior so users keep their previous clipboard.
          go func() {
              time.Sleep(150 * time.Millisecond)
              writeClipboard(savedClip)
          }()
          log.Debugf("[capslock] selection converted: %q -> %q", newClip, converted)
          return
      }

      // 6. No selection — restore clipboard and convert last word.
      writeClipboard(savedClip)
      log.Debug("[capslock] no selection, falling back to last-word")
      convertLastWord(buf, detector) // existing function used by old hotkey
  }
  ```
- [x] Reuse existing helpers — no duplication:
  - [x] `readClipboard` / `writeClipboard` — defined in `replacer_darwin.go`.
  - [x] `sendCopy` / `sendPaste` — defined in `replacer_darwin.go`.
  - [x] `finishSelectionConvert` — handles selection path including clipboard restore.
  - [x] `convertLastWordFromBuffer` — handles the no-selection path with the toggle window.
  - [x] `replacing` atomic flag — declared in `replacer_darwin.go`; reused.
- [x] Verbose log entry/exit, decision branch (existing `log.Printf` calls inside `finishSelectionConvert` and `convertLastWordFromBuffer`).

## Phase 4 — Shutdown Wiring (main.go)

- [x] In `main()` early (before `startHook`): call `remapCapsLockToF18()` (logging on warn) and `defer restoreCapsLock()`.
- [x] Folded `restoreCapsLock()` into the existing SIGINT/SIGTERM signal handler goroutine before `os.Exit(0)` — no second handler.
- [x] Also call `restoreCapsLock()` before the `log.Fatalf("Hook error: ...")` exit and from `goTrayQuit` (both bypass `defer` via `os.Exit`).
- [x] `restoreCapsLock` is idempotent (`capsLockRemapped` is flipped to `false` on first run) — safe from `defer`, signal handler, tray Quit, and Hook-error fast path.

## Phase 5 — Config Cleanup Verification

- [x] `config.go` no longer references `convert_selection` or `convert_last_word` hotkey fields (verified — only `Enabled`, `PrimaryLanguage`, `MinWordLength`, `ExcludedApps`, `AutoConvert`).
- [x] `.ai-factory/config.yaml` is the AI Factory project config (no app-level `hotkeys:` section); user-facing `config.yaml` is generated from `Config` struct, which has no hotkey fields.
- [x] `hook_darwin.go` / `hook_windows.go` have no leftover `cfg.Hotkeys.*` references.

## Phase 6 — Manual Verification Checklist

Run the binary on macOS and check each item:

- [ ] `hidutil property --get "UserKeyMapping"` while the app runs shows our entry mapping `0x700000039 -> 0x70000006D` plus any pre-existing entries.
- [ ] Caps Lock LED no longer turns on when pressing the key.
- [ ] In a text field, type a word in the wrong layout, press Caps Lock with no selection — last word is converted.
- [ ] Press Caps Lock again — toggles back (existing convert_last_word semantics).
- [ ] Select a word, press Caps Lock — selection is converted via Cmd+C/Cmd+V path; the previously copied clipboard is restored after ~150ms.
- [ ] Press Cmd+CapsLock — event passes through (not intercepted), because flags are non-zero.
- [ ] Hold Caps Lock down — autorepeat events are ignored, no clipboard storms.
- [ ] Quit via Ctrl+C in the launching terminal — Caps Lock returns to normal (LED works again, `hidutil property --get "UserKeyMapping"` no longer shows our entry, or shows only pre-existing entries).
- [ ] Quit via SIGTERM (`kill <pid>`) — same restore behavior.
- [ ] If `hidutil` is renamed/missing on PATH (simulate with `PATH=/tmp ./helm_switch`): app starts, logs a warn, no crash, F18 path simply never triggers (because Caps Lock is not remapped).

## Files Changed

- [x] `main.go` — `remapCapsLockToF18`, `restoreCapsLock`, parser/encoder helpers, signal/defer wiring, log-fatal pre-cleanup.
  - Post-review fixes: `capsLockRemapped` now `int32` with `atomic.CompareAndSwapInt32` (race-safe across the four shutdown paths); `parseHidutilNumber` handles both decimal and hex output (`0x700000039`); refuse-to-install guard when parser can't extract any entry from non-empty output (so we never clobber a user's mapping on shutdown); `looksLikeEmptyHidutilList` recognises `()`, `(\n)`, `(null)`.
- [x] `hook_darwin.go` — reverted event mask to keyDown-only, dropped flags-changed handling, added `f18KeyCode` and `anyRealModifierMask`, autorepeat-skip in C callback, slimmed `goKeyCallback` signature.
- [x] `hook_windows.go` — reverted `onKeyEvent` signature, dropped CG/CapsLock cross-compile constants, added shared `f18KeyCode` (sentinel `0xFFFF` — `0x4F` would collide with `VK_O`) and `anyRealModifierMask` constants.
- [x] `tray_darwin.go` — added `restoreCapsLock()` call in `goTrayQuit` so Quit-from-tray (which calls `os.Exit` and skips defers) still restores the Caps Lock mapping.

## Files NOT Changed

- [ ] `config.go` — already cleaned up; no further changes.
- [ ] `.ai-factory/config.yaml` and any runtime `config.yaml` — already cleaned up.
- [ ] `detector.go`
- [ ] `dict.go`
- [ ] `keymap.go`
- [ ] `replacer_darwin.go`
- [ ] `exceptions.go`
- [ ] `rollback.go`
- [ ] `buffer.go`
- [ ] `appid_darwin.go`
- [ ] `logging.go`
- [ ] All `*_test.go` files (testing: no per settings).

## Edge Cases

- [x] **`hidutil` missing or not on PATH** → `remapCapsLockToF18` logs `WARN [hidutil] not found ...` and returns `nil`. App continues; F18 path simply never fires. `restoreCapsLock` is a no-op because `capsLockRemapped == 0`.
- [x] **Pre-existing user remappings** → read existing list, merge our Caps Lock entry without dropping theirs, and on shutdown restore exactly the original list.
- [x] **Parser breakage on a future macOS version** → if `parseHidutilUserKeyMapping` returns zero entries from non-empty/non-`(null)` output, refuse to install the remap so shutdown can never wipe a user's UserKeyMapping. (`looksLikeEmptyHidutilList` distinguishes legitimate empty results from parse failures.)
- [x] **Hard crash / `kill -9`** → cannot run defer or signal handler. Manual reset command logged at startup.
- [x] **Cmd+CapsLock, Shift+CapsLock, etc.** → `(flags & anyRealModifierMask) == 0` guard in `onKeyEvent` lets the event pass through.
- [x] **Key repeat (holding Caps Lock)** → autorepeat F18 events filtered in the C-side `eventCallback` via `kCGKeyboardEventAutorepeat`; `replacing` atomic flag inside `handleCapsLock` is the second layer.
- [x] **Concurrent shutdown paths** (`defer`, signal handler goroutine, tray-Quit Cocoa callback, fatal-error pre-cleanup) → `atomic.CompareAndSwapInt32(&capsLockRemapped, 1, 0)` makes `hidutil --set` run at most once; the snapshot read happens after a successful CAS so it's race-free.
- [x] **Clipboard probe interferes with running apps** → save and restore clipboard around every probe; restore on a delayed goroutine after the paste finishes.
- [x] **Selection probe gives empty string** → `newClip != ""` guard treats as no-selection and falls back to last-word.
- [x] **App still has a Caps Lock event in flight when shutdown fires** → `restoreCapsLock` is independent of the hook; worst case is a stale paste; no resource leak.

## Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| `hidutil` output format changes between macOS versions and our parser breaks. | Prefer `--output json` form; if the platform doesn’t support it, fall back to a permissive regex over `HIDKeyboardModifierMappingSrc/Dst` pairs. On any parse failure, fall back to "no pre-existing mapping" and log `WARN`. Worst case: on shutdown we clear the mapping fully — same as a fresh boot. |
| User had pre-existing `UserKeyMapping` we accidentally drop. | Always parse and merge; on shutdown restore the parsed snapshot verbatim. Log both before and after states at verbose level so a user can recover manually if needed. |
| 100ms clipboard probe is too short on slow machines, missing the selection copy. | Constant is centralized; if reports come in, bump to 150ms or add a short retry loop. Document the constant inline. |
| Restore not called on `kill -9` leaves Caps Lock remapped after exit. | Cannot intercept SIGKILL. Document manual reset command in the README/log; users see a clear `INFO [hidutil] to reset manually run: hidutil property --set '{"UserKeyMapping":[]}'` at startup. |
| F18 already used by another app for a global shortcut. | Acceptable: Caps Lock now triggers that shortcut + ours. Document as a known caveat. The chance is low — F18 is typically unused on Apple keyboards. |
| Suppressing F18 returns `true` but downstream apps still see the event. | `CGEventTapCallback` returning `nil` from C-side filter (i.e. setting `*event = NULL`) is the documented suppression path; mirror what the existing convert_selection path does to suppress its trigger. |
| Concurrent Caps Lock presses spawn overlapping goroutines. | `replacing` atomic CAS at the top of `handleCapsLock` short-circuits re-entry; combined with autorepeat filtering this is safe. |
| `hidutil` requires elevated privileges in some configurations. | If the call fails with permission error, log `WARN` and continue without the remap; the existing fallback path keeps the app usable. |

## Rollback Plan

If anything regresses after merging this change:

1. Revert the three edited files (`main.go`, `hook_darwin.go`, `hook_windows.go`).
2. Run `hidutil property --set '{"UserKeyMapping":[]}'` once to clear any leftover remap from a crashed run.
3. Reintroduce the previous `convert_selection` / `convert_last_word` config entries if needed.
