# Sizing the controller

The controller's memory grows with the Services and EndpointSlices that its cache holds. By default, that is every one in the cluster.

## Setting the limit

The limit by the size of the cluster:

| Services in the cluster | `limits.memory` |
|---|---|
| up to 10,000 | 256Mi, the default |
| 30,000 | 512Mi |
| 80,000 | 1Gi |

The rule: leave twice the measured heap ([Measured](#measured)), and a margin of 128Mi: `limits.memory >= 128Mi + 2 x 5.5 KiB x (Services of the cluster)`.

The values that set it:

| Value | Effect |
|---|---|
| `controller.resources.limits.memory` | The container's limit, 256Mi by default. The controller sets the Go runtime's soft memory limit to 90% of it. |
| `controller.resources.requests.memory` | 64Mi by default. Raise it together with the limit, so that the scheduler finds the room. |

A controller that runs out of memory restarts. Until the next one serves, the other sites count this site's members `Unknown` ([RFC0004](../rfcs/RFC0004.md)). The alert `StraitNoController` fires at this site, and `StraitPeerUnreachable` at the others ([Metrics and alerts](metrics.md)).

## Measured

`TestCacheMemory` in `cmd/strait-controller/cache_test.go` measures the controller's live heap on envtest's API server, with 10,000 Services:

| Cache | Services cached | Live heap, MiB | Bytes per Service and its EndpointSlice |
|---|---|---|---|
| `managedFields` left out | 10,000 | 53 | 5,621 |
| With `controller.namespacedReads`, one namespace of 20 | 500 | 3 | 346 |

In the last row, the bytes are per Service of the cluster.

```sh
STRAIT_CACHE_MEMORY=10000 make test
```

## Fewer objects in the cache

With `controller.namespaces` set, `controller.namespacedReads: true` limits the controller's reads of Services, EndpointSlices and CloudNativePG's objects to those namespaces and the release namespace ([RFC0007](../rfcs/RFC0007.md) requirement 26). The cache then holds only theirs, and the chart grants those reads in those namespaces only.

A `SiteService`, `Export` or `Import` with `spec.port` in another namespace is not read: its conditions show the error, and a `SiteService` routes nowhere. Details: RFC0007 requirement 26.
