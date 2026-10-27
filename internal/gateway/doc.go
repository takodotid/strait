// Package gateway is the gateway agent of RFC#0005. It takes part in the gateway Lease. While it holds the Lease,
// it keeps one WireGuard interface per wireguard SiteLink, publishes the site's public key and reports link health.
//
// The agent needs exactly these RBAC rules, a subset of RFC#0007's gateway agent table.
// The status rules of that table also allow get and update; the agent uses patch only.
//
//	# ClusterRole
//	- apiGroups: ["strait.tako.id"]
//	  resources: ["sites", "sitepeers", "sitelinks"]
//	  verbs: ["get", "list", "watch"]
//	- apiGroups: ["strait.tako.id"]
//	  resources: ["sites/status", "sitelinks/status"]
//	  verbs: ["patch"]
//
//	# Role in the gateway's namespace
//	- apiGroups: [""]
//	  resources: ["secrets"]
//	  resourceNames: ["<Site.spec.wireguard.privateKeySecretRef.name>"]
//	  verbs: ["get"]
//	- apiGroups: ["coordination.k8s.io"]
//	  resources: ["leases"]
//	  verbs: ["create"]
//	- apiGroups: ["coordination.k8s.io"]
//	  resources: ["leases"]
//	  resourceNames: ["strait-gateway"]
//	  verbs: ["get", "update"]
//
// The agent container needs the NET_ADMIN capability, and root in the pod's own network namespace.
package gateway
