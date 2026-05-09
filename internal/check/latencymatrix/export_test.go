package latencymatrix

import (
	"reflect"
	"testing"
)

func TestParseLogLine(t *testing.T) {
	got := ParseLogLine("[2026-05-07 16:15:40.000] Hello, world!")
	want := LogLine{Timestamp: "2026-05-07 16:15:40.000", Message: "Hello, world!"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if ParseLogLine("no brackets").Message != "no brackets" {
		t.Fatal("expected full line as message")
	}
}

func TestParseLogBytes(t *testing.T) {
	raw := "[2026-05-07 16:15:40.000] a\n\n[2026-05-07 16:15:41.000] b\n"
	got := ParseLogBytes([]byte(raw))
	if len(got) != 2 || got[0].Message != "a" || got[1].Message != "b" {
		t.Fatalf("got %+v", got)
	}
}
