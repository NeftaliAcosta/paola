package agent

import (
	"testing"
	"unicode/utf8"
)

func TestNormalizeUTF8(t *testing.T) {
	got := normalizeUTF8(string([]byte("before\xffafter")))
	if !utf8.ValidString(got) {
		t.Fatal("normalized value is not valid UTF-8")
	}
	if got != "before�after" {
		t.Fatalf("normalized value = %q", got)
	}
}
