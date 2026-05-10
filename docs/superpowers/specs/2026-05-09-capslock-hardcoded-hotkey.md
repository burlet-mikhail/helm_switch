# RuSwitch: Caps Lock as Hardcoded Conversion Hotkey

**Date:** 2026-05-09
**Status:** Draft

## Summary

Caps Lock becomes единственной горячей клавишей для конвертации. Поведение зависит от контекста:

- **Нет выделения** — конвертирует последнее набранное слово (аналог текущего `convert_last_word`)
- **Есть выделение** — конвертирует весь выделенный текст (аналог текущего `convert_selection`)

Горячая клавиша хардкодится — убирается из конфигурации. Оба конфигурируемых хоткея (`convert_selection`, `convert_last_word`) заменяются одним Caps Lock.

---

## 1. Почему Caps Lock

- Caps Lock — бесполезная клавиша для двуязычного пользователя, идеально подходит для переназначения
- Одна клавиша вместо двух хоткеев — проще запомнить
- Контекстное поведение (выделение vs. последнее слово) интуитивно

---

## 2. Почему нельзя перехватить Caps Lock через CGEventTap

Caps Lock на macOS обрабатывается на уровне IOKit HID **до** event tap. Возврат NULL из CGEventTap callback **не подавляет** Caps Lock — OS всё равно:
- Переключает состояние Caps Lock (LED, регистр)
- Может показать переключатель раскладки (если настроен)
- Отправляет `kCGEventFlagsChanged` чисто как уведомление, а не как перехватываемое событие

Попытка ловить `kCGEventFlagsChanged` + keycode `0x39` и подавлять — **не работает**.

---

## 3. Решение: hidutil remap Caps Lock → F18

### Подход

Используем `hidutil` для ремаппинга Caps Lock → F18 на уровне HID-драйвера. После ремаппинга:
- Физическая клавиша Caps Lock генерирует **обычный `kCGEventKeyDown`** с keycode F18 (`0x4F` = 79)
- Caps Lock LED **не загорается** — ОС не знает что нажат Caps Lock
- Event tap может **полностью подавить** F18 через return NULL
- Никаких побочных эффектов (переключатель раскладки, смена регистра)

### Команды hidutil

**Установка ремаппинга** (Caps Lock → F18):
```bash
hidutil property --set '{"UserKeyMapping":[{"HIDKeyboardModifierMappingSrc":0x700000039,"HIDKeyboardModifierMappingDst":0x70000006D}]}'
```

- `0x700000039` = USB HID usage code для Caps Lock
- `0x70000006D` = USB HID usage code для F18

**Сброс ремаппинга** (восстановление Caps Lock):
```bash
hidutil property --set '{"UserKeyMapping":[]}'
```

### Жизненный цикл

1. **При старте** (`main()`, до `startHook()`) — установить ремаппинг через `exec.Command("hidutil", ...)`
2. **При завершении** (signal handler + `defer`) — сбросить ремаппинг
3. Ремаппинг `hidutil` действует до перезагрузки и не требует root — идеально для user-space приложения

### Важно: сохранение существующих ремаппингов

Перед установкой нужно прочитать текущие `UserKeyMapping` через `hidutil property --get "UserKeyMapping"`, добавить наш ремаппинг к существующим (если есть), и при сбросе — восстановить оригинальные, а не очищать всё.

---

## 3a. Детекция F18 в event tap

После ремаппинга Caps Lock приходит как **обычный `kCGEventKeyDown`** — не нужен `kCGEventFlagsChanged`:

```go
const f18KeyCode uint16 = 0x4F // 79 — macOS virtual keycode для F18
```

В `onKeyEvent`: если `eventType == kCGEventTypeKeyDown && keycode == f18KeyCode` → вызвать `handleCapsLock()`, return true (suppress).

### Что упрощается

- Event mask — **только `kCGEventKeyDown`** (как было до этой фичи)
- `goKeyCallback` / `onKeyEvent` — **не нужен параметр `eventType`** (можно вернуть к оригинальной сигнатуре)
- Не нужны константы `kCGEventTypeFlagsChanged`, `capsLockMask`, `anyModifierMask`

---

## 4. Определение наличия выделения

При нажатии Caps Lock нужно понять: есть ли у пользователя выделенный текст. Алгоритм:

1. Сохранить текущий clipboard
2. Отправить Cmd+C (скопировать)
3. Подождать ~100ms
4. Прочитать clipboard
5. **Если clipboard изменился** — есть выделение → конвертировать выделенный текст
6. **Если clipboard не изменился** — нет выделения → конвертировать последнее слово из буфера

