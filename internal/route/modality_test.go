package route

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// voiceCap is a peer's row for model "m" published with a modality.
func voiceCap(slots, busy int, modality string) meshapi.ModelCapacity {
	m := capM(slots, busy, 1)
	m.Modality = modality
	return m
}

func modalityFixture(performance bool) *fixture {
	f := &fixture{local: newFakeLocal(), reports: &fakeReports{}, clock: &clock{now: t0}}
	f.router = New(Config{Self: "self", Local: f.local, Remote: f.reports, StaleAfter: 3 * time.Second,
		QueueMax: 4, QueueTimeout: 50 * time.Millisecond, Now: f.clock.Now, Performance: performance})
	return f
}

func TestNonTextPeerIsNeverACandidate(t *testing.T) {
	for _, perf := range []bool{false, true} {
		f := modalityFixture(perf)
		// The voice host has every free slot and the best score; the text
		// host has one slot and no score.
		v := voiceCap(8, 0, meshapi.ModalityVoice)
		v.TTFTOverheadMs, v.PrefillMsPer1k = 1, 1
		f.reports.set(report("V", t0, time.Millisecond, v), report("T", t0, 50*time.Millisecond, capM(1, 0, 1)))
		for _, req := range []Request{{}, {Prefer: []string{"V"}}, {SessionKey: 42}} {
			l := f.pick(t, req)
			if got := l.Target().Node; got != "T" {
				t.Errorf("perf=%v %+v: node = %q, want T", perf, req, got)
			}
			l.Release()
		}
		// With the text host full the request waits for it; it is not handed
		// to the voice host.
		f.reports.set(report("V", t0, time.Millisecond, v), report("T", t0, 50*time.Millisecond, capM(1, 1, 1)))
		if _, _, err := f.router.Acquire(context.Background(), Request{Model: "m"}); !errors.Is(err, ErrQueueTimeout) {
			t.Errorf("perf=%v: full text host: err = %v, want ErrQueueTimeout", perf, err)
		}
	}
}

// An unknown modality is not text: a reader that cannot say what a model is
// must not send it a chat completion.
func TestUnknownModalityIsNotText(t *testing.T) {
	f := modalityFixture(false)
	f.reports.set(report("X", t0, time.Millisecond, voiceCap(4, 0, "hologram")))
	var me *ModalityError
	if err := f.pickErr(Request{}); !errors.As(err, &me) || me.Modality != "hologram" {
		t.Fatalf("err = %v, want a ModalityError for hologram", err)
	}
	f.reports.set(report("X", t0, time.Millisecond, voiceCap(4, 0, meshapi.ModalityText)))
	f.pick(t, Request{}).Release()
}

func TestNonTextOnlyModelIsRefused(t *testing.T) {
	f := modalityFixture(false)
	f.reports.set(
		report("sloth-voice", t0, time.Millisecond, voiceCap(2, 0, meshapi.ModalityVoice)),
		report("gb2", t0, time.Millisecond, voiceCap(2, 0, meshapi.ModalityVoice)),
		report("T", t0, time.Millisecond, meshapi.ModelCapacity{Name: "other", Slots: 1, HealthyBackends: 1}),
	)
	check := func(name string, req Request, nodes []string, host string) {
		t.Helper()
		err := f.pickErr(req)
		var me *ModalityError
		if !errors.Is(err, ErrNotText) || !errors.As(err, &me) {
			t.Fatalf("%s: err = %v, want ErrNotText", name, err)
		}
		if me.Model != "m" || me.Modality != meshapi.ModalityVoice || !reflect.DeepEqual(me.Nodes, nodes) || me.Host != host {
			t.Errorf("%s: %+v", name, *me)
		}
	}
	check("plain", Request{}, []string{"gb2", "sloth-voice"}, "")
	check("pinned", Request{Host: "sloth-voice"}, []string{"sloth-voice"}, "sloth-voice")
	check("pinned elsewhere", Request{Host: "T"}, []string{"gb2", "sloth-voice"}, "")
	check("preferred", Request{Prefer: []string{"sloth-voice"}}, []string{"gb2", "sloth-voice"}, "")
	// A refusal, not a wait: Acquire answers at once.
	if _, _, err := f.router.Acquire(context.Background(), Request{Model: "m"}); !errors.Is(err, ErrNotText) {
		t.Errorf("Acquire: err = %v, want ErrNotText", err)
	}
	// A stale report still names the model: existence does not flap.
	f.clock.set(t0.Add(time.Minute))
	check("stale", Request{}, []string{"gb2", "sloth-voice"}, "")
}

// A pin cannot force a text request onto the voice host of a model that is
// also served as text elsewhere.
func TestPinToNonTextHostIsRefused(t *testing.T) {
	f := modalityFixture(false)
	f.local.add("m", newFakeBackend("m/0", 1))
	f.reports.set(report("V", t0, time.Millisecond, voiceCap(4, 0, meshapi.ModalityVoice)), report("T", t0, time.Millisecond, capM(4, 0, 1)))
	var me *ModalityError
	if err := f.pickErr(Request{Host: "V"}); !errors.As(err, &me) || !reflect.DeepEqual(me.Nodes, []string{"V"}) {
		t.Fatalf("pin V: err = %v", err)
	}
	if got := target(t, f.pick(t, Request{Host: "T"})); got != "T" {
		t.Errorf("pin T: %q", got)
	}
	if got := target(t, f.pick(t, Request{Host: "self"})); got != "self" {
		t.Errorf("pin self: %q", got)
	}
	// Unpinned, this node and T serve; V never does.
	for range 6 {
		if got := target(t, f.pick(t, Request{})); got == "V" {
			t.Fatal("routed to the voice host")
		}
	}
}

// A queued request whose only text host leaves, with the model still listed
// by a voice host, is answered with the refusal instead of timing out.
func TestQueuedWaiterGetsModalityRefusal(t *testing.T) {
	f := modalityFixture(false)
	f.router.c.QueueTimeout = 5 * time.Second
	f.reports.set(report("T", t0, time.Millisecond, capM(1, 1, 1)), report("V", t0, time.Millisecond, voiceCap(4, 0, meshapi.ModalityVoice)))
	done := make(chan error, 1)
	go func() {
		_, _, err := f.router.Acquire(context.Background(), Request{Model: "m"})
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for f.router.QueueLen("m") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	f.reports.set(report("V", t0, time.Millisecond, voiceCap(4, 0, meshapi.ModalityVoice)))
	f.router.Wake()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotText) {
			t.Errorf("err = %v, want ErrNotText", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not completed")
	}
}

func TestImageClassIgnoresNonTextHosts(t *testing.T) {
	f := newImgFixture(false)
	v := imgCap(4, 0, imgYesP)
	v.Modality = meshapi.ModalityImage
	f.reports.set(report("V", t0, time.Millisecond, v), report("T", t0, time.Millisecond, imgCap(4, 0, imgNoP)))
	// The image host's yes must not hold a request for a text host that
	// says no: the answer is the text fleet's.
	if err := f.pickErr(Request{Images: true}); !errors.Is(err, ErrNoImageInput) {
		t.Errorf("err = %v, want ErrNoImageInput", err)
	}
}
