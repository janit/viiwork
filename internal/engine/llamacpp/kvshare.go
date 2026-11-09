package llamacpp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

// kv_share for llama.cpp: a session's conversation is saved from the slot
// that ran it (POST /slots/N?action=save) and restored into a slot on the
// member it spills to (action=restore). llama.cpp works by slot index and
// says in no response which slot served a request, so for a kv_share model
// the node chooses every slot itself (SlotAddressed). Spec:
// docs/superpowers/specs/2026-10-29-llamacpp-kv-share-design.md.

var (
	_ engine.SlotSharer    = (*Engine)(nil)
	_ engine.SlotAddressed = (*Engine)(nil)
)

// KVShareOptions is the llamacpp.kv_share block: the keys every engine
// shares, plus where llama-server writes its slot files.
type KVShareOptions struct {
	engine.KVShareOptions `yaml:",inline"`
	// SaveDir is llama-server's --slot-save-path: an absolute directory the
	// node reads and writes at the same path. A 50k-token conversation of
	// Qwen3.8-27B is a ~4 GB file, so a disk rather than tmpfs on a host
	// short of RAM.
	SaveDir string `yaml:"save_dir"`
}

const (
	// A slot file is the K/V of every cell, the running state of a hybrid
	// model and its checkpoints. Generous: the bound only refuses a put
	// that cannot be a file of this model.
	slotBytesPerToken = 1 << 20
	slotBytesFixed    = 16 << 30
	// slotField is the request body field that names a slot.
	slotField = "id_slot"
)

// stateArgs are the flags that shape what a slot file holds, for the config
// fingerprint. A mismatch the engine reads (cache types, V transposition)
// already fails a restore; these make the receiver refuse the replica from
// its header, before anything of it is stored.
var stateArgs = []string{
	"-ctk", "--cache-type-k", "-ctv", "--cache-type-v", "-fa", "--flash-attn",
	"--swa-full", "-ctxcp", "--ctx-checkpoints", "--checkpoint-min-step",
}

// refusedWithKVShare are operator args that would leave the node's slot
// record wrong or the files where the node cannot find them.
var refusedWithKVShare = map[string]string{
	"--slot-save-path": "the engine sets it from llamacpp.kv_share.save_dir",
	"-np":              "the slot count is models[].parallel, which the node's slot record follows",
	"--parallel":       "the slot count is models[].parallel, which the node's slot record follows",
	"-kvu":             "a unified K/V cache is not supported with kv_share yet",
	"--kv-unified":     "a unified K/V cache is not supported with kv_share yet",
}

func validateKVShare(path string, o *KVShareOptions, s engine.Spec) error {
	if o == nil {
		return nil
	}
	if err := o.KVShareOptions.Validate(path); err != nil {
		return err
	}
	if strings.TrimSpace(o.SaveDir) == "" || !filepath.IsAbs(o.SaveDir) {
		return fmt.Errorf("%s.save_dir must be an absolute directory (got %q): llama-server saves and restores slot files there", path, o.SaveDir)
	}
	for _, a := range s.Args {
		flag, _, _ := strings.Cut(a, "=")
		if why, ok := refusedWithKVShare[flag]; ok {
			return fmt.Errorf("%s: args must not contain %s: %s", path, flag, why)
		}
	}
	return nil
}

// SlotField is engine.SlotAddressed: "id_slot" for a kv_share model.
func (e *Engine) SlotField(s engine.Spec) string {
	if o, err := options(s); err == nil && o.KVShare != nil {
		return slotField
	}
	return ""
}

// SessionShare is engine.SlotSharer.
func (e *Engine) SessionShare(s engine.Spec) (engine.SessionShareConfig, bool) {
	o, err := options(s)
	if err != nil || o.KVShare == nil {
		return engine.SessionShareConfig{}, false
	}
	cfg := o.KVShare.Config(filepath.Clean(o.KVShare.SaveDir), int64(s.Context)*slotBytesPerToken+slotBytesFixed)
	// Zero when the file cannot be read: a zero fingerprint matches nothing,
	// so such a node neither restores nor is restored from.
	cfg.Model, _ = e.modelFingerprint(s.Path)
	cfg.Config = configFingerprint(s.Args)
	return cfg, true
}

// ModelFingerprint is engine.ModelFingerprinter.
func (e *Engine) ModelFingerprint(path string) (uint64, error) {
	return e.modelFingerprint(path)
}

