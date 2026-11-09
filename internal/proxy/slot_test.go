package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWithSlot(t *testing.T) {
	cases := map[string]struct {
		in   string
		want map[string]any // decoded result, nil when the body must be unchanged
	}{
		"plain":           {`{"model":"m","messages":[]}`, map[string]any{"model": "m", "messages": []any{}, "id_slot": 3.0}},
		"leading space":   {"  \n{ \"model\":\"m\"}", map[string]any{"model": "m", "id_slot": 3.0}},
		"empty object":    {`{}`, map[string]any{"id_slot": 3.0}},
		"empty spaced":    {`{ }`, map[string]any{"id_slot": 3.0}},
		"client's slot":   {`{"model":"m","id_slot":0}`, map[string]any{"model": "m", "id_slot": 3.0}},
		"slot in content": {`{"messages":[{"role":"user","content":"say \"id_slot\""}]}`, map[string]any{"messages": []any{map[string]any{"role": "user", "content": `say "id_slot"`}}, "id_slot": 3.0}},
		"escaped key":     {`{"model":"m","id\u005fslot":0}`, map[string]any{"model": "m", "id_slot": 3.0}},
		"escaped content": {`{"messages":[{"content":"\u00e4\u00e4"}]}`, map[string]any{"messages": []any{map[string]any{"content": "ää"}}, "id_slot": 3.0}},
		"not an object":   {`[1,2]`, nil},
		"not json":        {`hello`, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			out := withSlot([]byte(c.in), "id_slot", 3)
			if c.want == nil {
				if string(out) != c.in {
					t.Fatalf("changed a body it cannot address: %s", out)
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("not JSON: %s: %v", out, err)
			}
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(c.want)
			if string(gb) != string(wb) {
				t.Fatalf("got %s, want %s", gb, wb)
			}
			if strings.Count(string(out), `"id_slot":`) != 1 {
				t.Fatalf("the field appears more than once: %s", out)
			}
		})
	}
}

func TestEscapedKey(t *testing.T) {
	for in, want := range map[string]bool{
		`{"model":"m"}`:                        false,
		`{"id\u005fslot":1}`:                   true,
		`{"a":{"\u0069d_slot" :1}}`:            true,
		`{"content":"\u00e4"}`:                 false,
		`{"content":"\u00e4 \"x\": y"}`:        false,
		`{"a\\u005f":1}`:                       false, // an escaped backslash, then u
		`{"c":"\\","id\u005fslot":2}`:          true,
		`{"c":"\u00e4","k":"v","n\u0061me":1}`: true,
		`{"c":"\u00e4`:                         false,
	} {
		if got := escapedKey([]byte(in)); got != want {
			t.Errorf("%s: %v, want %v", in, got, want)
		}
	}
}

func BenchmarkWithSlot(b *testing.B) {
	body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", 16384) + `"}]}`)
	b.ReportAllocs()
	for b.Loop() {
		withSlot(body, "id_slot", 1)
	}
}

func TestHandlerNamesTheLeasedSlot(t *testing.T) {
	f := newHandlerFx(t)
	eng := newRecEngine(t, nil)
	plain := newRecEngine(t, nil)
	bs := f.local.add("m", eng.addr())
	bs[0].slotField, bs[0].slots = "id_slot", 2
	f.local.add("other", plain.addr())
	f.build()

	if rec := f.do("POST", "/v1/chat/completions", `{"model":"m","id_slot":1,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body, _ := eng.last()
	var got struct {
		IDSlot *int `json:"id_slot"`
	}
	if json.Unmarshal([]byte(body), &got) != nil || got.IDSlot == nil || *got.IDSlot != 0 {
		t.Fatalf("engine got %s, want the leased slot 0 over the client's 1", body)
	}
	bs[0].mu.Lock()
	busy := len(bs[0].slotBusy)
	bs[0].mu.Unlock()
	if busy != 0 {
		t.Fatalf("%d slots still busy after the request", busy)
	}

	if rec := f.do("POST", "/v1/chat/completions", `{"model":"other","messages":[]}`); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if body, _ := plain.last(); strings.Contains(body, "id_slot") {
		t.Fatalf("a backend that does not address slots got one: %s", body)
	}
}
