package engine

import (
	"context"

	"github.com/gopaytech/pergent.sh/internal/instances"
)

// finishRemovals completes the removal of the instances of the dashboard that
// an operator is removing (Removing): once every project of one is gone from
// the store (reconcile took each off its platform, since the dashboard
// switched its repositories off), it deletes the instance and its secrets,
// forgets it in the registry and tells OnInstanceRemoved. An instance with a
// project left waits for the next pass. It runs at the end of each
// reconcile; errors are logged and the next pass tries again.
func (e *Engine) finishRemovals(ctx context.Context) {
	var removing []instances.Instance
	for _, inst := range e.registry().All() {
		if inst.Source == instances.SourceDashboard && inst.Removing {
			removing = append(removing, inst)
		}
	}
	if len(removing) == 0 || ctx.Err() != nil {
		return
	}
	projects, err := e.Store.Projects(ctx)
	if err != nil {
		e.log().Error("couldn't list the projects to finish removing instances", "error", e.redact(err.Error()))
		return
	}
	left := map[string]int{}
	for _, p := range projects {
		left[p.Instance]++
	}
	for _, inst := range removing {
		log := e.log().With("instance", inst.Name)
		if n := left[inst.Name]; n > 0 {
			log.Info("an instance being removed still has projects to take off its platform", "projects", n)
			continue
		}
		// A sign-in that raced the removal may have made an account since
		// the dashboard deleted them.
		if err := e.Store.DeleteInstanceData(ctx, inst.Name); err != nil {
			log.Error("couldn't finish removing an instance", "error", e.redact(err.Error()))
			continue
		}
		if err := e.Store.DeleteInstance(ctx, inst.Name); err != nil {
			log.Error("couldn't finish removing an instance", "error", e.redact(err.Error()))
			continue
		}
		e.registry().Remove(inst.Name)
		if e.OnInstanceRemoved != nil {
			e.OnInstanceRemoved(inst.Name)
		}
		log.Info("instance removed")
	}
}
