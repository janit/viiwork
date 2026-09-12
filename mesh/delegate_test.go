package mesh

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/janit/viiwork/v2/meshapi"
)

type recordingPayload struct {
	mu     sync.Mutex
	msgs   [][]byte
	merged [][]byte
	joins  []bool
	state  []byte
}

func (p *recordingPayload) NotifyMsg(msg []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, msg)
}

func (p *recordingPayload) LocalState(bool) []byte { return p.state }

func (p *recordingPayload) MergeRemoteState(buf []byte, join bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.merged = append(p.merged, append([]byte(nil), buf...))
	p.joins = append(p.joins, join)
}

type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *logRecorder) count(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

type delegateFixture struct {
	d     *delegate
	log   *logRecorder
	clock *fakeClock
	table *memberTable
}

func newDelegateFixture(t *testing.T, o Options, startedAgo time.Duration) *delegateFixture {
	t.Helper()
	clock := newClock()
	tbl := newMemberTable(selfMember(), clock.Now)
	rec := &logRecorder{}
	queue := &memberlist.TransmitLimitedQueue{NumNodes: func() int { return 3 }, RetransmitMult: 3}
	meta := mustMeta(t, meshapi.RoleNode)
	if o.Name == "" {
		o = baseOptions()
	}
	d := newDelegate(o, meta, tbl, queue, rec.logf, clock.Now().Add(-startedAgo), clock.Now)
	return &delegateFixture{d: d, log: rec, clock: clock, table: tbl}
}

func TestDelegateMetadata(t *testing.T) {
	f := newDelegateFixture(t, Options{}, time.Hour)
	if !bytes.Equal(f.d.NodeMeta(512), mustMeta(t, meshapi.RoleNode)) {
		t.Error("NodeMeta must return the encoded metadata")
	}
}

func TestDelegateMessagesAndState(t *testing.T) {
	p := &recordingPayload{state: []byte("mine")}
	o := baseOptions()
	o.Payload = p
	f := newDelegateFixture(t, o, time.Hour)

	msg := frame(framePayload, []byte("abc"))
	f.d.NotifyMsg(msg)
	msg[1] = 'X'
	if len(p.msgs) != 1 || string(p.msgs[0]) != "abc" {
		t.Errorf("payload got %q, want abc unaffected by later mutation", p.msgs)
	}

	f.d.NotifyMsg([]byte{0x07, 'x'})
	f.d.NotifyMsg([]byte{0x07, 'x'})
	if n := f.log.count("ignoring"); n != 1 {
		t.Errorf("unknown kind logged %d times, want 1", n)
	}

	if string(f.d.LocalState(true)) != "mine" {
		t.Error("LocalState must reach the payload")
	}
	f.d.MergeRemoteState([]byte("theirs"), true)
	if len(p.merged) != 1 || string(p.merged[0]) != "theirs" || !p.joins[0] {
		t.Errorf("merged=%q joins=%v", p.merged, p.joins)
	}

	bare := newDelegateFixture(t, Options{}, time.Hour)
	if bare.d.LocalState(false) != nil {
		t.Error("without a payload LocalState is nil")
	}
	bare.d.MergeRemoteState([]byte("x"), false)
	bare.d.NotifyMsg(frame(framePayload, []byte("x")))
}

func TestDelegateBroadcasts(t *testing.T) {
	f := newDelegateFixture(t, Options{}, time.Hour)
	f.d.broadcast("stable-coder", []byte("v1"))
	f.d.broadcast("stable-coder", []byte("v2"))
	got := f.d.GetBroadcasts(0, 1400)
	if len(got) != 1 || !bytes.Equal(got[0], []byte{0x02, 'v', '2'}) {
		t.Errorf("GetBroadcasts = %q, want one 0x02 v2", got)
	}
}

func TestDelegateAliveFilter(t *testing.T) {
	open := newDelegateFixture(t, Options{}, time.Hour)
	self := &memberlist.Node{Name: "gb1", Addr: net.ParseIP("100.64.0.1"), Meta: []byte("garbage")}
	if err := open.d.NotifyAlive(self); err != nil {
		t.Errorf("own name: %v", err)
	}
	v1 := &memberlist.Node{Name: "old", Addr: net.ParseIP("100.64.0.2"), Meta: []byte(`{"v":1,"api":8086,"ver":"1.8.1","role":"node"}`)}
	if err := open.d.NotifyAlive(v1); err == nil || !strings.Contains(err.Error(), "not viiwork v2") {
		t.Errorf("v1 metadata: %v", err)
	}
	if err := open.d.NotifyAlive(node(t, "b", "100.64.0.2")); err != nil {
		t.Errorf("valid member: %v", err)
	}
	rogue := node(t, "r", "192.0.2.10")
	if err := open.d.NotifyAlive(rogue); err == nil || !strings.Contains(err.Error(), "not on the tailnet range") {
		t.Errorf("out of range in an open mesh: %v", err)
	}
	_ = open.d.NotifyAlive(rogue)
	if n := open.log.count("ignoring member r"); n != 1 {
		t.Errorf("rejection logged %d times, want 1", n)
	}

	so := baseOptions()
	so.SecretKey = key1
	secured := newDelegateFixture(t, so, time.Hour)
	if err := secured.d.NotifyAlive(node(t, "r", "192.0.2.10")); err != nil {
		t.Errorf("a secured mesh does no address check: %v", err)
	}
}

func TestDelegateConflicts(t *testing.T) {
	o := baseOptions()
	o.ConflictWindow = 60 * time.Second
	young := newDelegateFixture(t, o, 5*time.Second)
	existing := node(t, "gb1", "100.64.0.1")
	other := node(t, "gb1", "100.64.0.9")
	young.d.NotifyConflict(existing, other)
	select {
	case err := <-young.d.fatal():
		if !strings.Contains(err.Error(), `duplicate node name "gb1"`) || !strings.Contains(err.Error(), "100.64.0.9") {
			t.Errorf("fatal = %v", err)
		}
	default:
		t.Fatal("a young node must report a duplicate name as fatal")
	}
	done := make(chan struct{})
	go func() {
		young.d.NotifyConflict(existing, other)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a second conflict blocked")
	}
	select {
	case <-young.d.fatal():
		t.Error("the fatal error must be sent only once")
	default:
	}

	old := newDelegateFixture(t, o, 61*time.Second)
	old.d.NotifyConflict(existing, other)
	select {
	case <-old.d.fatal():
		t.Error("an older node must keep its name")
	default:
	}
	if old.log.count("keeping it") != 1 {
		t.Error("an older node logs that it keeps the name")
	}

	old.d.NotifyConflict(node(t, "node-a", "100.64.0.3"), node(t, "node-a", "100.64.0.4"))
	if old.log.count("claimed by both") != 1 {
		t.Error("another node's conflict is logged")
	}
}

func TestDelegateEvents(t *testing.T) {
	f := newDelegateFixture(t, Options{}, time.Hour)
	next := func() MemberEvent {
		select {
		case ev := <-f.d.events():
			return ev
		case <-time.After(time.Second):
			t.Fatal("no event")
			return MemberEvent{}
		}
	}

	f.d.NotifyJoin(node(t, "b", "100.64.0.2"))
	if ev := next(); ev.Kind != EventJoin || ev.Member.Name != "b" {
		t.Errorf("join event = %+v", ev)
	}
	f.d.NotifyMsg(frame(frameLeave, []byte("b")))
	f.d.NotifyLeave(node(t, "b", "100.64.0.2"))
	if ev := next(); ev.Kind != EventLeave || ev.Member.State != meshapi.MemberLeft {
		t.Errorf("leave event = %+v", ev)
	}

	f.d.NotifyJoin(node(t, "c", "100.64.0.4"))
	next()
	f.d.NotifyLeave(node(t, "c", "100.64.0.4"))
	if ev := next(); ev.Kind != EventFail || ev.Member.State != meshapi.MemberDead {
		t.Errorf("fail event = %+v", ev)
	}

	f.d.NotifyJoin(node(t, "d", "100.64.0.5"))
	next()
	f.d.NotifyMsg(frame(frameLeave, []byte("d")))
	f.clock.Advance(31 * time.Second)
	f.d.NotifyLeave(node(t, "d", "100.64.0.5"))
	if ev := next(); ev.Member.State != meshapi.MemberDead {
		t.Errorf("a 31 s old leave notice must read as dead, got %+v", ev)
	}
}

func TestDelegateEventsNeverBlock(t *testing.T) {
	f := newDelegateFixture(t, Options{}, time.Hour)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1100; i++ {
			f.d.NotifyJoin(node(t, fmt.Sprintf("n%d", i), "100.64.0.2"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("NotifyJoin blocked with nobody reading events")
	}
	if n := f.log.count("dropped"); n != 1 {
		t.Errorf("dropped logged %d times, want 1", n)
	}
}
