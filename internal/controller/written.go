package controller

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// written keeps, for each object, what the controller's last status write returned, and the resourceVersions that its
// writes replaced, until the cache shows that write. Each write replaces the version just before it, so a cached object
// with one of those versions predates the last write, and any other version is the last write or newer.
// The zero value is ready to use.
type written[T any, P interface {
	*T
	client.Object
}] struct {
	mu   sync.Mutex
	last map[types.NamespacedName]*write[P]
}

type write[P client.Object] struct {
	object   P
	replaced map[string]bool
}

// latest replaces a cached object that predates the controller's last status write with what that write returned.
// The reconcile then compares with, and writes over, the status last written. A cache that lags behind the
// controller's own writes would otherwise make the next write conflict.
func (w *written[T, P]) latest(obj P) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := client.ObjectKeyFromObject(obj)
	l, ok := w.last[key]
	if !ok {
		return
	}
	if l.replaced[obj.GetResourceVersion()] {
		*obj = *l.object.DeepCopyObject().(P)
		return
	}
	delete(w.last, key)
}

// keys returns the keys of the objects whose last status write the cache may not show yet, and whose write matches.
func (w *written[T, P]) keys(match func(P) bool) []types.NamespacedName {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []types.NamespacedName
	for key, l := range w.last {
		if match(l.object) {
			out = append(out, key)
		}
	}
	return out
}

// forget drops what was kept for an object that is gone.
func (w *written[T, P]) forget(key types.NamespacedName) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.last, key)
}

// remember records what a status write returned, and the resourceVersion it replaced.
func (w *written[T, P]) remember(replaced string, obj P) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		w.last = map[types.NamespacedName]*write[P]{}
	}
	key := client.ObjectKeyFromObject(obj)
	l, ok := w.last[key]
	if !ok {
		l = &write[P]{replaced: map[string]bool{}}
		w.last[key] = l
	}
	l.replaced[replaced] = true
	l.object = obj.DeepCopyObject().(P)
}

// updateStatus writes obj's status over the resourceVersion that obj holds, and reports whether it wrote.
// A conflict means that another writer changed the object after it was read. It is logged at info level and not
// returned: the cache shows that change soon, and the next reconcile writes again from it.
func (w *written[T, P]) updateStatus(ctx context.Context, c client.Client, obj P) (bool, error) {
	replaced := obj.GetResourceVersion()
	if err := c.Status().Update(ctx, obj); err != nil {
		if apierrors.IsConflict(err) {
			msg := fmt.Sprintf("status not written: the %s changed since it was read; the next reconcile writes it", reflect.TypeFor[T]().Name())
			log.FromContext(ctx).Info(msg, "error", err.Error())
			return false, nil
		}
		return false, err
	}
	w.remember(replaced, obj)
	return true, nil
}
