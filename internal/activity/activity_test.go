package activity

import (
	"fmt"
	"testing"
)

func TestEventHistoryCap(t *testing.T) {
	l := NewLogWithHistory(10, 5)
	for i := 1; i <= 8; i++ {
		l.Emit("system", -1, "event %d", i)
	}
	got := l.Recent()
	if len(got) != 5 || got[0].Message != "event 4" || got[4].Message != "event 8" {
		t.Errorf("events = %+v", got)
	}

	d := NewLogWithHistory(10, 0)
	for i := 0; i < DefaultEventHistory+10; i++ {
		d.Emit("system", -1, "e%d", i)
	}
	if n := len(d.Recent()); n != DefaultEventHistory {
		t.Errorf("default history kept %d events, want %d", n, DefaultEventHistory)
	}
	if got := d.Recent()[0].Message; got != fmt.Sprintf("e%d", 10) {
		t.Errorf("oldest kept event = %q", got)
	}
}
