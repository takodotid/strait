// Package v1 holds the strait.tako.id/v1 API.
// +kubebuilder:object:generate=true
// +groupName=strait.tako.id
package v1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version of every resource in this package.
	GroupVersion = schema.GroupVersion{Group: "strait.tako.id", Version: "v1"}

	// DeprecatedGroupVersion is served too, with the same schema, so objects written by v0.1 releases keep working.
	DeprecatedGroupVersion = schema.GroupVersion{Group: GroupVersion.Group, Version: "v1alpha1"}

	// SchemeBuilder registers the types of this package.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types of this package to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
