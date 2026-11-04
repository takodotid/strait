package controller

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// recording is a manager that keeps the runnables added to it.
type recording struct {
	manager.Manager
	added []manager.Runnable
}

func (m *recording) Add(r manager.Runnable) error {
	m.added = append(m.added, r)
	return m.Manager.Add(r)
}

// workers returns the MaxConcurrentReconciles of the one controller added to m.
func workers(t *testing.T, m *recording) int {
	t.Helper()
	if len(m.added) != 1 {
		t.Fatalf("%d runnables added, want 1 controller", len(m.added))
	}
	v := reflect.ValueOf(m.added[0])
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	f := v.FieldByName("MaxConcurrentReconciles")
	if !f.IsValid() {
		t.Fatalf("%T has no MaxConcurrentReconciles", m.added[0])
	}
	return int(f.Int())
}

// Exports and Imports reconcile one at a time, even where the manager allows more: the gateway port allocation reads
// the ports in use from the cache, and two concurrent reconciles could take one port (RFC0005, requirement 30).
func TestOneWorkerForExportsAndImports(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := straitv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	newManager := func() *recording {
		mgr, err := ctrl.NewManager(&rest.Config{Host: "https://127.0.0.1:1"}, ctrl.Options{
			Scheme:  scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
			Controller: config.Controller{
				MaxConcurrentReconciles: 4,
				GroupKindConcurrency:    map[string]int{"Export.strait.tako.id": 4, "Import.strait.tako.id": 4},
				SkipNameValidation:      ptr.To(true),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return &recording{Manager: mgr}
	}

	exports := newManager()
	if err := (&ExportReconciler{Client: exports.GetClient()}).SetupWithManager(exports); err != nil {
		t.Fatal(err)
	}
	if n := workers(t, exports); n != 1 {
		t.Errorf("Export controller: %d workers, want 1", n)
	}
	imports := newManager()
	if err := (&ImportReconciler{Client: imports.GetClient()}).SetupWithManager(imports, make(chan event.GenericEvent)); err != nil {
		t.Fatal(err)
	}
	if n := workers(t, imports); n != 1 {
		t.Errorf("Import controller: %d workers, want 1", n)
	}
}
