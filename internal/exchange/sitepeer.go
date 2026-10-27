package exchange

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/takodotid/strait/api/v1alpha1"
)

// Condition type and reasons of a SitePeer.
const (
	ConditionReachable = "Reachable"
	ReasonFresh        = "Fresh"
	ReasonStale        = "Stale"
	ReasonNoDocument   = "NoDocument"
	ReasonSiteMismatch = "SiteMismatch"
)

// statusTick is the time between two writes of the SitePeer statuses.
const statusTick = time.Second

// StatusWriter writes every SitePeer's status from the poller's state. Start runs it.
//
// +kubebuilder:rbac:groups=strait.tako.id,resources=sitepeers,verbs=get;list;watch
// +kubebuilder:rbac:groups=strait.tako.id,resources=sitepeers/status,verbs=get;update;patch
type StatusWriter struct {
	Client client.Client
	Poller *Poller
}

// Start writes the statuses every second until ctx is cancelled.
func (w *StatusWriter) Start(ctx context.Context) error {
	ticker := time.NewTicker(statusTick)
	defer ticker.Stop()
	for {
		if err := w.sync(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.FromContext(ctx).Error(err, "cannot write SitePeer status")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// sync writes the status of every SitePeer whose status changed.
func (w *StatusWriter) sync(ctx context.Context, now time.Time) error {
	var list v1alpha1.SitePeerList
	if err := w.Client.List(ctx, &list); err != nil {
		return err
	}
	timeout := w.Poller.Exchange().PeerTimeout.Duration
	var errs []error
	for i := range list.Items {
		sp := &list.Items[i]
		doc, state := w.Poller.Snapshot(sp.Name)
		status := peerStatus(sp.Status, sp.Generation, doc, state, now, timeout)
		if equality.Semantic.DeepEqual(sp.Status, status) {
			continue
		}
		orig := sp.DeepCopy()
		sp.Status = status
		if err := w.Client.Status().Patch(ctx, sp, client.MergeFrom(orig)); err != nil {
			errs = append(errs, fmt.Errorf("SitePeer %s: %w", sp.Name, err))
		}
	}
	return errors.Join(errs...)
}

// peerStatus returns a SitePeer's status from its poll state, keeping the current conditions' transition times.
func peerStatus(current v1alpha1.SitePeerStatus, generation int64, doc Document, state PeerState, now time.Time, timeout time.Duration) v1alpha1.SitePeerStatus {
	out := v1alpha1.SitePeerStatus{
		Reachable:  state.Fresh(now, timeout),
		LastError:  state.LastError,
		Services:   int32(len(doc.Services)),
		Exports:    int32(len(doc.Exports)),
		Conditions: slices.Clone(current.Conditions),
	}
	if !state.LastSuccess.IsZero() {
		t := metav1.NewTime(state.LastSuccess).Rfc3339Copy()
		out.LastSeenTime = &t
	}
	c := metav1.Condition{
		Type:               ConditionReachable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.NewTime(now).Rfc3339Copy(),
	}
	switch {
	case out.Reachable:
		c.Status = metav1.ConditionTrue
		c.Reason = ReasonFresh
		c.Message = "the peer's last document is fresh"
	case state.Mismatch:
		c.Reason = ReasonSiteMismatch
		c.Message = "the peer answered with another site's document"
	case state.LastSuccess.IsZero():
		c.Reason = ReasonNoDocument
		c.Message = "no document received from the peer"
	default:
		c.Reason = ReasonStale
		c.Message = fmt.Sprintf("no document received from the peer for more than %s", timeout)
	}
	meta.SetStatusCondition(&out.Conditions, c)
	return out
}
