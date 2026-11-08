package main

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mainplane-ai/mainplane/pkg/catalog"
	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// errNoModel is a new session with nothing to start from: no session to copy
// and no catalog model connected. The harness cannot list a provider's models,
// so a model outside the catalog is known only once a session has used it.
// The catalog groups the terminal app reads: the models it offers, and the
// ones a new session takes when the newest session's is not connected.
const (
	group    = "self-hosted-all"
	defaults = "self-hosted-default"
)

var errNoModel = errors.New("no model to start from: make one session with `mainplane new`; new threads then copy the newest session's model")

// choices is what a session can run on: the catalog's presets whose provider
// the harness serves, in the catalog's order, then the models of recent
// sessions that are not in the catalog and whose provider it serves, newest
// first. A model outside the catalog is any "<provider>/<model>" of a provider
// the harness serves; the catalog only adds names and preset configs.
type choices struct {
	cat       catalog.Catalog
	providers []string // the providers the harness serves
	ids       []string
}

func newChoices(cat catalog.Catalog, providers []string, recent []statefile.Conf) choices {
	c := choices{cat: cat, providers: providers}
	for _, id := range cat.Groups[group] {
		if c.serves(cat.Presets[id].Provider + "/") {
			c.ids = append(c.ids, id)
		}
	}
	for _, r := range recent {
		if _, ok := c.preset(r.Model); !ok && c.serves(r.Model) && !slices.Contains(c.ids, r.Model) {
			c.ids = append(c.ids, r.Model)
		}
	}
	return c
}

// serves reports whether the harness serves model's provider, the part of
// "<provider>/<model>" before the slash.
func (c choices) serves(model string) bool {
	p, _, ok := strings.Cut(model, "/")
	return ok && slices.Contains(c.providers, p)
}

// preset is the catalog preset id names, when the harness serves its provider.
func (c choices) preset(id string) (catalog.Preset, bool) {
	p, ok := c.cat.Presets[id]
	return p, ok && c.serves(p.Provider+"/")
}

// startConf is the config of a new session, picked as the terminal app picks
// it, but for any connected model: base's model (the session to copy, or
// nil), else the catalog's default, else the first catalog model connected. A
// catalog model takes its preset's config; another is base as it was. The
// workers are left for the caller.
func (c choices) startConf(base *statefile.Conf) (statefile.Conf, error) {
	if base != nil && c.serves(base.Model) {
		n := *base
		for _, id := range []string{n.Preset, n.Model} {
			if p, ok := c.preset(id); ok {
				return presetConf(id, p), nil
			}
		}
		n.Workers = nil
		return n, nil
	}
	for _, id := range c.cat.Groups[defaults] {
		if p, ok := c.preset(id); ok {
			return presetConf(id, p), nil
		}
	}
	for _, id := range c.ids {
		if p, ok := c.preset(id); ok {
			return presetConf(id, p), nil
		}
	}
	return statefile.Conf{}, errNoModel
}

// startBase is the config a new thread's session copies: the newest of recent
// (newest first) that a thread shows, so that sessions made elsewhere, such
// as test runs on another model, do not move the chats; else, when no thread
// shows a recent one, the newest. Nil when there are no sessions.
func startBase(recent []sessionInfo, shown []string) *statefile.Conf {
	for _, s := range recent {
		if slices.Contains(shown, s.ID) {
			return &s.Config
		}
	}
	if len(recent) > 0 {
		return &recent[0].Config
	}
	return nil
}

// switchConf is the config /model s continues cur in, with cur's workers: a
// connected catalog model s names (its id, model or name) with its preset's
// config; else a recent model s names by id or model part, or any
// "<provider>/<model>" of a connected provider, with cur's tools and context
// limit. Params and input types stay behind: they belong to cur's provider.
func (c choices) switchConf(cur statefile.Conf, s string) (statefile.Conf, error) {
	var conf statefile.Conf
	if id := pickModel(c.cat, c.ids, s); id != "" {
		conf = presetConf(id, c.cat.Presets[id])
	} else if m := c.model(s); m != "" {
		conf = statefile.Conf{Model: m, Tools: cur.Tools, ContextLimit: cur.ContextLimit}
	} else {
		return conf, fmt.Errorf("no connected model %q: !model lists them", s)
	}
	conf.Workers = cur.Workers
	return conf, nil
}

// model is the model outside the catalog that s names: a recent one by id or
// model part, else s itself when it is "<provider>/<model>" of a connected
// provider.
func (c choices) model(s string) string {
	for _, id := range c.ids {
		if _, ok := c.cat.Presets[id]; ok {
			continue
		}
		if _, m, _ := strings.Cut(id, "/"); strings.EqualFold(s, id) || strings.EqualFold(s, m) {
			return id
		}
	}
	if _, m, _ := strings.Cut(s, "/"); m != "" && c.serves(s) {
		return s
	}
	return ""
}

// list is the models a picker offers, cur's first, so that a picker that shows
// only some shows it.
func (c choices) list(cur statefile.Conf) []model {
	now := cmp.Or(cur.Preset, cur.Model)
	ids := c.ids
	if i := slices.Index(ids, now); i > 0 {
		ids = slices.Concat(ids[i:i+1], ids[:i], ids[i+1:])
	}
	var ms []model
	for _, id := range ids {
		ms = append(ms, model{ID: id, Name: cmp.Or(c.cat.Presets[id].Name, id), Current: id == now})
	}
	return ms
}

// pickModel is the id among ids that s names: the id, the model or its name.
func pickModel(cat catalog.Catalog, ids []string, s string) string {
	for _, id := range ids {
		p, ok := cat.Presets[id]
		if ok && (strings.EqualFold(s, id) || strings.EqualFold(s, p.Model) || strings.EqualFold(s, p.Name)) {
			return id
		}
	}
	return ""
}

func presetConf(id string, p catalog.Preset) statefile.Conf {
	return statefile.Conf{Model: p.Provider + "/" + p.Model, Tools: p.Tools, ContextLimit: p.ContextLimit, Input: p.Input, Params: p.Params, Preset: id}
}
