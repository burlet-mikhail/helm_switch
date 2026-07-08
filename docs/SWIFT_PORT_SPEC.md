# Helm Switch — спецификация для нативного Swift/macOS порта

Документ описывает **полное поведение** текущего приложения (Go + cgo) так, чтобы его
можно было переписать на Swift/AppKit без потери функциональности и — что критично —
**без потери выстраданных багфиксов**. Идентификаторы, имена API, константы и keycodes
приведены в оригинальном виде.

Источник истины — текущий код в репозитории. При расхождении спецификации и поведения
существующего приложения — приоритет у наблюдаемого поведения (оно протестировано
вручную пользователем).

---

## 1. Назначение

Helm Switch — фоновая утилита в строке меню macOS, исправляющая текст, набранный
в **неправильной раскладке** (русская ⇄ английская). Два режима:

1. **Авто-конвертация** — пока пользователь печатает, по границе слова (пробел/пунктуация/Enter)
   приложение определяет «слово набрано не в той раскладке» и заменяет его на лету.
2. **Ручной хоткей (Caps Lock)** — пользователь выделяет текст или ставит курсор у слова и
   жмёт Caps Lock; приложение конвертирует выделение/слово и переключает системную раскладку.

Не-цели: перевод смысла текста, поддержка раскладок кроме RU/EN, работа без прав Accessibility.

---

## 2. Платформа и фреймворки

- **Только macOS 12+** (текущий `LSMinimumSystemVersion = 12.0`). Windows-ветка (`*_windows.go`)
  в Swift-порте **выбрасывается**.
- Приложение — **agent / LSUIElement** (без иконки в Dock, `NSApplicationActivationPolicyAccessory`).
- Требует **Accessibility** (`AXIsProcessTrusted`) — без него event tap не создаётся.
- Используемые системные API (всё имеет прямой Swift-аналог):
  - **CoreGraphics** — `CGEvent`, `CGEventTap` (перехват и синтез клавиатуры).
  - **ApplicationServices / HIServices** — `AXUIElement*` (чтение выделения/каретки).
  - **Carbon (TIS)** — `TISInputSource*` (текущая раскладка и переключение).
  - **AppKit** — `NSPasteboard`, `NSStatusItem`, `NSWorkspace`, `NSApplication`.
  - **hidutil** (CLI через `Process`) — ремап Caps Lock → F18 на HID-уровне.
- Внешние зависимости Go, которые надо заместить:
  - `github.com/kljensen/snowball` (стеммер RU/EN) → найти Swift-стеммер Snowball **или**
    предвычислить стемы офлайн (см. §6).
  - `gopkg.in/yaml.v3` (конфиг) → заменить на JSON/`plist` + `Codable` (рекомендуется отказаться от YAML).

---

## 3. Архитектура и компоненты

| Модуль (Go) | Ответственность | Swift-эквивалент |
|---|---|---|
| `keymap.go` | Карты QWERTY⇄RU, классификация пунктуации | статические `[Character:Character]` |
| `dict.go` | Загрузка частотных словарей, стемминг, fuzzy | `Dict` + ресурсы bundle |
| `detector.go` | Определение «неправильная раскладка → исправление» | `Detector` |
| `buffer.go` | Накопление набранных символов, границы слов | `Buffer` (actor/lock) |
| `hook_darwin.go` | CGEventTap, фильтр autorepeat, suppress | `EventTap` |
| `replacer_darwin.go` | Синтез клавиш, буфер обмена, AX, TIS | `KeyboardOutput`, `Accessibility`, `LayoutSwitcher` |
| `appid_darwin.go` | Frontmost bundle id (NSWorkspace observer) | `FrontmostApp` |
| `tray_darwin.{go,m}` | NSStatusItem, флаг-эмодзи, меню | `StatusItemController` (SwiftUI/AppKit) |
| `rollback.go` | Машина состояний «пользователь откатил» | `RollbackTracker` |
| `exceptions.go` | Персистентный список исключений (JSON) | `ExceptionStore` (`Codable`) |
| `config.go` | Конфиг | `Config` (`Codable`) |
| `main.go` | Оркестрация, onKeyEvent, handleCapsLock, lifecycle | `AppController` |

**Главный инвариант потоков:** обработчик event tap вызывается синхронно на потоке tap'а;
он обязан возвращаться **мгновенно**. Любой синтез клавиш / I/O буфера обмена / AX уходит в
отдельную задачу (в Go — горутина; в Swift — `DispatchQueue`/`Task`).

---

## 4. Карты раскладки (`keymap.go`) — перенести дословно

`enToRu` (QWERTY→RU), нижний и верхний регистр; `ruToEn` строится как обратная карта в `init()`.

```
q→й w→ц e→у r→к t→е y→н u→г i→ш o→щ p→з [→х ]→ъ
a→ф s→ы d→в f→а g→п h→р j→о k→л l→д ;→ж '→э
z→я x→ч c→с v→м b→и n→т m→ь ,→б .→ю `→ё \→ё
```
(плюс заглавные: `Q→Й … {→Х }→Ъ … :→Ж "→Э … <→Б >→Ю ~→Ё`)

Классификации (используются и детектором, и определением границ слова):

