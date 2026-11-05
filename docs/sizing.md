# Sizing the controller

The controller's memory grows with the Services and EndpointSlices that its cache holds: by default, every one of the cluster.

## Measured

`TestCacheMemory` in `cmd/strait-controller/cache_test.go` starts the controller's cache on envtest's API server, and measures its live heap. Each Service has four labels, two annotations and two ports. Each has one EndpointSlice with three ready endpoints, each with a pod reference, a node and a zone. Each object has one `managedFields` entry. The 10,000 Services are spread over 20 namespaces.

| Cache | Services cached | Live heap | Per Service and its EndpointSlice |
|---|---|---|---|
| 1.2.x | 10,000 | 67 MiB | 7,056 bytes |
| 1.3.0, which leaves out `managedFields` | 10,000 | 53 MiB | 5,621 bytes |
| 1.3.0 with `controller.namespacedReads`, one namespace of the 20 | 500 | 3 MiB | 346 bytes per Service of the cluster |

With no Service, the controller's live heap was 2 MiB more than the test's own. An object with more labels, endpoints or `managedFields` entries takes more.

```sh
STRAIT_CACHE_MEMORY=10000 make test
```

## Setting the limit

| Value | Effect |
|---|---|
| `controller.resources.limits.memory` | The container's limit, 256Mi by default. The chart passes it to the controller, which sets the Go runtime's soft memory limit to 90% of it. Near that limit, the garbage collector runs more often, instead of letting the heap grow to twice what it holds. |
| `controller.resources.requests.memory` | 64Mi by default. Raise it with the limit, so that the scheduler finds the room. |

Leave twice the measured heap, and a margin of 128Mi for the binary, the Go runtime and the controller's other objects:

```
limits.memory >= 128Mi + 2 x 5.6 KiB x (Services of the cluster)
```

| Services in the cluster | `limits.memory` |
|---|---|
| up to 10,000 | 256Mi, the default |
| 30,000 | 512Mi |
| 80,000 | 1Gi |

A controller that runs out of memory restarts. Until the next one serves, the other sites count this site's members `Unknown` ([RFC0004](../rfcs/RFC0004.md)). The alert `StraitNoController` fires at this site, and `StraitPeerUnreachable` at the others ([Metrics and alerts](metrics.md)).

## Fewer objects in the cache

With `controller.namespaces` set, `controller.namespacedReads: true` limits the controller's reads of Services, EndpointSlices and CloudNativePG's objects to those namespaces and the release namespace ([RFC0007](../rfcs/RFC0007.md) requirement 26). The cache then holds only theirs, and the chart grants those reads in those namespaces only.

| Object in another namespace | With `namespacedReads` |
|---|---|
| `SiteService` | Its local members are `Unknown` with the read's error, it routes nowhere, and its `Applied` condition is `False` and names the namespace. With `controller.namespaces` alone, the controller cannot write its Service there either. |
| `Export` | No ready endpoint: its `Ready` condition is `False`, reason `ReadFailed`, and peers see it not ready. |
| `Import` with `spec.port` | Its `Service` condition is `False`, reason `SyncFailed`, and names the namespace. |
| `Import` without `spec.port` | Works as before. |
