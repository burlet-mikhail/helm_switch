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

## 2. Как macOS сообщает о Caps Lock

Caps Lock в macOS — **не** обычный `kCGEventKeyDown`. Это `kCGEventFlagsChanged` (тип 12). Keycode Caps Lock = `0x39` (57). При нажатии выставляется флаг `NX_ALPHASHIFTMASK` (1 << 16 = 0x10000).

### Текущая проблема

`hook_darwin.go` перехватывает только `kCGEventKeyDown`:

```c
CGEventMask mask = (1 << kCGEventKeyDown);
```

Caps Lock **не попадает** в текущий event tap.

### Решение

Расширить маску событий, добавив `kCGEventFlagsChanged`:

```c
CGEventMask mask = (1 << kCGEventKeyDown) | (1 << kCGEventFlagsChanged);
```

---

## 3. Детекция нажатия Caps Lock

В `goKeyCallback` / `onKeyEvent` нужно отличить Caps Lock от других modifier-only событий (Shift, Cmd и т.д.).

Алгоритм:

1. Событие `kCGEventFlagsChanged` с keycode `0x39` — это Caps Lock
2. Реагируем **только на нажатие** (флаг `NX_ALPHASHIFTMASK` появился), игнорируем отпускание
3. **Подавляем событие** (return NULL) — чтобы Caps Lock не переключал регистр в системе

### Подавление стандартного поведения Caps Lock

При подавлении через event tap (return NULL) — Caps Lock LED может не выключиться / вести себя непредсказуемо. Два подхода:

- **Вариант A (рекомендуемый):** Использовать `hidutil` или IOKit для ремаппинга Caps Lock → no-op на уровне системы, а в event tap ловить `0x39` чисто для логики. Но это требует действий от пользователя.
- **Вариант B (реалистичный):** Подавлять через event tap. LED будет toggle-иться, но функционально это не мешает. Документировать рекомендацию: "Переназначьте Caps Lock на No Action в System Settings > Keyboard > Modifier Keys".

**Выбор: Вариант B** — проще реализовать, LED-toggle не критичен.

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

| Что | Изменение |
|-----|-----------|
| Event mask | Добавить `kCGEventFlagsChanged` в маску |
| `eventCallback` | Обрабатывать `kCGEventFlagsChanged`: извлекать keycode и flags, вызывать `goKeyCallback` с дополнительным параметром типа события |
| `goKeyCallback` | Новая сигнатура: добавить параметр `eventType` чтобы отличать key-down от flags-changed |
| `onKeyEvent` | Новая сигнатура: добавить `eventType` |

### `main.go`

| Что | Изменение |
|-----|-----------|
| Caps Lock handler | Новый блок в `onKeyEvent`: если `eventType == kCGEventFlagsChanged && keycode == 0x39 && (flags & capsLockMask) != 0` → вызвать `handleCapsLock()` |
| `handleCapsLock()` | Новая функция: определяет наличие выделения, делегирует в нужный путь |
| Удалить hotkey checks | Убрать блоки проверки `lastWordHotkey` и `selectionHotkey` |
| Удалить `ParsedHotkeys()` вызов | Больше не нужен в `main()` |

### `config.go`

| Что | Изменение |
|-----|-----------|
| `HotkeyConfig` | Удалить struct целиком |
| `Config.Hotkeys` | Удалить поле |
| `ParseHotkey()` | Удалить функцию |
| `ParsedHotkeys()` | Удалить метод |
| Hotkey constants | Удалить `defaultHotkeyConvertSelection`, `defaultHotkeyConvertLastWord` |
| Modifier/key maps | Удалить `modifierMap`, `keyMap` |
| `DefaultConfig()` | Убрать `Hotkeys` из дефолтов |
| `LoadConfig()` | Убрать backfill логику для hotkeys |

### `config.yaml`

Убрать секцию `hotkeys` целиком. Старые конфиги с секцией `hotkeys` — YAML-парсер просто проигнорирует неизвестные поля (уже работает так).

---

## 7. Новые константы

```go
const (
    kCGEventFlagsChanged = 12
    capsLockKeyCode      = 0x39  // 57
    capsLockMask   int64 = 1 << 16  // NX_ALPHASHIFTMASK / kCGEventFlagMaskAlphaShift
)
```

---

## 8. Edge Cases

| Кейс | Поведение |
|------|-----------|
| Caps Lock зажат долго (key repeat) | `kCGEventFlagsChanged` не повторяется — сработает ровно 1 раз |
| Caps Lock + другой modifier (Cmd+CapsLock) | Игнорировать — реагировать только на чистый Caps Lock |
| Пустой буфер + нет выделения | No-op, Caps Lock подавлен |
| Выделение в терминале (не поддерживает Cmd+C) | Clipboard не изменится → fallback на последнее слово. Приемлемо |
| Приложение из excluded_apps | Caps Lock конвертация работает всегда (как текущие хоткеи — checked before enabled gate) |
| Toggle (повторное нажатие) | Работает только для режима "последнее слово". Для selection — нет toggle |

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

3. **Caps Lock LED** — будет toggle-иться даже при подавлении события. Рекомендация пользователю: переназначить Caps Lock в System Settings.

---

## 11. Тестирование

- **Unit tests:** проверка определения `kCGEventFlagsChanged` + keycode 0x39
- **Удалить тесты** для `ParseHotkey()` (функция удалена)
- **Manual testing:**
  - Набрать слово на неправильной раскладке → Caps Lock → слово конвертировано
  - Повторный Caps Lock → откат (toggle)
  - Выделить текст → Caps Lock → выделение конвертировано
  - Caps Lock в excluded app → конвертация все равно работает
  - Caps Lock при пустом буфере и без выделения → ничего не происходит
