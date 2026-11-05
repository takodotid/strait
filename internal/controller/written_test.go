package controller

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
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

// List lists as the API server holds the objects, less the frozen ones, which it lists as they were. A field
// selector of one of the controller's indexes matches each object as the list returns it, as an informer's index does.
func (l *laggingCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	o := &client.ListOptions{}
	o.ApplyOptions(opts)
	selector := o.FieldSelector
	o.FieldSelector = nil
	if err := l.Client.List(ctx, list, o); err != nil {
		return err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return err
	}
	var kept []runtime.Object
	for _, item := range items {
		obj := item.(client.Object)
		l.copyFrozen(client.ObjectKeyFromObject(obj), obj)
		if selector == nil || indexed(obj, selector) {
			kept = append(kept, obj)
		}
	}
	return meta.SetList(list, kept)
}

// indexed reports whether obj matches every requirement of a selector of the controller's indexes.
func indexed(obj client.Object, selector fields.Selector) bool {
	index := map[string]client.IndexerFunc{IndexExportService: exportService, IndexGatewayPort: gatewayPortIndex}
	for _, req := range selector.Requirements() {
		f, ok := index[req.Field]
		if !ok || !slices.Contains(f(obj), req.Value) {
			return false
		}
	}
	return true
}