- `qwertyRuPunct` = `, . ; ' [ ] ` \` — QWERTY-пунктуация, которая на самом деле русские буквы
  (НЕ границы слова).
- `shiftedRuPunct` = `^→, &→. $→; @→" #→№` — Shift+цифры на EN, которые юзер набрал, думая что он в RU.
- `universalPunct` = `! ?` — одинаковая клавиша на обеих раскладках (границы слова, но сохраняются
  как хвостовая пунктуация).

`QWERTYToRussian(s)` / `RussianToQWERTY(s)`: посимвольно по карте; **символ без сопоставления
проходит насквозь без изменений** (пробелы, цифры, неизвестная пунктуация). Это делает
конверсию биекцией на сопоставленном наборе и **обратимой** — критично для отката хоткеем
(повторное нажатие Caps само возвращает слово). Сохранить это свойство.

---

## 5. Детектор (`detector.go`) — алгоритм `Check(text) -> (wrong, corrected)`

Перенести дословно, включая порядок проверок (он определяет качество и ложные срабатывания).

1. Сбрасывает `trailingPunct = 0`.
2. **Однобуквенные слова**: если `text` есть в `singleLetterRu` (`f→а d→в j→о r→к c→с z→я b→и e→у`)
   И (последняя замена была RU, либо детектор ещё не инициализирован) → конвертировать.
3. `detectScript(text)` по первому буквенному символу: `cyrillic` / `latin` / `unknown`
   (Unicode-категории `Cyrillic`/`Latin`).
4. **latin**:
   - `converted = QWERTYToRussian(text)`; `inRu = ruDict.Has(converted)`;
     `inEnExact = enDict.words[lower(text)]` (точное совпадение, без стемминга).
   - **Хвостовая пунктуация** (только для слов длиной > 2), `lastChar` = последний символ,
     `trimConv = QWERTYToRussian(text без последнего)`:
     - `shiftedRuPunct[lastChar]` → если `ruDict.Has(trimConv)`: вернуть `trimConv`, `trailingPunct = ruPunct`.
     - `universalPunct[lastChar]` (`! ?`) → если `ruDict.Has(trimConv)`: вернуть `trimConv`, `trailingPunct = lastChar`.
     - `qwertyRuPunct[lastChar]` (`, . ; '` …) → неоднозначно (буква vs пунктуация). Предпочесть
       trim, если `ruDict.Has(trimConv)` и (`!inRu` или длина обрезанного ≤ 2);
       если обрезанное валидно и длиннее 2 — тоже trim. (Точную ветвистую логику см. в коде, перенести как есть.)
   - Если `inRu`:
     - если `inEnExact && !IsRussianLayout()` → это настоящее английское слово на EN-раскладке
       (`if`, `the`, `and`, `no`) → **не трогать**.
     - иначе → конвертировать в `converted`.
   - **Fuzzy** (только слова ≥ 6 рун и `!inEnExact`): `ruDict.FuzzyFind(converted)` (1 правка) → исправить.
5. **cyrillic**:
   - если `ruDict.Has(text)` → валидное русское, **не трогать**.
   - иначе `converted = RussianToQWERTY(text)`; если `enDict.Has(converted)` → это английское,
     набранное в RU → конвертировать.
6. Иначе — не трогать.

`Detector` хранит состояние `lastLangRu`/`initialized` (контекст для однобуквенных слов).
`trailingPunct` читается вызывающей стороной сразу после `Check` (см. §7, авто-замена).

`IsRussianLayout()` — TIS: текущий input source содержит «Russian» (регистронезависимо).

---

## 6. Словари (`dict.go`)

- Ресурсы: `dicts/ru_freq.txt` (~98k слов), `dicts/en_freq.txt` (~9.8k). В Swift — bundle resources;
  грузить в `Set<String>` (lowercased), плюс множество стемов.
- `Dict.Has(word)`:
  1. точное совпадение по `words`;
  2. совпадение по Snowball-стему (`stems`);
  3. для RU — **fuzzy по суффиксам**: перебор `ruSuffixes`, отрезание суффикса, варианты
     с чередованием согласных (`consonantAlts`: ж↔з/г/д, ш↔с/х, ч↔к/ц, щ↔ст/ск, …),
     проверка `root + инфинитивные окончания` (`ть ать ять ить еть уть нуть овать зать сать`)
     и `root` как основа существительного/прилагательного (`root`, `+а +о +ь +ка`).
     Перенести таблицы `ruSuffixes`/`consonantAlts` дословно.
- `Dict.FuzzyFind(word)` — возвращает слово в пределах 1 правки, **в порядке**: вставки →
  замены → удаления (алфавит `абвгдеёжзийклмнопрстуфхцчшщъыьэюя`). Порядок важен (вставка — самая
  частая опечатка). Использует `Has` для проверки кандидата.
- **Стемминг**: текущий код использует Snowball (`russian.Stem`, `english.Stem`).
  Варианты в Swift: (а) подключить Swift/C-порт Snowball; (б) **предвычислить стемы офлайн**
  и положить рядом со словарём (`*_stems.txt`) — убирает зависимость в рантайме. Рекомендуется (б).

---

## 7. Буфер и авто-конвертация