Это re-use текущей логики `convertSelectedText()`, которая уже делает шаги 1-4.

### Объединенный обработчик

```go
func handleCapsLock() {
    savedClipboard := readClipboard()

    atomic.StoreInt32(&replacing, 1)
    sendCopy()
    time.Sleep(100 * time.Millisecond)

    selected := readClipboard()

    if selected != "" && selected != savedClipboard {
        // Есть выделение — конвертируем selection
        convertAndPasteSelection(selected, savedClipboard)
    } else {
        // Нет выделения — конвертируем последнее слово
        atomic.StoreInt32(&replacing, 0)
        if savedClipboard != "" {
            writeClipboard(savedClipboard)
        }
        convertLastWordFromBuffer()
    }
}
```

---

## 5. Поведение

### Caps Lock без выделения (последнее слово)

Полностью повторяет текущую логику `convert_last_word`:

1. `buf.LastWord()` — получить последнее слово
2. Если пусто — no-op
3. Эвристика направления: кириллица → RU→QWERTY, иначе QWERTY→RU
4. Удалить слово backspace-ами, набрать конвертированное
5. Переключить раскладку (`switchLang()`)
6. Вооружить toggle-window (`lastWord.Set(current, converted)`)

**Toggle:** повторное нажатие Caps Lock (без набора между ними) — откат к исходному тексту. Идентично текущему поведению.

### Caps Lock с выделением

Полностью повторяет текущую логику `convert_selection`:

1. Cmd+C → получить выделенный текст
2. Конвертировать (эвристика направления)
3. Записать в clipboard → Cmd+V (вставить)
4. Восстановить оригинальный clipboard
5. Переключить раскладку

---

## 6. Изменения в коде

### `hook_darwin.go`

| Что                  | Изменение                                                                                            |
|----------------------|------------------------------------------------------------------------------------------------------|
| Event mask           | **Вернуть** к `(1 << kCGEventKeyDown)` — убрать `kCGEventFlagsChanged`                              |
| `eventCallback`      | **Вернуть** к оригиналу — обрабатывать только `kCGEventKeyDown`                                     |
| `goKeyCallback`      | **Вернуть** оригинальную сигнатуру: `(keycode, character, flags)` — убрать `eventType`               |
| `onKeyEvent`         | **Вернуть** оригинальную сигнатуру: `(keycode uint16, char rune, flags int64) bool`                  |
| Убрать константы     | Удалить `kCGEventTypeFlagsChanged`, `capsLockKeyCode`, `capsLockMask`, `anyModifierMask`             |
| Новая константа      | `f18KeyCode uint16 = 0x4F` (79)                                                                     |

### `main.go`

| Что                       | Изменение                                                                                       |
|---------------------------|-------------------------------------------------------------------------------------------------|
| `remapCapsLockToF18()`    | Новая функция: вызывает `hidutil` для ремаппинга Caps Lock → F18                               |
| `restoreCapsLock()`       | Новая функция: сбрасывает ремаппинг (вызывается в signal handler + defer)                       |
| `main()` init             | Вызвать `remapCapsLockToF18()` перед `startHook()`, добавить `defer restoreCapsLock()`          |
| Signal handler            | Добавить `restoreCapsLock()` перед `os.Exit(0)`                                                |
| F18 handler в `onKeyEvent`| Если `keycode == f18KeyCode` → `go handleCapsLock(buf, detector)`, return true                  |
| `onKeyEvent` сигнатура    | Вернуть к `(keycode uint16, char rune, flags int64) bool` — без `eventType`                     |

### `config.go`

Без изменений (уже вычищен в предыдущей итерации).

### `hook_windows.go`

| Что                  | Изменение                                                                              |
|----------------------|----------------------------------------------------------------------------------------|
| `onKeyEvent`         | Вернуть оригинальную сигнатуру без `eventType`                                         |
| Убрать константы     | Удалить `kCGEventTypeKeyDown`, `kCGEventTypeFlagsChanged`, `capsLockKeyCode` и т.д.    |
| Новая константа      | `f18KeyCode uint16 = 0x4F` (dead code на Windows, но нужен для компиляции `main.go`)   |

### `config.yaml`

Без изменений (секция `hotkeys` уже убрана).

---

## 7. Новые константы и функции

