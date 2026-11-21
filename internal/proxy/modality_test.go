package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/catalog"
	"github.com/janit/viiwork/v2/internal/discovery"
	"github.com/janit/viiwork/v2/internal/modelinfo"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

// voiceModel is a viiwork-voice member's capacity row: a model with session
// slots, published with modality voice.
func voiceModel(name string, slots int) meshapi.ModelCapacity {
	m := peerModel(name, slots, 0)
	m.Engine, m.Modality = "haru", meshapi.ModalityVoice
	return m
}

// voiceFx is a node serving "m" itself, with a voice member "sloth-voice"
// serving "haru-4b" and an alias "talk" to it.
func voiceFx(t *testing.T) *handlerFx {
	f := newHandlerFx(t)
	f.local.add("m", newRecEngine(t, nil).addr())
	f.capacity = fakeCapacity{{Name: "m", Engine: "llamacpp", Slots: 1, HealthyBackends: 1, Backends: 1, Ctx: 8192}}
	f.reports.add("sloth-voice", "127.0.0.1:1", time.Now(), voiceModel("haru-4b", 2))
	f.resolve = func(requested string) (string, string, error) {
		if requested == "talk" {
			return "haru-4b", "talk", nil
		}
		return requested, "", nil
	}
	f.extra = func() []meshapi.ModelEntry {
		return []meshapi.ModelEntry{
			{ID: "talk", OwnedBy: meshapi.OwnedByAlias, Target: "haru-4b"},
			{ID: "stable", OwnedBy: meshapi.OwnedByAlias, Target: "m"},
		}
	}
	return f.build()
}

func TestTextRequestToAVoiceModelIs400(t *testing.T) {
	f := voiceFx(t)
	for _, path := range []string{meshapi.PathChatCompletions, meshapi.PathCompletions, meshapi.PathEmbeddings} {
		start := time.Now()
		rec := f.do(http.MethodPost, path, `{"model":"haru-4b","messages":[{"role":"user","content":"hi"}]}`)
		want := `{"error":{"message":"model \"haru-4b\" is voice-only (modality voice): it does not accept ` + path +
			`; it is served by node(s) sloth-voice — open a session there","type":"invalid_request","code":"model_modality"}}`
		if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%s: code=%d body=%s", path, rec.Code, rec.Body.String())
		}
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Errorf("%s: answered after %s: it must not queue", path, d)
		}
	}
}