### 7.1 Буфер (`buffer.go`)
- `chars []rune` — текущее набираемое слово; `lastFlushed []rune` — последнее «закрытое» слово.
- `Add(r)`:
  - если `r` — граница слова (`isWordBoundary`):
    - если `universalPunct[r]` и буфер непуст → дописать `r` в слово, эмитить слово (с пунктуацией), сбросить;
    - иначе если буфер непуст → эмитить слово (без `r`), сбросить;
  - иначе дописать `r` в `chars`.
- `isWordBoundary(r)`: пробел → да; буква/цифра → нет; `qwertyRuPunct[r]` → нет; `shiftedRuPunct[r]` → нет;
  иначе → да.
- `Backspace()` убирает последний символ из `chars`. `Clear()` очищает `chars` (НЕ `lastFlushed`).
- `LastWord()` = `chars` если непуст, иначе `lastFlushed`. `IsBufferEmpty()` = `len(chars)==0`.
- Колбэк `onWord` вызывается **синхронно после** освобождения мьютекса (иначе дедлок: колбэк зовёт `Clear`).

> **ВАЖНО (фикс):** буфер **очищается на жестах** мыши/стрелок/Cmd (см. §9.2), чтобы авто-конвертация
> не срабатывала через прыжок курсора. Ручной хоткей буфер больше **не использует** (читает документ через AX).

### 7.2 Авто-конвертация по границе слова (`onWord`, в `main.go`)
Колбэк выходит сразу, если: `!cfg.Enabled` ИЛИ `replacing==1` ИЛИ `!isAutoConvertEnabled()`.
Затем:
1. длина слова < `cfg.MinWordLength` (по умолчанию 2) → выход;
2. `looksLikeContext(word)` (URL/email/путь/идентификатор) → выход;
3. `cfg.IsAppExcluded(FrontmostAppID())` → выход;
4. `store.IsException(app, word)` → выход (пользователь это слово уже откатывал);
5. `detector.Check(word)` → если не wrong, выход;
6. с учётом `detector.trailingPunct` вызвать `doReplace` с правильным числом удаляемых символов и текстом
   (3 случая: без хвостовой пунктуации; пунктуация-как-`universalPunct`; обычная — см. код).

`doReplace` = `undo.Save` + `tracker.OnConversion` + `replaceText` (см. §10).

`looksLikeContext(word)`: `true` если содержит `@ / \ : _` (URL/почта/путь/идентификатор);
для `- .` — считает только при `≥1 точка и есть цифра`, либо `≥2 разделителя`. Перенести как есть.

### 7.3 Авто-конвертация по Enter
Enter «флашит» буфер (`FlushWord`). Если включено и слово проходит фильтры (min length, context,
excluded, exception, `Check`) — в отдельной горутине: `replacing=1`, удалить N символов
(+5 для search-apps, см. §10), напечатать исправление, `switchLang`, `replacing=0`, затем `sendEnter`.
Иначе Enter проходит как есть.

---

## 8. Глобальный хоткей Caps Lock — HID remap + Event Tap

### 8.1 Ремап Caps Lock → F18 (`hidutil`)
Caps Lock сам по себе перехватывать неудобно; вместо этого на старте ставится HID-ремап
**Caps Lock (`0x700000039`) → F18 (`0x70000006D`)** через `hidutil property --set`.

- `remapCapsLockToF18`:
  1. `hidutil` нет в PATH → предупредить, продолжить без хоткея.
  2. Прочитать текущий `UserKeyMapping` (`hidutil property --get`), распарсить (`parseHidutilUserKeyMapping`,
     принимает и hex `0x…`, и десятичные значения).
  3. **Защита:** если из непустого вывода распарсилось 0 записей И вывод не «пустой список»
     (`looksLikeEmptyHidutilList`: `()`, `(\n)`, `(null)` с любыми пробелами) → **не ставить** ремап
     (иначе при восстановлении затрём пользовательские маппинги).
  4. Смержить: убрать существующую запись Caps Lock, добавить свою (Caps→F18), записать обратно.
  5. Запомнить `originalUserKeyMapping` (для восстановления) только **после** успешного `--set`.
  6. Атомарно выставить `capsLockRemapped = 1`.
- `restoreCapsLock`: атомарный CAS `capsLockRemapped 1→0` (ровно один раз из нескольких путей
  завершения); восстановить `UserKeyMapping` в исходный вид. **Должен вызываться на ВСЕХ путях
  выхода** (defer, SIGINT/SIGTERM, tray-quit, fatal-fast-path) — иначе Caps останется F18 до перезагрузки.

### 8.2 Event Tap (`hook_darwin.go`)
- Маска: `kCGEventKeyDown | kCGEventLeftMouseDown`. Tap = `kCGSessionEventTap`,
  `kCGHeadInsertEventTap`, `kCGEventTapOptionDefault`.
- При `kCGEventTapDisabledByTimeout`/`…ByUserInput` → **переподключить** (`CGEventTapEnable(tap, true)`)
  и вернуть событие (самовосстановление).
- Для `kCGEventKeyDown`:
  - **autorepeat F18 фильтруется в C** (`kCGKeyboardEventAutorepeat && keycode==0x4F` → вернуть `NULL`,
    то есть подавить) — иначе удержание Caps вызовет шторм проб.
  - извлечь unicode-символ (`CGEventKeyboardGetUnicodeString`), keycode, flags → `onKeyEvent`.
  - вернуть `NULL` (подавить) если `onKeyEvent` вернул true, иначе пропустить.
