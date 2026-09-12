package accept

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func r1Report() Report {
	return Report{
		Command: "join",
		Target:  "node-a",
		Started: time.Date(2026, 9, 11, 22, 0, 0, 0, time.UTC),
		Checks: []Check{
			{Name: "a", Pass: true, Detail: "ok", Elapsed: 2340 * time.Millisecond},
			{Name: "b", Pass: false},
		},
	}
}

func TestReportText(t *testing.T) { // R1
	r := r1Report()
	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	want := "PASS  a  2.3s  ok\nFAIL  b\njoin node-a: 1/2 passed\n"
	if buf.String() != want {
		t.Errorf("text:\n%q\nwant:\n%q", buf.String(), want)
	}
	if r.Pass() {
		t.Error("Pass() with a failing check")
	}
	r.Checks[1].Pass = true
	if !r.Pass() {
		t.Error("Pass() false with every check passing")
	}
}

func TestReportWithoutChecksFails(t *testing.T) { // R2
	if (Report{Command: "join", Target: "node-a"}).Pass() {
		t.Error("a report with no checks passes")
	}
}

func TestReportJSONRoundTrip(t *testing.T) { // R3
	r := r1Report()
	var buf bytes.Buffer
	if err := r.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "\n  \"command\": \"join\"") {
		t.Errorf("not indented by two spaces:\n%s", buf.String())
	}
	var back Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if !back.Started.Equal(r.Started) {
		t.Errorf("started %v, want %v", back.Started, r.Started)
	}
	back.Started = r.Started
	if !reflect.DeepEqual(back, r) {
		t.Errorf("round trip:\n%+v\nwant:\n%+v", back, r)
	}
}
