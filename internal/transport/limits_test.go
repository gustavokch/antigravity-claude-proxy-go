package transport

import (
	"io"
	"strings"
	"testing"
)

func TestReadCapped(t *testing.T) {
	t.Run("under the cap is whole and unflagged", func(t *testing.T) {
		data, truncated, err := ReadCapped(strings.NewReader("hello"), 16)
		if err != nil || truncated || string(data) != "hello" {
			t.Fatalf("data=%q truncated=%v err=%v", data, truncated, err)
		}
	})
	t.Run("exactly the cap is not truncated", func(t *testing.T) {
		data, truncated, err := ReadCapped(strings.NewReader(strings.Repeat("a", 16)), 16)
		if err != nil || truncated || len(data) != 16 {
			t.Fatalf("len=%d truncated=%v err=%v", len(data), truncated, err)
		}
	})
	t.Run("over the cap is flagged and cut at the cap", func(t *testing.T) {
		data, truncated, err := ReadCapped(strings.NewReader(strings.Repeat("a", 24)), 16)
		if err != nil {
			t.Fatal(err)
		}
		if !truncated {
			t.Error("a body past the cap must report truncation")
		}
		if len(data) != 16 {
			t.Errorf("len(data) = %d, want 16", len(data))
		}
	})
	t.Run("a read error is returned with what it got", func(t *testing.T) {
		r := io.MultiReader(strings.NewReader("abc"), errReader{})
		data, _, err := ReadCapped(r, 16)
		if err == nil || string(data) != "abc" {
			t.Fatalf("data=%q err=%v", data, err)
		}
	})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
