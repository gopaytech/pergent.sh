package engine

import "context"

// RecoverLegacyPanels handles a review panel a main build left in flight. It
// logs one warning when anything of it is in the database, queues each
// waiting panel run again as a single review, deletes the Jobs of the live
// members (best effort) and wakes the dispatcher. It runs once at start,
// before the usage fill. A failure is logged: the server still starts.
func (e *Engine) RecoverLegacyPanels(ctx context.Context) {
	log := e.log()
	chains, members, err := e.Store.LegacyPanel(ctx)
	if err != nil {
		log.Error("read the legacy review panel data failed", "err", err)
		return
	}
	runs, jobs, err := e.Store.RequeueLegacyPanels(ctx)
	if err != nil {
		log.Error("requeue the legacy review panels failed", "err", err)
		return
	}
	if chains == 0 && members == 0 && runs == 0 {
		return
	}
	log.Warn("review panel data from a main build is not used", "chains", chains, "members", members, "runs_requeued", runs)
	for _, name := range jobs {
		if e.Launcher == nil {
			break
		}
		if err := e.Launcher.Delete(ctx, name); err != nil {
			log.Warn("delete the Job of a legacy panel member failed", "job", name, "err", err)
		}
	}
	e.Wake()
}
