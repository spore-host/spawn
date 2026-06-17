package cmd

import (
	"bufio"
	"strings"
	"testing"
)

// TestConfirmTypedPhrase is the #217 gate core: only the EXACT phrase (trimmed)
// proceeds; anything else — wrong text, a bare "y", empty, EOF — aborts. This is
// what stands between a fat-fingered keypress and a non-refundable charge.
func TestConfirmTypedPhrase(t *testing.T) {
	const want = "I UNDERSTAND THIS IS NON-REFUNDABLE"
	cases := []struct {
		name  string
		input string
		ok    bool
	}{
		{"exact", want + "\n", true},
		{"exact with surrounding spaces", "  " + want + "  \n", true},
		{"y is not enough", "y\n", false},
		{"yes is not enough", "yes\n", false},
		{"wrong phrase", "I understand\n", false},
		{"case mismatch", "i understand this is non-refundable\n", false},
		{"empty line", "\n", false},
		{"eof / no input", "", false},
		{"partial then newline", "purchase\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(c.input))
			if got := confirmTypedPhrase(r, "prompt: ", want); got != c.ok {
				t.Errorf("confirmTypedPhrase(%q) = %v, want %v", c.input, got, c.ok)
			}
		})
	}
}

// TestConfirmTypedPhrase_PriceGate covers gate 1 (the exact price) and gate 2
// (purchase <offering-id>) — same mechanism, different expected phrases.
func TestConfirmTypedPhrase_PriceAndOffering(t *testing.T) {
	// Gate 1: exact price.
	r := bufio.NewReader(strings.NewReader("12480.00\n"))
	if !confirmTypedPhrase(r, "price: ", "12480.00") {
		t.Error("exact price should confirm")
	}
	r = bufio.NewReader(strings.NewReader("12480\n")) // missing decimals
	if confirmTypedPhrase(r, "price: ", "12480.00") {
		t.Error("inexact price must NOT confirm")
	}
	// Gate 2: purchase <offering-id>.
	r = bufio.NewReader(strings.NewReader("purchase cbo-0abc123\n"))
	if !confirmTypedPhrase(r, "offering: ", "purchase cbo-0abc123") {
		t.Error("exact offering phrase should confirm")
	}
	r = bufio.NewReader(strings.NewReader("purchase cbo-0wrong\n"))
	if confirmTypedPhrase(r, "offering: ", "purchase cbo-0abc123") {
		t.Error("wrong offering id must NOT confirm")
	}
}
