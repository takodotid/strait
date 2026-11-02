//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestE2E builds a two-site fixture, runs the scenarios in order on it, and stops at the first failure.
func TestE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), totalTimeout)
	defer cancel()
	e := newEnv(t)

	// Cleanups run last in, first out: heal, then dump on failure, then delete the clusters.
	t.Cleanup(func() { e.teardown(t) })
	t.Cleanup(func() {
		if t.Failed() {
			e.dump(t)
		}
	})
	t.Cleanup(func() {
		hctx, hcancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer hcancel()
		e.heal(hctx)
	})

	steps := []struct {
		name string
		run  func(context.Context, *testing.T)
	}{
		{"setup", e.setup},
		{"1-baseline", e.baseline},
		{"import", e.importService},
		{"2-link-failover", e.linkFailover},
		{"3-switchover", e.switchover},
		{"4-partition", e.partition},
		{"5-disagreeing-declarations", e.declarations},
		{"6-site-loss", e.siteLoss},
		{"7-two-writable", e.twoWritable},
		{"rbac", e.noForbidden},
	}
	for _, s := range steps {
		start := time.Now()
		ok := t.Run(s.name, func(t *testing.T) { s.run(ctx, t) })
		e.times.record(t, s.name, time.Since(start))
		if !ok {
			t.FailNow()
		}
	}
}

// teardown deletes both clusters and the temporary directory, unless STRAIT_E2E_KEEP=1.
func (e *env) teardown(t *testing.T) {
	if e.keep {
		for _, s := range e.sites() {
			fmt.Fprintf(os.Stderr, "e2e: kept site %s: kubectl --kubeconfig %s --context %s\n", s.name, s.kubeconfig, s.context)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err := parallel(
		func() error { return e.deleteCluster(ctx, e.a) },
		func() error { return e.deleteCluster(ctx, e.b) },
	)
	if err != nil {
		t.Errorf("teardown: %v", err)
		return
	}
	_ = os.RemoveAll(e.dir)
}
