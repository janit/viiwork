package alias

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

type sent struct {
	key string
	msg []byte
}

type recorder struct {
	mu         sync.Mutex
	broadcasts []sent
	events     []string
}

func (r *recorder) broadcast(key string, msg []byte) {
	r.mu.Lock()
	r.broadcasts = append(r.broadcasts, sent{key, append([]byte(nil), msg...)})
	r.mu.Unlock()
}

func (r *recorder) Emit(typ string, gpuID int, format string, args ...any) {
	r.mu.Lock()
	r.events = append(r.events, fmt.Sprintf("%s|%d|", typ, gpuID)+fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *recorder) counts() (broadcasts, events int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.broadcasts), len(r.events)
}

func (r *recorder) lastEvent() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return ""
	}
	return r.events[len(r.events)-1]
}

func (r *recorder) lastBroadcast() sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.broadcasts) == 0 {
		return sent{}
	}
	return r.broadcasts[len(r.broadcasts)-1]
}

func (r *recorder) eventsContaining(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if strings.Contains(e, sub) {
			n++
		}
	}
	return n
}

type serviceFx struct {
	*resolverFx
	rec *recorder
	svc *Service
}

func newServiceFx(t *testing.T) *serviceFx {
	t.Helper()
	fx := &serviceFx{resolverFx: newResolverFx(t), rec: &recorder{}}
	fx.svc = NewService(fx.store, fx.resolver, fx.rec.broadcast, fx.rec, fx.logs.logf)
	return fx
}

