package services

import (
	"strings"
	"testing"
)

func TestIVSChannelName(t *testing.T) {
	tests := map[string]string{
		"Liberty Hill Cookout": "Liberty-Hill-Cookout",
		" Gospel: Live! ":      "Gospel-Live",
		"🎤":                    "event",
	}

	for input, want := range tests {
		if got := ivsChannelName(input); got != want {
			t.Errorf("ivsChannelName(%q) = %q, want %q", input, got, want)
		}
	}

	longName := ivsChannelName(strings.Repeat("a", 200))
	if len(longName) != 128 {
		t.Errorf("long channel name has length %d, want 128", len(longName))
	}
}