// SaveSlot is engine.SlotSharer.
func (e *Engine) SaveSlot(ctx context.Context, addr string, slot int, name string) error {
	return e.slotAction(ctx, addr, slot, "save", name)
}

// RestoreSlot is engine.SlotSharer.
func (e *Engine) RestoreSlot(ctx context.Context, addr string, slot int, name string) error {
	return e.slotAction(ctx, addr, slot, "restore", name)
}

func (e *Engine) slotAction(ctx context.Context, addr string, slot int, action, name string) error {
	body, _ := json.Marshal(map[string]string{"filename": name})
	url := "http://" + addr + "/slots/" + strconv.Itoa(slot) + "?action=" + action
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil
	}
	var msg struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&msg)
	// 501 on a save: the slot holds what a file cannot (an image's
	// chunks); nothing to send, as with a conversation no longer held.
	if action == "save" && resp.StatusCode == http.StatusNotImplemented {
		return fmt.Errorf("%w: %s", engine.ErrSessionGone, msg.Error.Message)
	}
	return fmt.Errorf("llamacpp: slot %d %s: %d %s", slot, action, resp.StatusCode, msg.Error.Message)
}

// Model fingerprints are read once per file version: the header and samples
// of a multi-gigabyte file are not reread on every reload.
var fingerprints struct {
	sync.Mutex
	m map[string]fingerprint
}

type fingerprint struct {
	size  int64
	mtime time.Time
	sum   uint64
}

const (
	fpHead    = 16 << 20 // the GGUF header and tensor infos, and more
	fpSamples = 8
	fpSample  = 1 << 20
)

// modelFingerprint is FNV-1a 64 over the model's size, its first fpHead
// bytes (the GGUF header: architecture, tokenizer, tensor shapes) and
// fpSamples evenly spread samples of the rest, which tell apart two
// fine-tunes of one architecture. A split GGUF counts in its total size,
// and its first file is what is sampled.
func (e *Engine) modelFingerprint(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	total, err := e.modelSize(path)
	if err != nil {
		total = st.Size()
	}
	fingerprints.Lock()
	if fp, ok := fingerprints.m[path]; ok && fp.size == total && fp.mtime.Equal(st.ModTime()) {
		fingerprints.Unlock()
		return fp.sum, nil
	}
	fingerprints.Unlock()

	h := fnv.New64a()
	_ = binary.Write(h, binary.LittleEndian, total)
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, min(fpHead, st.Size()))); err != nil {
		return 0, err
	}
	if rest := st.Size() - fpHead; rest > fpSample {
		buf := make([]byte, fpSample)
		for i := int64(1); i <= fpSamples; i++ {
			off := fpHead + rest*i/(fpSamples+1)
			n, err := f.ReadAt(buf, off)
			if err != nil && err != io.EOF {
				return 0, err
			}
			h.Write(buf[:n])
		}
	}
	sum := h.Sum64()
	fingerprints.Lock()
	if fingerprints.m == nil {
		fingerprints.m = map[string]fingerprint{}
	}
	fingerprints.m[path] = fingerprint{size: total, mtime: st.ModTime(), sum: sum}
	fingerprints.Unlock()
	return sum, nil
}

// configFingerprint is FNV-1a 64 over the stateArgs in args with their
// values, in a fixed order.
func configFingerprint(args []string) uint64 {
	var parts []string
	for i := 0; i < len(args); i++ {
		flag, val, inline := strings.Cut(args[i], "=")
		if !slices.Contains(stateArgs, flag) {
			continue
		}
		if !inline && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			val = args[i+1]
			i++
		}
		parts = append(parts, canonicalFlag(flag)+"="+val)
	}
	slices.Sort(parts)
	h := fnv.New64a()
	h.Write([]byte("llamacpp-slot-v1\x00"))
	h.Write([]byte(strings.Join(parts, "\x00")))
	return h.Sum64()
}

// canonicalFlag folds a short spelling into its long one, so that -ctk and
// --cache-type-k fingerprint alike.
func canonicalFlag(f string) string {
	switch f {
	case "-ctk":
		return "--cache-type-k"
	case "-ctv":
		return "--cache-type-v"
	case "-fa":
		return "--flash-attn"
	case "-ctxcp":
		return "--ctx-checkpoints"
	}
	return f
}
