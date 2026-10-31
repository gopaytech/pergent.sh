package engine

import (
	"context"
	"fmt"

	"github.com/gopaytech/pergent.sh/internal/instances"
)

// attInstanceRemovalStuck is the kind of the instance-wide "Needs attention"
// item for an instance being removed that still has projects on its platform
// after a reconcile. It goes with the instance's other rows when the removal
// finishes (Store.DeleteInstanceData).
const attInstanceRemovalStuck = "instance_removal_stuck"

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
			msg := fmt.Sprintf("pergent.sh could not take %d project(s) of the instance %q, which is being removed, off its platform; "+
				"it tries again at the next reconcile", n, inst.Name)
			if err := e.Store.SetAttention(ctx, inst.Name, "", attInstanceRemovalStuck, msg); err != nil {
				log.Warn("couldn't record that an instance's removal is stuck", "error", e.redact(err.Error()))
			}
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
