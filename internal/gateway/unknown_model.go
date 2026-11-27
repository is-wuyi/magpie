package gateway

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
)

// unknownModel is what a request for a model magpie doesn't know is told
// (MOMO on Discord: "Zen-Space-Bunny" got a 404 that said neither how a
// model is named nor which one was meant): the ids nearest to it, the
// ways a model can be named, then every id there is. what is "" for a
// chat, or what the model was for (" to draw with"). A group magpie found
// that is gone with its model is said so, with what to do (GoneAutoGroup).
// Cloud errors name only providers and groups the caller may see.
func unknownModel(who access.Identity, asked, what string) string {
	visible := visibleModelIDs(who)
	nearest := func() []string {
		all := provider.Closest(asked, len(provider.Catalog()))
		out := all[:0]
		for _, id := range all {
			if visible[id] {
				out = append(out, id)
			}
			if len(out) == 3 {
				break
			}
		}
		return out
	}
	if provider.GoneAutoGroup(asked) {
		// a conversation saved on a group magpie found whose model no
		// provider serves now (canlee886 on X): what to do, not every id
		msg := fmt.Sprintf("magpie no longer serves %q%s: it was the routing group magpie found for a model, and no provider in magpie serves that model now", asked, what)
		if near := nearest(); len(near) > 0 {
			msg += " (nearest now: " + strings.Join(near, ", ") + ")"
		}
		return msg + "; pick another model for this conversation in the agent (in Codex, the model menu under the message box), or add back a provider that serves it"
	}
	msg := fmt.Sprintf("magpie knows no model %q%s", asked, what)
	if near := nearest(); len(near) > 0 {
		msg += "; did you mean " + strings.Join(near, ", ") + "?"
	}
	msg += " Name a model as provider/model (as /v1/models lists it), a routing group's id or name, or a bare model id" +
		" (one served by several providers goes to the group magpie found for it, else to the first of them in the Providers order)"
	ids := make([]string, 0, len(visible))
	for id := range visible {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if len(ids) > 0 {
		msg += "; it has " + strings.Join(ids, ", ")
	} else {
		msg += "; add a provider in magpie first"
	}
	return msg
}

func visibleModelIDs(who access.Identity) map[string]bool {
	out := map[string]bool{}
	for _, e := range provider.Catalog() {
		if e.Group != "" {
			if g, ms, ok := provider.FindGroup(e.ID); ok && groupAllowed(who, g, ms) {
				out[e.ID] = true
			}
		} else if e.Provider != nil && providerAllowed(who, *e.Provider) {
			out[e.ID] = true
		}
	}
	return out
}