```go
const f18KeyCode uint16 = 0x4F // 79 — macOS virtual keycode для F18

// remapCapsLockToF18 вызывает hidutil для ремаппинга Caps Lock → F18.
// Caps Lock (USB HID 0x700000039) → F18 (USB HID 0x70000006D).
func remapCapsLockToF18() error {
    cmd := exec.Command("hidutil", "property", "--set",
        `{"UserKeyMapping":[{"HIDKeyboardModifierMappingSrc":0x700000039,"HIDKeyboardModifierMappingDst":0x70000006D}]}`)
    return cmd.Run()
}

// restoreCapsLock сбрасывает hidutil ремаппинг.
func restoreCapsLock() {
    exec.Command("hidutil", "property", "--set",
        `{"UserKeyMapping":[]}`).Run()
}
```

---

## 8. Edge Cases

| Кейс | Поведение |
|------|-----------|
| F18 зажат долго (key repeat)                       | F18 как обычная клавиша может генерировать repeat — проверить `isRepeat` flag (bit 0 CGEventFlags), либо не проблема если handleCapsLock идемпотентна |
| Caps Lock + другой modifier (Cmd+CapsLock)         | После ремаппинга это Cmd+F18 — проверять `(flags & allModsMask) == 0`, реагировать только на чистый F18 |
| Пустой буфер + нет выделения                       | No-op, F18 подавлен                                                                                   |
| Выделение в терминале (не поддерживает Cmd+C)       | Clipboard не изменится → fallback на последнее слово. Приемлемо                                        |
| Приложение из excluded_apps                         | Конвертация работает всегда (checked before enabled gate)                                              |
| Toggle (повторное нажатие)                          | Работает только для режима "последнее слово". Для selection — нет toggle                                |
| hidutil не установлен / ошибка                      | Логировать warning, продолжить без Caps Lock хоткея — приложение работает, просто без ручной конвертации |
| Пользователь уже имеет свои hidutil ремаппинги      | Читать существующие маппинги, добавлять наш, при выходе — восстанавливать оригинальные                 |
| Крэш без вызова restoreCapsLock()                   | Caps Lock останется как F18 до перезагрузки. Документировать: `hidutil property --set '{"UserKeyMapping":[]}'` для ручного сброса |

---

## 9. Что НЕ меняется

| Файл | Причина |
|------|---------|
| `detector.go` | Логика детекции не затронута |
| `dict.go` | Словарь не меняется |
| `keymap.go` | Маппинг символов не меняется |
| `replacer_darwin.go` | Низкоуровневый ввод не меняется |
| `exceptions.go` | Исключения не затронуты |
| `rollback.go` | Rollback tracker не затронут |
| `buffer.go` | `LastWord()` и буфер без изменений |

---

## 10. Риски

1. **100ms задержка на определение выделения** — при каждом нажатии Caps Lock будет ощутимая задержка (Cmd+C + ожидание). Mitigation: можно уменьшить до 50ms и проверить стабильность.

2. **Побочный эффект Cmd+C без выделения** — в некоторых приложениях Cmd+C без выделения может давать side-effect (например, копировать текущую строку в IDE). Mitigation: проверить на целевых приложениях.

3. **Крэш оставляет Caps Lock заремапленным** — если приложение крэшнет без вызова `restoreCapsLock()`, Caps Lock останется F18 до перезагрузки. Mitigation: документировать команду ручного сброса, рассмотреть launchd watchdog.

4. **Конфликт с существующими hidutil ремаппингами** — пользователь может уже иметь свои ремаппинги (например Karabiner). Mitigation: читать текущие маппинги перед изменением, мержить, восстанавливать при выходе.

---

## 11. Тестирование

- **Unit tests:** проверка `remapCapsLockToF18()` / `restoreCapsLock()` (mock exec)
- **Manual testing:**
  - `hidutil property --get "UserKeyMapping"` — проверить что ремаппинг установлен при запуске
  - Набрать слово на неправильной раскладке → Caps Lock → слово конвертировано
  - Повторный Caps Lock → откат (toggle)
  - Выделить текст → Caps Lock → выделение конвертировано
  - Caps Lock в excluded app → конвертация все равно работает
  - Caps Lock при пустом буфере и без выделения → ничего не происходит
  - Caps Lock LED **не загорается** при нажатии
  - Завершить приложение (Ctrl+C) → `hidutil property --get "UserKeyMapping"` пуст → Caps Lock работает нормально
  - Kill -9 приложения → Caps Lock остается F18 → ручной сброс через `hidutil property --set '{"UserKeyMapping":[]}'`
