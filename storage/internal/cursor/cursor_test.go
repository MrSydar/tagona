package cursor

import (
	"testing"
	"time"
)

func TestKeyCursorRoundTripsToTheMicrosecond(t *testing.T) {
	at := time.Date(2026, 10, 8, 9, 0, 0, 123456000, time.UTC)
	id := "00000000-0000-4000-8000-000000000001"
	gotAt, gotID, err := DecodeKey(EncodeKey(at, id))
	if err != nil || !gotAt.Equal(at) || gotID != id {
		t.Fatalf("%v %v %v", gotAt, gotID, err)
	}
}

func TestKeyCursorRefusesOtherCursors(t *testing.T) {
	// an object cursor is not a collection cursor
	if _, _, err := DecodeKey(Encode(time.Now(), "00000000-0000-4000-8000-000000000001")); err == nil {
		t.Error("an object cursor was accepted")
	}
	for _, bad := range []string{"", "!!!", "bm9uc2Vuc2U", "azEyfA"} { // "", garbage, "nonsense", "k12|"
		if _, _, err := DecodeKey(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