- `goKeyCallback`: **если `replacing==1` → сразу вернуть 0 (пропустить, не входя в `onKeyEvent`)** —
  это не даёт нашим синтетическим событиям (Cmd+C/V, Option+Shift+Left, backspace, ввод) повторно
  войти в обработчик. Ключевой инвариант.
- Left mouse down → `onMouseEvent` (см. §9.2).
- Tap создаётся в фоне (свой run loop), `CFRunLoopRun`. NSApp — на главном потоке.

Константы: `f18KeyCode = 0x4F`; `kCGEventFlagMaskCommand = 1<<20`;
`anyRealModifierMask = (1<<17)|(1<<18)|(1<<19)|(1<<20)` (Shift|Control|Option|Command).

---

## 9. `onKeyEvent` — диспетчер клавиш (`main.go`)

Вызывается синхронно из tap. Возвращает true = подавить событие.

### 9.1 Caps Lock (F18)
```
if keycode == f18KeyCode && (flags & anyRealModifierMask) == 0 {
    go handleCapsLock(buf)   // в фоне
    return true              // подавить F18
}
```
`(flags & anyRealModifierMask) == 0` — Cmd+CapsLock / Shift+CapsLock и т.п. **пропускаются** в систему.

### 9.2 Накопление буфера и инвалидация (для авто-конвертации)
- **Backspace** (`0x33`): `buf.Backspace()`, `tracker.ObserveKey(Backspace)`, не подавлять.
- **Навигация** (`isNavigationKey`: стрелки `0x7B 0x7C 0x7D 0x7E`, Home `0x73`, End `0x77`,
  PageUp `0x74`, PageDown `0x79`): `buf.Clear()`, не подавлять. *(Фикс: курсор сдвинулся → буфер невалиден;
  плюс не пускаем function-символы стрелок в буфер.)*
- `char == 0 || char == 0x08` → не подавлять (модификаторы и т.п.).
- **Enter/Return** (`0x24`/`0x4C`/`\r`/`\n`): авто-конвертация по Enter (§7.3) либо `tracker.ObserveKey(Other)`.
- **Любая Cmd-комбинация** (`flags & Command`):
  - **Cmd+Z** (`keycode==0x06`) при включённой авто-конвертации → undo (§12).
  - иначе (Cmd+A, Cmd+стрелки, Cmd+C/V, …): `buf.Clear()`, пропустить. *(Фикс: Cmd+A и пр. меняют
    выделение/курсор → буфер невалиден.)*
- **Обычный символ** (без Cmd): `buf.Add(char)`, `tracker.ObserveKey(Char)`, сбросить окно undo.

### 9.3 Мышь
`onMouseEvent = { buf.Clear() }` — *(Фикс: клик переставляет курсор/начинает выделение.)*
**Должен быть назначен** (в старой версии забывали — клики игнорировались).

---

## 10. Замена текста для авто-конвертации (`replaceText`, `replacer_darwin.go`)

Используется авто-конвертацией (НЕ ручным хоткеем). Защищён CAS `replacing 0→1`
(если уже идёт замена — пропустить).
1. `buf.Clear()`; пауза 50ms (дать ОС обработать границу/пробел).
2. Если **search-app** (`isSearchApp`): `sendOptionBackspace()` (Option+Backspace удаляет слово
   целиком, побеждает inline-автодополнение), пауза 50ms.
   Иначе: цикл `deleteChars` раз `sendBackspace()` (каждый ~15ms) + 20ms.
3. Печать `newText` посимвольно `sendUnichar` (5ms между символами).
4. `switchLayout()`; 30ms; `replacing=0`.

`isSearchApp(app)` = bundle id ∈ {`com.apple.systempreferences`, `com.apple.Spotlight`,
`com.raycast.macos`, `com.runningwithcrayons.Alfred`}.

> **Известный компромисс:** в search-app по Enter-пути и в `convertLastWord`-историческом пути
> добавлялось `+5` backspaces для борьбы с автодополнением; это может «съесть» лишние символы,
> если подсказки не было. В ручном хоткее (§11) этот хак НЕ используется — там работаем по выделению.

