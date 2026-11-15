package engine

import (
	"strings"
	"testing"
)

func TestKVSharePeersValidation(t *testing.T) {
	for _, c := range []struct {
		name string
		o    KVShareOptions
		want string // "" = valid
	}{
		{"lan address", KVShareOptions{Group: "g", Peers: map[string]string{"forge-black": "192.168.42.64:8086"}}, ""},
		{"hostname", KVShareOptions{Group: "g", Peers: map[string]string{"b": "forge-black.lan:8086"}}, ""},
		{"no port", KVShareOptions{Group: "g", Peers: map[string]string{"b": "192.168.42.64"}}, "must be host:port"},
		{"bad port", KVShareOptions{Group: "g", Peers: map[string]string{"b": "192.168.42.64:0"}}, "must be host:port"},
		{"no host", KVShareOptions{Group: "g", Peers: map[string]string{"b": ":8086"}}, "must be host:port"},
		{"empty name", KVShareOptions{Group: "g", Peers: map[string]string{"": "10.0.0.1:8086"}}, "not a node name"},
		{"no group", KVShareOptions{Local: true, Peers: map[string]string{"b": "10.0.0.1:8086"}}, "needs a group"},
	} {
		err := c.o.Validate("models[0].strata.kv_share")
		if c.want == "" && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

func TestKVSharePeersReachTheConfig(t *testing.T) {
	o := KVShareOptions{Group: "g", Peers: map[string]string{"b": "10.0.0.2:8086"}}
	cfg := o.Config("/k", 1<<30)
	if cfg.Peers["b"] != "10.0.0.2:8086" {
		t.Fatalf("peers %v", cfg.Peers)
	}
	o.Peers["b"] = "changed"
	if cfg.Peers["b"] != "10.0.0.2:8086" {
		t.Fatal("the config shares the options' map")
	}
}
