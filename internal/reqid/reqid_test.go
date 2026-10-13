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

func TestFloorAndTime(t *testing.T) {
	at := time.UnixMilli(1_760_000_000_123)
	id := newAt(at)
	floor := Floor(at)
	if floor > id || Floor(at.Add(time.Millisecond)) <= id {
		t.Fatalf("floor %s does not bound %s", floor, id)
	}
	got, ok := Time(id)
	if !ok || !got.Equal(at) {
		t.Fatalf("Time(%s) = %v, %v", id, got, ok)
	}
	if _, ok := Time("not-an-id"); ok {
		t.Fatal("invalid id accepted")
	}
}
