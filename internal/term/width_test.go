package term

import (
	"os"
	"testing"
)

func TestWidthOfNonTerminalIsZero(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if w := Width(f.Fd()); w != 0 {
		t.Fatalf("width of a plain file = %d", w)
	}
}
