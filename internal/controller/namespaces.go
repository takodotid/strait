package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NamespacedWrites returns a client that writes Services and EndpointSlices only in namespaces, through c. A write
// elsewhere returns an error and sends no request, so the status of the object that needed it says why (RFC#0007,
// requirement 16). Every other write and every read goes through as it is. With no namespaces, it returns c.
func NamespacedWrites(c client.Client, namespaces []string) client.Client {
	if len(namespaces) == 0 {
		return c
	}
	ns := slices.Clone(namespaces)
	slices.Sort(ns)
	return namespacedWrites{Client: c, namespaces: slices.Compact(ns)}
}

type namespacedWrites struct {
	client.Client
	namespaces []string
}

// refuse returns an error when obj is a Service or an EndpointSlice outside the namespaces.
func (c namespacedWrites) refuse(obj client.Object) error {
	switch obj.(type) {
	case *corev1.Service, *discoveryv1.EndpointSlice:
	default:
		return nil
	}
	if slices.Contains(c.namespaces, obj.GetNamespace()) {
		return nil
	}
	return fmt.Errorf("namespace %q is not one where the controller writes Services and EndpointSlices: %s (chart value controller.namespaces)",
		obj.GetNamespace(), strings.Join(c.namespaces, ", "))
}

func (c namespacedWrites) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := c.refuse(obj); err != nil {
		return err
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c namespacedWrites) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if err := c.refuse(obj); err != nil {
		return err
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c namespacedWrites) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if err := c.refuse(obj); err != nil {
		return err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c namespacedWrites) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if err := c.refuse(obj); err != nil {
		return err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

// DeleteAllOf takes its namespace from its options, not from obj.
func (c namespacedWrites) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	o := &client.DeleteAllOfOptions{}
	o.ApplyOptions(opts)
	probe, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not an object", obj)
	}
	probe.SetNamespace(o.Namespace)
	if err := c.refuse(probe); err != nil {
		return err
	}
	return c.Client.DeleteAllOf(ctx, obj, opts...)
}
