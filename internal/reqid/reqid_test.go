package reqid

import (
	"testing"
	"time"
)

func TestEncodeKnownValues(t *testing.T) {
	var zero [16]byte
	if got := encode(zero); got != "00000000000000000000000000" {
		t.Fatalf("zero: %s", got)
	}
	var max [16]byte
	for i := range max {
		max[i] = 0xff
	}
	if got := encode(max); got != "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Fatalf("max: %s", got)
	}
}

func TestOrdering(t *testing.T) {
	a := newAt(time.UnixMilli(1_000))
	b := newAt(time.UnixMilli(2_000))
	if len(a) != 26 || !(a[:10] < b[:10]) {
		t.Fatalf("ids not time-ordered: %s %s", a, b)
	}
}