func TestAliasToAVoiceModelIs400(t *testing.T) {
	f := voiceFx(t)
	rec := f.do(http.MethodPost, "/v1/chat/completions", `{"model":"talk"}`)
	e := errorOf(t, rec)
	if rec.Code != http.StatusBadRequest || e.Code != meshapi.ErrCodeModelModality ||
		e.Message != `alias "talk" targets model "haru-4b" is voice-only (modality voice): it does not accept /v1/chat/completions; it is served by node(s) sloth-voice — open a session there` {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// Neither the pin nor a preference can push a text request onto the voice
// host, even for a name the fleet also serves as text.
func TestPinAndPreferCannotForceAVoiceHost(t *testing.T) {
	f := newHandlerFx(t)
	eng := newRecEngine(t, nil)
	f.local.add("m", eng.addr())
	f.reports.add("sloth-voice", "127.0.0.1:1", time.Now(), voiceModel("m", 8))
	f.build()

	rec := f.do(http.MethodPost, "/v1/chat/completions?host=sloth-voice", chatReq)
	e := errorOf(t, rec)
	if rec.Code != http.StatusBadRequest || e.Code != meshapi.ErrCodeModelModality ||
		e.Message != `model "m" is voice-only (modality voice): it does not accept /v1/chat/completions; host "sloth-voice" serves it as voice — open a session there` {
		t.Errorf("pin: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = f.do(http.MethodPost, "/v1/chat/completions?prefer=sloth-voice", chatReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("prefer: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if body, _ := eng.last(); body != chatReq {
		t.Errorf("prefer: the local engine did not take the request: %q", body)
	}
}

func TestVoiceModelsAreNotListedForChatClients(t *testing.T) {
	f := voiceFx(t)
	rec := f.do(http.MethodGet, "/v1/models", "")
	var resp meshapi.ModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range resp.Data {
		got = append(got, e.ID)
	}
	if strings.Join(got, " ") != "m stable" {
		t.Errorf("/v1/models = %v, want m and the alias to it only", got)
	}
	for _, m := range discovery.Models(f.h) {
		if m.Name == "haru-4b" || m.Name == "talk" {
			t.Errorf("discovery record lists %q", m.Name)
		}
	}
	info, _ := json.Marshal(modelinfo.Build(discovery.Models(f.h)))
	cat, _ := json.Marshal(catalog.Build(catalog.Config{}, discovery.Models(f.h), "http://x"))
	for name, doc := range map[string][]byte{"modelinfo": info, "catalog": cat} {
		if strings.Contains(string(doc), "haru-4b") || strings.Contains(string(doc), `"talk"`) {
			t.Errorf("%s lists the voice model: %s", name, doc)
		}
		if !strings.Contains(string(doc), `"m"`) {
			t.Errorf("%s lost the text model: %s", name, doc)
		}
	}
}

func TestFleetCapacityCarriesModality(t *testing.T) {
	f := voiceFx(t)
	rec := f.do(http.MethodGet, meshapi.PathFleetCapacity, "")
	var resp meshapi.FleetCapacityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// The fixture's StaleAfter is 0, so the member is listed as stale: its
	// modality describes the model, not a reading, and is carried anyway.
	m := model(t, resp, "haru-4b")
	if m.Modality != meshapi.ModalityVoice || m.Engine != "haru" {
		t.Errorf("haru-4b = %+v", m)
	}
	if h := host(t, m, "sloth-voice"); h.Modality != meshapi.ModalityVoice {
		t.Errorf("host = %+v", h)
	}
	if !strings.Contains(rec.Body.String(), `"modality":"voice"`) {
		t.Errorf("body: %s", rec.Body.String())
	}
	if tm := model(t, resp, "m"); tm.Modality != "" {
		t.Errorf("a text model carries modality %q", tm.Modality)
	}
}

// A voice host that shares a text model's name is listed under it with its
// own modality, but its slots and context are none of the text capacity.
func TestFleetTextTotalsExcludeANonTextHost(t *testing.T) {
	v := mc("tg", 16, 0, 0, 512)
	v.Engine, v.Modality = "haru", meshapi.ModalityVoice
	got := aggregateFleet("gb1",
		[]meshapi.ModelCapacity{mc("tg", 4, 1, 0, 8192)},
		[]capacity.Report{rep("sloth-voice", "10.0.0.9:8086", 100*time.Millisecond, v)},
		fleetNow, 3*time.Second)
	m := model(t, got, "tg")
	if m.Modality != "" || m.Slots != 4 || m.Free != 3 || m.Ctx != 8192 || m.Engine != "llamacpp" || len(m.Hosts) != 2 {
		t.Errorf("tg = %+v", m)
	}
	if h := host(t, m, "sloth-voice"); h.Modality != meshapi.ModalityVoice || h.Slots == nil || *h.Slots != 16 {
		t.Errorf("voice host = %+v", h)
	}
	// Two non-text hosts that disagree: the smallest name, deterministically.
	a, b := mc("x", 1, 0, 0, 0), mc("x", 1, 0, 0, 0)
	a.Modality, b.Modality = meshapi.ModalityVoice, meshapi.ModalityImage
	got = aggregateFleet("gb1", nil, []capacity.Report{rep("A", "", 0, a), rep("B", "", 0, b)}, fleetNow, 3*time.Second)
	if x := model(t, got, "x"); x.Modality != meshapi.ModalityImage || x.Slots != 2 {
		t.Errorf("x = %+v", x)
	}
}
