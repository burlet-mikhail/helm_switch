package main

import (
	"sync"
	"unicode"
)

// Buffer collects keystrokes and emits words at boundaries
type Buffer struct {
	mu      sync.Mutex
	chars   []rune
	onWord  func(word string)
}

func NewBuffer(onWord func(string)) *Buffer {
	return &Buffer{
		chars:  make([]rune, 0, 64),
		onWord: onWord,
	}
}

func (b *Buffer) Add(r rune) {
	// Trace special chars only in verbose mode.
	if !('a' <= r && r <= 'z') && !('A' <= r && r <= 'Z') && !('а' <= r && r <= 'я') && !('А' <= r && r <= 'Я') && r != ' ' {
		vlog("Buffer.Add special char: %q (U+%04X)", string(r), r)
	}

	b.mu.Lock()
	var emit string
	if isWordBoundary(r) {
		if universalPunct[r] && len(b.chars) > 0 {
			b.chars = append(b.chars, r)
			emit = string(b.chars)
			b.chars = b.chars[:0]
		} else if len(b.chars) > 0 {
			emit = string(b.chars)
			b.chars = b.chars[:0]
		}
	} else {
		b.chars = append(b.chars, r)
	}
	b.mu.Unlock()

	// Call onWord synchronously AFTER releasing the mutex to avoid deadlock
	// (callback may call buf.Clear() which needs the same mutex).
	// Synchronous call also prevents race conditions on shared Detector state.
	if emit != "" && b.onWord != nil {
		b.onWord(emit)
	}
}

func (b *Buffer) Backspace() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.chars) > 0 {
		b.chars = b.chars[:len(b.chars)-1]
	}
}

func (b *Buffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.chars = b.chars[:0]
}

// FlushWord returns the current buffered word and clears the buffer.
func (b *Buffer) FlushWord() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.chars) == 0 {
		return ""
	}
	word := string(b.chars)
	b.chars = b.chars[:0]
	return word
}

// LastWord returns the current buffered word WITHOUT mutating or flushing
// the buffer. Used by the convert-last-word hotkey so that repeated
// presses keep observing the same word until a real keystroke arrives.
// Returns "" when the buffer is empty.
func (b *Buffer) LastWord() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.chars) == 0 {
		return ""
	}
	// string(b.chars) copies the underlying runes, so the returned value
	// is safe to use after we release b.mu and even if b.chars mutates.
	return string(b.chars)
}

func isWordBoundary(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return false
	}
	// QWERTY punctuation that maps to Russian letters — NOT boundaries
	if qwertyRuPunct[r] {
		return false
	}
	// Shifted number keys that map to Russian punctuation — NOT boundaries
	if _, ok := shiftedRuPunct[r]; ok {
		return false
	}
	return true
}
