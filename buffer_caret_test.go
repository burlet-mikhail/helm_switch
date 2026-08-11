package main

import "testing"

// The hotkey rewrites text by backspacing over what LastWord reports, so these
// tests pin down when the buffer is allowed to claim it knows what sits at the
// caret. Getting this wrong does not merely skip a conversion — it deletes
// characters somewhere else.

// TestClearForgetsFlushedWord covers the caret having moved away: Clear runs on
// clicks, arrow keys and Cmd shortcuts, after which the last flushed word is no
// longer at the cursor and must not be offered for conversion.
func TestClearForgetsFlushedWord(t *testing.T) {
	buf := NewBuffer(nil)
	for _, r := range "привет " { // the space flushes the word
		buf.Add(r)
	}
	if got := buf.LastWord(); got != "привет" {
		t.Fatalf("LastWord() before Clear = %q, want %q", got, "привет")
	}

	buf.Clear() // user clicked elsewhere

	if got := buf.LastWord(); got != "" {
		t.Errorf("LastWord() after Clear = %q, want %q — the caret has moved", got, "")
	}
}

// TestSetWordMakesConversionToggle reproduces the "press Caps twice and nothing
// happens" report: after a conversion the buffer must describe the text that is
// now on screen, so the next press converts it back instead of retyping the
// same result.
func TestSetWordMakesConversionToggle(t *testing.T) {
	tests := []struct {
		name    string
		typed   string
		flushed bool
	}{
		{"mid-word", "ghbdtn", false},
		{"after a space", "ghbdtn", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := NewBuffer(nil)
			buf.SetWord(tt.typed, tt.flushed)

			if got := buf.LastWord(); got != tt.typed {
				t.Fatalf("LastWord() = %q, want %q", got, tt.typed)
			}
			if got := buf.IsBufferEmpty(); got != tt.flushed {
				t.Fatalf("IsBufferEmpty() = %v, want %v", got, tt.flushed)
			}

			// What the conversion writes back on the next press.
			converted := convertByHeuristic(tt.typed)
			buf.SetWord(converted, tt.flushed)

			if got := buf.LastWord(); got != converted {
				t.Errorf("LastWord() after conversion = %q, want %q", got, converted)
			}
			if got := convertByHeuristic(buf.LastWord()); got != tt.typed {
				t.Errorf("second press converts to %q, want the original %q", got, tt.typed)
			}
		})
	}
}

// TestSetWordDropsStaleFlushedWord guards a subtle one: a mid-word SetWord must
// drop the previously flushed word, or LastWord would fall back to it as soon
// as the user backspaces the current word away.
func TestSetWordDropsStaleFlushedWord(t *testing.T) {
	buf := NewBuffer(nil)
	for _, r := range "первое " {
		buf.Add(r)
	}
	buf.SetWord("второе", false)

	for range "второе" {
		buf.Backspace()
	}
	if got := buf.LastWord(); got != "" {
		t.Errorf("LastWord() = %q, want %q — %q was flushed before the rewrite", got, "", "первое")
	}
}