func broadcastMsg(t *testing.T, name string, e meshapi.AliasEntry) []byte {
	t.Helper()
	raw, err := json.Marshal(meshapi.AliasBroadcast{Name: name, Entry: e})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func coderReq() meshapi.AliasWriteRequest {
	return meshapi.AliasWriteRequest{Target: "Qwen3.8-27B", Fallbacks: []string{"gemma-4-31B-it"}}
}

func TestServiceWrites(t *testing.T) {
	fx := newServiceFx(t)
	b, err := fx.svc.Set("stable-coder", coderReq())
	if err != nil || b.Entry.Ver != 1 || b.Name != "stable-coder" {
		t.Fatalf("G1: %+v, %v", b, err)
	}
	last := fx.rec.lastBroadcast()
	var decoded struct {
		Entry map[string]any `json:"entry"`
	}
	if err := json.Unmarshal(last.msg, &decoded); err != nil || last.key != "stable-coder" {
		t.Fatalf("G1: broadcast %q under %q: %v", last.msg, last.key, err)
	}
	if _, has := decoded.Entry["history"]; has || decoded.Entry["target"] != "Qwen3.8-27B" {
		t.Errorf("G1: the broadcast carries history: %s", last.msg)
	}
	if ev := fx.rec.lastEvent(); ev != "alias|-1|alias stable-coder: (none) -> Qwen3.8-27B by gb1" {
		t.Errorf("G1: event = %q", ev)
	}

	nb, ne := fx.rec.counts()
	var we *WriteError
	if _, err := fx.svc.Set("other", meshapi.AliasWriteRequest{Target: "nosuch"}); !errors.As(err, &we) || we.Status != 409 {
		t.Errorf("G2: err = %v", err)
	}
	if b2, e2 := fx.rec.counts(); b2 != nb || e2 != ne {
		t.Error("G2: a refused write broadcast or emitted")
	}
	if _, ok := fx.store.Get("other"); ok {
		t.Error("G2: a refused write was stored")
	}
	if _, err := fx.svc.Delete("nosuch"); !errors.As(err, &we) || we.Status != 404 {
		t.Errorf("G3: err = %v", err)
	}
	if _, err := fx.svc.Revert("stable-coder"); !errors.As(err, &we) || we.Status != 409 {
		t.Errorf("G4: err = %v", err)
	}

	b, err = fx.svc.Delete("stable-coder")
	if err != nil || !b.Entry.Deleted || !strings.HasSuffix(fx.rec.lastEvent(), "alias stable-coder: Qwen3.8-27B -> (deleted) by gb1") {
		t.Errorf("G5: %+v, %v, event %q", b, err, fx.rec.lastEvent())
	}
	var tomb meshapi.AliasBroadcast
	if err := json.Unmarshal(fx.rec.lastBroadcast().msg, &tomb); err != nil || !tomb.Entry.Deleted {
		t.Errorf("G5: broadcast = %s", fx.rec.lastBroadcast().msg)
	}
}

func TestServiceNotifyMsg(t *testing.T) {
	fx := newServiceFx(t)
	if _, err := fx.svc.Set("stable-coder", coderReq()); err != nil {
		t.Fatal(err)
	}
	newer := E(2, startMS+5000, "node-a", "gemma-4-31B-it")
	msg := broadcastMsg(t, "stable-coder", newer)
	nb, _ := fx.rec.counts()
	fx.svc.NotifyMsg(msg)
	if got, _ := fx.store.Get("stable-coder"); got.By != "node-a" || got.Ver != 2 {
		t.Errorf("G6: store = %+v", got)
	}
	if ev := fx.rec.lastEvent(); !strings.HasSuffix(ev, "alias stable-coder: Qwen3.8-27B -> gemma-4-31B-it by node-a") {
		t.Errorf("G6: event = %q", ev)
	}
	if b, _ := fx.rec.counts(); b != nb+1 || fx.rec.lastBroadcast().key != "stable-coder" {
		t.Errorf("G6: broadcasts %d -> %d", nb, b)
	}

	nb, ne := fx.rec.counts()
	fx.svc.NotifyMsg(msg)
	if b, e := fx.rec.counts(); b != nb || e != ne {
		t.Errorf("G7: repeat produced broadcasts %d -> %d, events %d -> %d", nb, b, ne, e)
	}

	fx.svc.NotifyMsg([]byte("not json"))
	fx.svc.NotifyMsg([]byte("not json"))
	if n := fx.logs.count("not valid JSON"); n != 1 {
		t.Errorf("G8: log lines = %d", n)
	}

	tie := E(2, startMS+9000, "gb2", "granite")
	fx.svc.NotifyMsg(broadcastMsg(t, "stable-coder", tie))
	fx.svc.NotifyMsg(broadcastMsg(t, "stable-coder", tie))
	if n := fx.rec.eventsContaining("alias conflict stable-coder:"); n != 1 {
		t.Errorf("G9: conflict events = %d", n)
	}
	if ev := fx.rec.eventsContaining("by gb2 at"); ev != 1 || fx.rec.eventsContaining("by node-a at") != 1 {
		t.Errorf("G9: the conflict event does not name both writers: %v", fx.rec.events)
	}
}

func TestServicePushPull(t *testing.T) {
	a := newServiceFx(t)
	if _, err := a.svc.Set("stable-coder", coderReq()); err != nil {
		t.Fatal(err)
	}
	a.clock.advance(time.Second)
	if _, err := a.svc.Set("stable-coder", meshapi.AliasWriteRequest{Target: "gemma-4-31B-it"}); err != nil {
		t.Fatal(err)
	}
	b := newServiceFx(t)
	b.svc.MergeRemoteState(a.svc.LocalState(false), true)
	if !reflect.DeepEqual(a.store.Table(), b.store.Table()) {
		t.Errorf("G10: %+v != %+v", b.store.Table(), a.store.Table())
	}

	tomb := E(3, startMS+10000, "node-a", "")
	tomb.Deleted = true
	raw, _ := json.Marshal(meshapi.AliasTable{V: 1, Aliases: map[string]meshapi.AliasEntry{"stable-coder": tomb}})
	b.svc.MergeRemoteState(raw, false)
	if len(b.resolver.Info()) != 0 || !strings.HasSuffix(b.rec.lastEvent(), "-> (deleted) by node-a") {
		t.Errorf("G11: info=%+v event=%q", b.resolver.Info(), b.rec.lastEvent())
	}
}

func TestServiceLargeMergedAlias(t *testing.T) {
	fx := newServiceFx(t)
	big := E(1, startMS, "node-a", "Qwen3.8-27B")
	for i := 0; i < 20; i++ {
		big.Fallbacks = append(big.Fallbacks, fmt.Sprintf("%02d%s", i, strings.Repeat("y", 58)))
	}
	raw, _ := json.Marshal(meshapi.AliasTable{V: 1, Aliases: map[string]meshapi.AliasEntry{"big": big}})
	fx.svc.MergeRemoteState(raw, false)
	fx.svc.MergeRemoteState(raw, false)
	if b, _ := fx.rec.counts(); b != 0 {
		t.Errorf("G12: %d broadcasts", b)
	}
	if n := fx.logs.count("push/pull will carry it"); n != 1 {
		t.Errorf("G12: log lines = %d", n)
	}
	if _, ok := fx.store.Get("big"); !ok {
		t.Error("G12: the store does not hold it")
	}
}

func TestServiceConcurrent(t *testing.T) {
	fx := newServiceFx(t)
	var mu sync.Mutex
	var seen []meshapi.AliasEntry
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if b, err := fx.svc.Set("stable-coder", coderReq()); err == nil {
				mu.Lock()
				seen = append(seen, b.Entry)
				mu.Unlock()
			}
		}()
		go func(i int) {
			defer wg.Done()
			e := E(uint64(i%5+1), startMS+int64(i), "node-a", "gemma-4-31B-it")
			mu.Lock()
			seen = append(seen, e)
			mu.Unlock()
			fx.svc.NotifyMsg(broadcastMsg(t, "stable-coder", e))
		}(i)
	}
	wg.Wait()
	final, _ := fx.store.Get("stable-coder")
	final.History = nil
	for _, e := range seen {
		if meshapi.CompareAliasEntries(e, final) > 0 {
			t.Errorf("G13: %+v beats the final entry %+v", e, final)
		}
	}
}

func TestServiceRunPurges(t *testing.T) {
	fx := newServiceFx(t)
	fx.svc.purgeEvery = 20 * time.Millisecond
	tomb := E(2, startMS-31*24*time.Hour.Milliseconds(), "node-a", "")
	tomb.Deleted = true
	if _, _, err := fx.store.Merge("old", tomb); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fx.svc.Run(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	if _, ok := fx.store.Get("old"); ok {
		t.Error("G14: the old tombstone was not purged")
	}
}
