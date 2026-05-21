package worker

import (
	"context"
	"log/slog"
	"time"

	"fenced/internal/obs"
	"fenced/internal/provider"
	"fenced/internal/store"
)

// Reconciler makes the provider's list and the ledger agree. Either side can be
// wrong: a create that timed out leaves a sandbox with no ref, and a provider
// incident leaves a ref with no sandbox.
type Reconciler struct {
	Store    *store.Store
	Provider provider.Provider
	Log      *slog.Logger
	Grace    time.Duration // how young a sandbox may be and still be left alone
	Keep     int           // snapshots kept per repository
}

func (c *Reconciler) Loop(ctx context.Context, every time.Duration) {
	for ctx.Err() == nil {
		if err := c.Once(ctx); err != nil {
			c.Log.Error("reconcile", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(every):
		}
	}
}

func (c *Reconciler) Once(ctx context.Context) error {
	// The provider is read first. A sandbox created in between is then missing
	// from that list, and the age check below keeps us from calling it lost.
	list, err := c.Provider.List(ctx)
	if err != nil {
		return err // never act on half a picture
	}
	open, err := c.Store.Open(ctx)
	if err != nil {
		return err
	}
	obs.Open.Set(float64(len(list)))
	have := map[string]bool{}
	for _, i := range list {
		have[i.Name] = true
		row, known := open[i.Name]
		if known && row.Wanted || i.Age < c.Grace {
			continue
		}
		if err := c.Provider.Destroy(ctx, i.Name, i.Ref); err != nil {
			c.Log.Warn("destroy leaked sandbox", "name", i.Name, "err", err)
			continue
		}
		obs.Leaked.Inc()
		c.Log.Info("removed leaked sandbox", "name", i.Name, "known", known)
		if known {
			_ = c.Store.Destroyed(ctx, i.Name)
			_ = c.Store.Event(ctx, row.RunID, row.Epoch, "leak_removed", map[string]any{"sandbox": i.Name})
		}
	}
	for name, row := range open {
		if have[name] || row.Age < c.Grace || row.Provider != c.Provider.Name() {
			continue
		}
		if row.Wanted && row.State == "live" {
			obs.Lost.Inc()
			c.Log.Warn("sandbox is gone", "name", name, "run", row.RunID)
			_ = c.Store.Event(ctx, row.RunID, row.Epoch, "sandbox_lost", map[string]any{"sandbox": name})
		}
		_ = c.Store.Destroyed(ctx, name)
	}
	if s, ok := c.Provider.(provider.Snapshotter); ok {
		old, err := c.Store.OldSnapshots(ctx, c.Provider.Name(), c.Keep)
		if err != nil {
			return err
		}
		for key, ref := range old {
			// The row goes first: a missing image fails a create, a missing row only
			// costs one rebuild.
			if err := c.Store.DropSnapshot(ctx, key); err != nil {
				return err
			}
			if err := s.DropSnapshot(ctx, ref); err != nil {
				c.Log.Warn("drop snapshot", "ref", ref, "err", err)
			}
		}
	}
	obs.Reconciled.SetToCurrentTime()
	return nil
}
