package strata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/janit/viiwork/v2/internal/engine"
)

var (
	_ engine.SessionSharer = (*Engine)(nil)
	_ engine.SessionHolder = (*Engine)(nil)
)

// KVShareOptions is the `kv_share:` block. Strata writes a conversation to a
// session file (STRSESS) and parks one in its conversation cache; the node
// carries the files between the members of the group (internal/sessionshare).
// Members must serve the same model files with the same Strata
// configuration: Strata checks both fingerprints and refuses a file from
// anything else.
type KVShareOptions = engine.KVShareOptions

const (
	// A session file holds the K/V and the running state: about 20 KiB per
	// token of context at most, plus the state and its checkpoint.
	sessionBytesPerToken = 20 << 10
	sessionBytesFixed    = 300 << 20
)

// validateKVShare is the shared rules plus Strata's own.
func validateKVShare(o *KVShareOptions, path string, s engine.Spec) error {
	if o == nil {
		return nil
	}
	if err := o.Validate(path); err != nil {
		return err
	}
	if len(s.GPUs) > 1 {
		return fmt.Errorf("%s needs one GPU per backend: Strata does not save the session of a layer split (gpus_per_backend %d)", path, len(s.GPUs))
	}
	return nil
}

// checkKVShare refuses a file that cannot serve kv_share: no slot_save_path
// to save and import in, or a conversation cache, which an import is parked
// in, turned off. Since Strata 0.2 the cache is on by default (sized from free
// RAM), so only an explicit 0 is refused.
func (c fileConfig) checkKVShare() error {
	if _, err := c.saveDir(); err != nil {
		return err
	}
	for i, a := range c.Args {
		if a == "--conversation-cache-mib" && i+1 < len(c.Args) {
			if mib, err := strconv.Atoi(c.Args[i+1]); err != nil || mib <= 0 {
				return errors.New(`kv_share needs a conversation cache to park a session in: "--conversation-cache-mib" in "args" turns it off`)
			}
		}
	}
	return nil
}

// saveDir is "slot_save_path" as an absolute path: relative to "cwd" as
// upstream reads it. A relative one without an absolute "cwd" is relative to
// the server's working directory, which the node cannot know.
func (c fileConfig) saveDir() (string, error) {
	var p, cwd string
	if c.SlotSavePath == nil || json.Unmarshal(c.SlotSavePath, &p) != nil || strings.TrimSpace(p) == "" {
		return "", errors.New(`kv_share needs "slot_save_path" (a directory the node can read and write) to save and import sessions in`)
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	if c.Cwd != nil && json.Unmarshal(c.Cwd, &cwd) == nil && filepath.IsAbs(cwd) {
		return filepath.Join(cwd, p), nil
	}
	return "", fmt.Errorf(`"slot_save_path" %q is relative and "cwd" is not an absolute path: make it absolute for kv_share`, p)
}

// SessionShare is engine.SessionSharer.
func (e *Engine) SessionShare(s engine.Spec) (engine.SessionShareConfig, bool) {
	opts, err := options("model", s)
	if err != nil || opts.KVShare == nil {
		return engine.SessionShareConfig{}, false
	}
	conf, err := readConfig(s.Path)
	if err != nil {
		return engine.SessionShareConfig{}, false
	}
	dir, err := conf.saveDir()
	if err != nil {
		return engine.SessionShareConfig{}, false
	}
	cfg := opts.KVShare.Config(dir, int64(s.Context)*sessionBytesPerToken+sessionBytesFixed)
	return cfg, true
}

// SaveSession is engine.SessionSharer: POST /slots/session?action=save
// (janit/Strata kv-export), which saves the conversation the client session
// last continued from an idle batch slot or the live session. 404 (never
// seen) and 409 (its slot was taken) are ErrSessionGone.
//
// With a base, kv-export's incremental save may answer with a delta (STRSDLT)
// against it, and says so; an unknown base gives a whole file as before.
func (e *Engine) SaveSession(ctx context.Context, addr, session, name, base string) (engine.SavedSession, error) {
	body := map[string]any{"session": session, "filename": name}
	if base != "" {
		body["base"] = base
	}
	var out struct {
		SaveID *string `json:"save_id"`
		Delta  bool    `json:"delta"`
	}
	if err := e.slotAction(ctx, addr, "/slots/session?action=save", body, &out); err != nil {
		return engine.SavedSession{}, err
	}
	saved := engine.SavedSession{Delta: out.Delta}
	if out.SaveID != nil {
		saved.ID = *out.SaveID
	}
	if saved.Delta && saved.ID == "" {
		return engine.SavedSession{}, errors.New("strata: a delta save without a save_id")
	}
	return saved, nil
}

// ImportSession is engine.SessionSharer: POST /slots/0?action=import, which
// validates the file like a restore and parks it in the conversation cache.
func (e *Engine) ImportSession(ctx context.Context, addr, name string) error {
	return e.slotAction(ctx, addr, "/slots/0?action=import", map[string]any{"filename": name}, nil)
}

// SessionHeld is engine.SessionHolder: POST /slots/session?action=held
// (janit/Strata kv-export since 9d44ecd2), how much of a conversation the
// engine still holds live, in a batch slot or parked. With n it names the
// conversation by its ids ("n", "hash"; since v0.2.6-kvx2), which a
// v0.2.6-kvx1 build ignores and answers for the session's last branch. An
// older build answers 400, which is "cannot say".
func (e *Engine) SessionHeld(ctx context.Context, addr, session string, n int64, hash uint64) (int64, error) {
	var out struct {
		NHeld *int64 `json:"n_held"`
	}
	body := map[string]any{"session": session}
	if n > 0 {
		body["n"], body["hash"] = n, fmt.Sprintf("%016x", hash)
	}
	if err := e.slotAction(ctx, addr, "/slots/session?action=held", body, &out); err != nil {
		return 0, err
	}
	if out.NHeld == nil || *out.NHeld < 0 {
		return 0, errors.New("strata: /slots/session?action=held: no n_held in the answer")
	}
	return *out.NHeld, nil
}

// slotAction posts body and decodes a 200's answer into out (nil = discard).
func (e *Engine) slotAction(ctx context.Context, addr, path string, body map[string]any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+path, bytes.NewReader(b))
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
		if out != nil {
			if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(out); err != nil {
				return fmt.Errorf("strata: %s: reading the answer: %w", path, err)
			}
			return nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil
	}
	var msg struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&msg)
	if _, isSave := body["session"]; isSave && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusConflict) {
		return fmt.Errorf("%w: %s", engine.ErrSessionGone, msg.Error.Message)
	}
	return fmt.Errorf("strata: %s: %d %s", path, resp.StatusCode, msg.Error.Message)
}
