package controller

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// laggingCache reads objects the way an informer cache can just after the controller's own write: once an object is
// frozen, Get and List return it as it was then, while writes reach the API server.
type laggingCache struct {
	client.Client
	frozen []client.Object
}

// freeze freezes the object of obj's type and key as the API server holds it now.
func (l *laggingCache) freeze(t *testing.T, obj client.Object) {
	t.Helper()
	if err := l.Client.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	l.frozen = append(l.frozen, obj)
}

// thaw ends every freeze: reads show the API server's objects again.
func (l *laggingCache) thaw() { l.frozen = nil }

// copyFrozen copies into obj the frozen object of its type and key, and reports whether there is one.
func (l *laggingCache) copyFrozen(key client.ObjectKey, obj client.Object) bool {
	for _, f := range l.frozen {
		if reflect.TypeOf(f) == reflect.TypeOf(obj) && client.ObjectKeyFromObject(f) == key {
			reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(f.DeepCopyObject()).Elem())
			return true
		}
	}
	return false
}

func (l *laggingCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if l.copyFrozen(key, obj) {
		return nil
	}
	return l.Client.Get(ctx, key, obj, opts...)
}

func (l *laggingCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := l.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return err
	}
	for _, item := range items {
		o := item.(client.Object)
		l.copyFrozen(client.ObjectKeyFromObject(o), o)
	}
	return nil
}