### 10.1 Синтез ввода (C-функции → Swift `CGEvent`)
- `sendUnichar(ch)`: создать key down/up, `CGEventSetFlags(…, 0)`, задать unicode-строку через
  `CGEventKeyboardSetUnicodeString`; для ASCII-пунктуации использовать **физический keycode**
  (`physicalKeycode`: space `0x31`, `, 0x2B`, `. 0x2F`, `; 0x29`, `' 0x27`, `[ 0x21`, `] 0x1E`, `` ` `` `0x32`),
  иначе keycode 0 — чтобы приложение не переинтерпретировало по текущей раскладке.
- `sendBackspace` (`0x33`): **явно сбросить флаги в 0** на down и up — иначе предыдущий синтетический
  Cmd+C/V оставляет «залипший» Command (CGEvent наследует текущее состояние модификаторов). *(Фикс.)*
- `sendEnter` (`0x24`, flags 0), `sendOptionBackspace` (`0x33` + Alternate),
  `sendCmdBackspace`, `sendCmdC` (`0x08`+Command), `sendCmdV` (`0x09`+Command).
- Между down и up — `usleep(15000)` (15ms). Все события постятся в `kCGHIDEventTap`.

---

## 11. Ручной хоткей `handleCapsLock(buf)` — ЯДРО, со всеми фиксами

Запускается в фоне на каждое нажатие Caps. Источник истины — **реальный документ через AX**,
а НЕ буфер набора. Алгоритм (перенести дословно):

```
0. CAS capsHandling 0→1; если не удалось — выйти (single-flight: не наслаивать синтез нажатий).
   defer capsHandling=0.
1. buf.Clear()  // документ перестаёт совпадать с буфером
2. replacing=1; defer replacing=0  // глушим повторный вход и авто-конвертацию
3. selLen = axSelectionLength()    // длина выделения: 0=нет, >0=есть, -1=AX недоступен
4. ЕСТЬ ВЫДЕЛЕНИЕ?
   a. if sel := axSelectedText(); sel != "" -> convertSelectionInPlace(sel); return  // нативные приложения
   b. if selLen != 0 {  // selLen>0: выделение есть, но строку AX не отдал; selLen==-1: AX недоступен
        if sel := copySelectionViaClipboard(); sel != "" -> convertSelectionInPlace(sel); return
      }
5. ВЫДЕЛЕНИЯ НЕТ — конвертируем токен слева от каретки:
   if selLen == 0 {                  // AX-чтение работает
        selectTokenLeftOfCaret()     // дорастянуть выделение на ВЕСЬ токен без пробелов
        word = axSelectedText()
   } else {                          // AX недоступно (браузер/Electron)
        sendOptionShiftLeft(); sleep 40ms
        word = copySelectionViaClipboard()
   }
   if TrimSpace(word) == "" -> log no-op; return
   convertSelectionInPlace(word)
```

### Почему именно так — обоснование фиксов (НЕ упрощать!)
- **`selLen = axSelectionLength()` ПЕРЕД любым `Option+Shift+Left`.** Если выделение уже есть
  (например Cmd+A), `Option+Shift+Left` НЕ создаёт новое выделение, а **сжимает существующее на одно
  слово справа** → конвертировался «весь текст кроме последнего слова». Поэтому сначала надёжно
  определяем наличие выделения (длина диапазона работает шире, чем чтение строки).
- **Сначала AX-чтение, потом буфер обмена.** Чистая проба `Cmd+C` без выделения в части приложений
  (VS Code и пр.) копирует всю строку → ложное «есть выделение» → вставка строки в каретку
  («абракадабра в начале из буфера»). AX-чтение этого избегает. Клипборд-проба используется
  только когда выделение уже подтверждено (`selLen != 0`) или когда мы сами его создали.
- **Замена ТОЛЬКО вставкой (`convertSelectionInPlace`), без `AXUIElementSetAttributeValue(kAXSelectedText)`.**
  AX-**запись** в части приложений возвращает «успех», но текст не меняет (наблюдалось как
  «выделяет и не переводит», и двойной лог одинаковой конвертации). AX-**чтение** надёжно,
  AX-**запись** — нет. Так как на момент записи выделение всегда есть, вставка надёжна везде.
- **`selectTokenLeftOfCaret` дорастягивает через пунктуацию.** `Option+Shift+Left` («выделить слово»)
  останавливается на пунктуации, поэтому в `hf,jnfkj` (где `б→,`) выделялось только `jnfkj` →
  переводился хвост → `hf,отало`. Решение — расширять выделение по словам, пока добавляемый кусок
  «приклеен» (между ним и токеном нет пробела), и откатить на шаг, если перешагнули реальный пробел.

### `selectTokenLeftOfCaret()` (требует рабочего AX-чтения)
```
prev = ""
for i in 0..<16 {
    sendOptionShiftLeft(); sleep 15ms
    sel = axSelectedText()
    if sel == prev { break }                       // дошли до начала поля/строки
    if prev != "" && sel.hasSuffix(prev) {
        prefix = sel[без хвоста prev]
        if prefix оканчивается пробелом (unicode.IsSpace) {
            sendOptionShiftRight(); sleep 15ms      // перешагнули слово-границу → откат
            break
        }
    }
    prev = sel
}
// выделение оставлено в поле; вызывающий читает axSelectedText() и конвертирует фактически выделенное
```
`sendOptionShiftLeft` = Left (`0x7B`) + Alternate|Shift; `sendOptionShiftRight` = Right (`0x7C`) + Alternate|Shift.

### `convertSelectionInPlace(selected)`
```
converted = convertByHeuristic(selected)
if converted == selected { log "unchanged"; return }   // нечего конвертировать — не трогать
pasteOverSelection(converted)
switchLang(); sleep 30ms
log "[caps] convert: selected → converted"
```

### `convertByHeuristic(s)` — направление конверсии
`containsCyrillic(s)` (есть хоть одна кириллическая буква а-я/А-Я/ё/Ё) → `RussianToQWERTY(s)`,
иначе `QWERTYToRussian(s)`. Пробелы/пунктуация сохраняются (см. §4), поэтому повторный хоткей
переводит обратно.

### `pasteOverSelection(text)` (надёжная замована выделения через буфер обмена)
```
saved = readClipboard()
writeClipboard(text); sleep 30ms
sendPaste()            // Cmd+V заменяет ВЫДЕЛЕНИЕ (insert только если выделения нет — но мы это гарантируем)
sleep 200ms            // дать приложению вставить ДО восстановления клипборда (иначе вставится старое)
writeClipboard(saved)  // восстановить (в т.ч. очистить, если было пусто)
```

### `copySelectionViaClipboard()` (чтение выделения, когда AX-чтение недоступно)
```
saved = readClipboard(); before = clipboardChangeCount()
sendCopy()
poll до ~150ms (15×10ms): если changeCount изменился → sel=readClipboard(); writeClipboard(saved); return sel
return ""
```
Детект копии — по **`NSPasteboard.changeCount`** (а не сравнением строк): надёжно даже если выделенный
текст совпадает с прежним содержимым буфера. *(Фикс.)*

### AX-функции (`replacer_darwin.go`)
- `axCopyFocusedElement` = `AXUIElementCopyAttributeValue(systemWide, kAXFocusedUIElementAttribute)`.
- `axSelectedText` = `kAXSelectedTextAttribute` фокусного элемента (строка UTF-8) или "".
- `axSelectionLength` = длина `kAXSelectedTextRangeAttribute` (`AXValue` типа `kAXValueCFRangeType`):
  `0`/`>0`/`-1` (AX недоступен).

---

## 12. Undo (Cmd+Z) и обучение исключениям

### 12.1 Undo
`undoState{original, replaced, timestamp}`. Окно — **5 секунд**, ровно одна отмена (`Get` потребляет).
По Cmd+Z (если включена авто-конвертация): получить undo; добавить `original` в `ExceptionStore`
(чтобы больше не конвертировать это слово в этом приложении); в фоне: `replacing=1`, удалить
`len(replaced)` символов, напечатать `original`, `switchLang`, `replacing=0`.

### 12.2 Rollback-обучение (`rollback.go`)
Машина состояний, ловит «пользователь вручную откатил нашу конвертацию» по потоку клавиш:
- `OnConversion(original, replaced, app)` → `TRACKING`, окно **10s**.
- `ObserveKey`: backspace’ы доводят до `len(replaced)` → `WAIT_RETYPE`; печать символов, остающихся
  префиксом `original`; при равенстве `original` → `ROLLBACK_DETECTED` → `store.Add(app, original)`.
- Любой сброс: смена раскладки, смена приложения, «прочая» клавиша, таймаут, новая конвертация,
  лишние backspace’ы, несовпадение символа.

### 12.3 Хранилище исключений (`exceptions.go`)
- Путь: `~/Library/Application Support/<AppName>/exceptions.json` (переопределяется `*_CONFIG_DIR`).
- Формат: `{version, updated, entries:[{app, word, added, hit_count}]}`; ключ = `lower(app)\x00lower(word)`;
  глобальное приложение = `"*"`.
- `IsException(app, word)`: точное `(app,word)` ИЛИ глобальное `(*,word)`, инкремент `hit_count`.
- `Add`/`Forget`/`ForgetApp`/`Clear`/`List`. Запись **атомарная** (`.tmp` + rename).
  Битый файл → переименовать в `.corrupt.bak`, начать с пустого. Hard cap 10000 + прунинг записей
  с `hit_count==1` старше 30 дней.
- В Swift — `Codable` + атомарная запись (`Data.write(options: .atomic)`), `RWLock`/`actor`.

---

## 13. Конфиг (`config.go`)

Поля (значения по умолчанию): `Enabled=true`, `PrimaryLanguage="ru"`, `MinWordLength=2`,
`ExcludedApps=["idea"]`, `AutoConvert=true`.
- Путь: `~/Library/Application Support/<AppName>/config.yaml` → **в Swift заменить на JSON/plist**.
- `IsAppExcluded(bundleID)` — регистронезависимое **вхождение подстроки** (`"idea"` матчит
  `com.jetbrains.intellij.idea.ce`).
- Загрузка мержит файл поверх дефолтов; неизвестные ключи игнорируются.

---

## 14. Tray / строка меню (`tray_darwin.*`)

- `NSStatusItem` переменной длины; **заголовок — флаг-эмодзи** текущей раскладки (`🇷🇺`/`🇺🇸`),
  системный шрифт 14pt.
- Обновление флага: подписка на `kTISNotifySelectedKeyboardInputSourceChanged` и legacy
  `AppleSelectedInputSourcesChangedNotification` (`NSDistributedNotificationCenter`); мутация UI —
  всегда на main queue.
- Меню: «**Автоконвертация**» (чекмарк ↔ `autoConvertEnabled`), разделитель, «**Выйти**» (`q`).
- `autoConvertEnabled` — атомарный флаг (старт из `cfg.AutoConvert`); тоггл из меню сохраняет конфиг.
- В Swift — `StatusItemController` (AppKit) или SwiftUI `MenuBarExtra` (macOS 13+). Активационная
  политика `.accessory`.

---

## 15. Lifecycle (`main.go` / `runNSApp`)

1. Парсинг CLI-флагов (см. §19); настройка лог-файла (`TMPDIR/<app>.log`, перезапись).
2. `LoadConfig`; если `!Enabled` — выйти.
3. `NewExceptionStore` (мягкий отказ), `NewRollbackTracker`.
4. `LoadDict("ru")`, `LoadDict("en")` (фатально при ошибке).
5. `NewDetector`; создать `Buffer` с колбэком авто-конвертации; назначить `onKeyEvent`, `onMouseEvent`.
6. `remapCapsLockToF18` (мягкий отказ); `defer restoreCapsLock`.
7. `startHook` — при ошибке (нет Accessibility) → `restoreCapsLock` + завершить с сообщением о выдаче прав.
8. `setAutoConvertEnabled(cfg.AutoConvert)`; `startTray` (ensureApp); `installFrontmostObserver`
   (NSWorkspace — после инициализации NSApplication).
9. Обработчик SIGINT/SIGTERM: `restoreCapsLock` + `os.Exit` (т.к. `os.Exit` пропускает defer).
10. `runAppLoop` (`[NSApp run]`).

**Frontmost app (`appid_darwin.go`):** кэш bundle id, обновляемый из
`NSWorkspaceDidActivateApplicationNotification` на main thread; потокобезопасное чтение
(в Go — atomic-указатель; в Swift — `os_unfair_lock`/atomic). `FrontmostAppID()` читается из любого потока.

---

## 16. Модель конкурентности (перенести гарантии, не реализацию)

| Флаг/состояние | Назначение | Семантика |
|---|---|---|
| `replacing` (atomic int32) | глушит повторный вход и авто-конвертацию во время нашего синтеза | ставится в 1 на время операции; `goKeyCallback` при `==1` не входит в `onKeyEvent` |
| `capsHandling` (atomic int32, **CAS**) | single-flight для `handleCapsLock` | повторное нажатие во время операции отбрасывается |
| `capsLockRemapped` (atomic int32, **CAS**) | ремап поставлен ровно один раз; restore ровно один раз | защищает от двойного `hidutil --set` из разных путей выхода |
| `autoConvertEnabled` (atomic int32) | вкл/выкл авто-режим из меню | независим от ручного хоткея |
| мьютексы | `Buffer`, `undoState`, `RollbackTracker`, `ExceptionStore` | все публичные методы под локом |

В Swift: `replacing`/`capsHandling`/… — `OSAllocatedUnfairLock`-обёртки или `Atomic` (Swift 6),
либо выделенная последовательная очередь. Главное — **сохранить инварианты**, особенно
«пока `replacing==1`, наши синтетические события не попадают в обработчик» и
«`handleCapsLock` не выполняется параллельно сам с собой».

---

## 17. Тайминги (константы — перенести; подбирались эмпирически)

| Где | Пауза | Зачем |
|---|---|---|
| down→up любой синтетической клавиши | 15ms | регистрация нажатия приложением |
| `replaceText`: перед backspace | 50ms | дать ОС обработать границу/пробел |
| `replaceText`: между backspace | ~15ms (+ ещё 20ms после) | надёжное удаление |
| печать символа (авто-замена) | 5ms | стабильность ввода |
| `selectTokenLeftOfCaret`: между расширениями | 15ms | дать AX обновиться |
| `handleCapsLock` fallback `Option+Shift+Left` | 40ms | применение выделения |
| `pasteOverSelection`: после write clipboard | 30ms | буфер обмена готов |
| `pasteOverSelection`: после `Cmd+V` до restore | **200ms** | приложение вставляет ДО восстановления клипборда |
| `copySelectionViaClipboard`: поллинг | 15×10ms (~150ms) | дождаться реального копирования |
| после `switchLang` | 30ms | стабилизация раскладки |
| окно undo | 5s | Cmd+Z отменяет последнюю замену |
| окно rollback-обучения | 10s | поймать ручной откат |

---

## 18. Keycodes и константы (быстрый справочник)

- Виртуальные keycodes: Backspace `0x33`, Return `0x24`, NumpadEnter `0x4C`, Z `0x06`, C `0x08`,
  V `0x09`, Space `0x31`, Cmd `0x37`, Left `0x7B`, Right `0x7C`, Down `0x7D`, Up `0x7E`,
  Home `0x73`, End `0x77`, PageUp `0x74`, PageDown `0x79`, **F18 `0x4F`**.
- HID usage: Caps Lock `0x700000039`, F18 `0x70000006D`.
- Флаги: Command `1<<20`; `anyRealModifierMask = (1<<17)|(1<<18)|(1<<19)|(1<<20)`.
- Физические keycodes пунктуации: см. §10.1.

---

## 19. CLI-флаги (`main.go`)

`-list-exceptions`, `-forget <word>`, `-forget-app <bundle>`, `-clear-exceptions`, `-verbose`
(обрабатываются до инициализации tray/hook и завершают процесс). В Swift — оставить как
launch-аргументы или перенести в окно настроек.

---

## 20. Маппинг Go/cgo → Swift

| Сейчас | Swift |
|---|---|
| `CGEventTapCreate` + C-колбэк | `CGEvent.tapCreate(...)`; колбэк — top-level функция, `self` через `Unmanaged.toOpaque()` в `userInfo` |
| `CGEventCreateKeyboardEvent`/`Post`/`SetFlags`/`KeyboardSetUnicodeString` | `CGEvent(keyboardEventSource:…)`, `.post(tap:)`, `.flags`, `.keyboardSetUnicodeString` |
| `AXUIElement*` | те же символы, импорт `ApplicationServices` |
| `TISCopyCurrentKeyboardInputSource`/`TISSelectInputSource` | те же (Carbon) |
| `NSPasteboard` | `NSPasteboard.general` |
| `NSStatusBar`/`NSStatusItem` | то же, либо SwiftUI `MenuBarExtra` (13+) |
| `NSWorkspace` нотификации | `NSWorkspace.shared.notificationCenter` |
| `hidutil` через `exec.Command` | `Process` |
| `embed.FS` словари | bundle resources (`Bundle.module`/`Bundle.main`) |
| `atomic.Int32` | `OSAllocatedUnfairLock`-обёртка / Swift `Atomic` |
| `goroutine` | `DispatchQueue.global().async` / `Task.detached` |
| Snowball через cgo-зависимость | Swift-порт Snowball **или** предвычисленные стемы |
| YAML | JSON/plist + `Codable` |

**Единственное «cgo-подобное» место в Swift** — захват `self` в C-колбэке event tap
(`Unmanaged.passUnretained(self).toOpaque()` → восстановление `fromOpaque` внутри колбэка).

---

## 21. Что добавить в Swift-версии (новые фичи/настройки)

Под цель пользователя (расширение настройками) и сильные стороны Swift:

- **Окно настроек (SwiftUI):** тоглы `Enabled`/`AutoConvert`, `MinWordLength`, редактор `ExcludedApps`,
  список/удаление исключений (`ExceptionStore.List/Forget`).
- **Кастомный хоткей** вместо жёсткого Caps→F18 (запись комбинации, опция «использовать Caps Lock»).
- **Автозапуск при логине** через `SMAppService` (вместо ручной установки).
- **Подпись + нотаризация** (Xcode): стабильная code signature → грант Accessibility **не слетает при
  пересборке** (текущая боль неподписанного бинаря).
- Возможные фичи: счётчик/статистика конвертаций, белый список приложений, выбор направления по умолчанию,
  hot-reload словарей, уведомление при первой конвертации.

---

## 22. Тестирование (портировать `*_test.go` на XCTest)

Существующее покрытие (~815 строк) обязательно перенести — оно фиксирует крайние случаи:
- `detect_test.go` — таблицы вход→исход для `Detector.Check` (включая хвостовую пунктуацию, fuzzy,
  «настоящие английские слова не трогаем»).
- `exceptions_test.go` — CRUD, персистентность, битый файл, прунинг.
- `rollback_test.go` — машина состояний отката.
- `shifted_test.go` — `shiftedRuPunct`.
- `integration_test.go` — связки.

Дополнительно покрыть **фиксы хоткея** (юнит-тесты на чистую логику + ручной чек-лист на интеграцию):
- `convertByHeuristic` обратима: `f(f(x)) == x` на сопоставленном наборе.
- `selectTokenLeftOfCaret`: токен с пунктуацией (`hf,jnfkj`) выделяется целиком (логику расширения
  можно протестировать на стабе AX-чтения).
- Направление по `containsCyrillic`.

**Ручной чек-лист интеграции (обязателен, AX/выделение нельзя полноценно юнит-тестировать):**
1. набрал слово (без пробела) → Caps → перевод; ещё раз Caps → откат;
2. слово + пробел → Caps;
3. слово с пунктуацией `работало ⇄ hf,jnfkj` целиком в обе стороны;
4. выделение мышью / Cmd+A / Shift+стрелки → Caps → переводит ровно выделенное;
5. клик в старое слово → Caps → переводит именно его (не мусор);
6. проверка в нативных (TextEdit/Notes — AX) и не-AX (браузер/Electron — фолбэк через буфер обмена) приложениях.

---

## 23. Известные компромиссы (сохранить осознанно)

- **Не-AX приложения, копирующие строку на пустой `Cmd+C` (VS Code):** ручной хоткей без выделения
  может конвертнуть всю строку. Нативные приложения не затронуты (там работает AX).
- **Search-apps (`+5` backspaces в авто-режиме):** может удалить лишнее, если автоподсказки не было.
- **`Option+Shift+Right` откат при дорастягивании токена** — поведение слегка зависит от приложения,
  но из-за конверсии-как-замены-выделения результат корректен (читаем и конвертируем **фактически
  выделенное**, а не запомненное).
- **Direction heuristic по наличию кириллицы** — для смешанных строк может быть неоднозначна; приемлемо,
  повторный Caps откатывает.
- Caps Lock как хоткей реализован через HID-ремап (`hidutil`), который **сбрасывается при ре-энумерации
  устройства** (сон/пробуждение, переподключение клавиатуры) — рассмотреть переустановку ремапа по
  пробуждению (`NSWorkspace.didWakeNotification`).

---

## 24. Критические инварианты (если что-то одно — то это)

1. **Источник истины ручного хоткея — документ через AX/выделение, НЕ буфер набора.**
2. **Определить наличие выделения (`axSelectionLength`) ДО любого `Option+Shift+Left`.**
3. **AX только читает; запись — вставкой (`Cmd+V`) поверх гарантированного выделения.**
4. **`replacing==1` ⇒ синтетические события не входят в обработчик.**
5. **`handleCapsLock` — single-flight (CAS `capsHandling`).**
6. **Восстановить Caps-ремап на ВСЕХ путях выхода.**
7. **Конверсия обратима (символы без сопоставления проходят насквозь).**
8. **Дорастягивать выделение токена через пунктуацию до пробела.**
