package main

import (
	"github.com/gopaytech/pergent.sh/internal/engine"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
	"github.com/gopaytech/pergent.sh/internal/webhook"
)

// commandBots tells the webhooks, from the connected platforms, which
// mention addresses an instance's review bot and whose comments are never
// commands: the review bot's and the approve bot's. An instance that isn't
// connected has no bots, so its comments are ignored.
func commandBots(platforms *syncmap.Map[engine.Platform]) webhook.Bots {
	return func(instance string) (string, []string, bool) {
		p, ok := platforms.Get(instance)
		if !ok || p.Forge == nil {
			return "", nil, false
		}
		ignore := []string{p.Forge.Bot().Username}
		if p.Approver != nil {
			ignore = append(ignore, p.Approver.Bot().Username)
		}
		return p.Forge.Mention(), ignore, true
	}
}
