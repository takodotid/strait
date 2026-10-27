# Strait

Strait is a virtual service between Kubernetes clusters at different sites.

An application uses one local name per dependency, such as `pg.db.svc` for a PostgreSQL primary. Strait points that name at the member that is active now, in its own site or another one, and carries the TCP traffic between sites.

Strait routes. It never fails over, promotes, demotes or reconfigures a workload. The workload's own controller, or the operator, does that, and Strait follows.

```
site b                                                          site a
app -> pg.db.svc -> gateway b --link (direct or wireguard)--> gateway a -> pg-a primary
```

| Piece | Does |
|---|---|
| `SiteService` | One application name: its members across sites, a mode, a plugin, a policy. |
| Plugins | Read a workload's state, read-only. v1: `cnpg` (CloudNativePG) and `declared`. |
| Observation exchange | Each site publishes what it sees. Peers poll it, so every site sees every member. |
| Gateway | Envoy per site, configured over xDS. Links per pair of sites, lowest-cost healthy one first. |
| `strait` | Read-only CLI. Also `kubectl strait`. |

Design and decisions: [`rfcs/`](rfcs/). Start with [RFC0001](rfcs/RFC0001.md).

## Quick start

The end-to-end test builds two sites on kind, with CloudNativePG in its distributed topology, and runs every scenario: switchover, site loss, a partition, two writable members, disagreeing declarations, link failover, and an Unknown member held then routed nowhere.

Prerequisites:

| Tool | Version |
|---|---|
| A Linux host with Docker | 8 vCPU and 16 GiB are enough |
| Go | 1.27 |
| kind | v0.33.0 |
| kubectl, helm | recent |
| Kernel module | `wireguard` (`sudo modprobe wireguard`) |

Run:

```sh
make e2e
```

It builds the image, creates the kind clusters `strait-e2e-a` and `strait-e2e-b`, runs the scenarios, and deletes the clusters. To keep them and look around:

```sh
STRAIT_E2E_KEEP=1 make e2e
make build
bin/strait --kubeconfig <kubeconfig of site b> --context kind-strait-e2e-b -n db explain pg
```

The test prints where it wrote the kubeconfigs. See [`test/e2e/README.md`](test/e2e/README.md) for details.

## A `SiteService`

Apply the same object at every site:

```yaml
apiVersion: strait.tako.id/v1alpha1
kind: SiteService
metadata:
  name: pg
  namespace: db
spec:
  mode: Single
  port: {name: postgres, port: 5432}
  plugin:
    cnpg: {role: primary}
  members:
    - {name: pg-a, site: a, cnpg: {cluster: cluster-a}}
    - {name: pg-b, site: b, cnpg: {cluster: cluster-b}}
  policy:
    unknownHold: 30s
```

Applications connect to `pg.db.svc:5432`. Strait writes that Service's endpoints: the primary's pods when it is local, the local gateway when it is remote, and nothing when it cannot tell which member is writable.

## Commands

| Command | Does |
|---|---|
| `make generate` | Generates deepcopy code and CRDs from `api/`. |
| `make verify` | Fails if generated files are stale, or `gofmt` or `go vet` complain. |
| `make build` | Builds `strait`, `strait-controller` and `strait-gateway` into `bin/`. |
| `make test` | Runs unit tests. |
| `make docs` | Writes the man pages and the skill document into `bin/`. |
| `make images` | Builds the container image. |
| `make e2e` | Runs the end-to-end tests on kind. |
| `make bench` | Runs the gateway benchmark. Results: [`docs/benchmark.md`](docs/benchmark.md). |

For AI agents: `strait --skill > <skills dir>/strait/SKILL.md` installs a skill that explains how to read Strait's state.

## Install

Each release publishes the image `ghcr.io/takodotid/strait` (linux/amd64, linux/arm64), the Helm chart `oci://ghcr.io/takodotid/charts/strait`, and the CLI archives for Linux and macOS with `SHA256SUMS` on the [releases page](https://github.com/takodotid/strait/releases). The chart's source is [`deploy/helm/strait`](deploy/helm/strait). The gateway's namespace needs the Pod Security level `privileged`, since the gateway agent holds WireGuard interfaces with `NET_ADMIN` ([RFC0007](rfcs/RFC0007.md)).

## License

Apache-2.0. See [LICENSE](LICENSE).
