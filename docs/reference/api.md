# Resources and chart values

## API

The API group is `strait.tako.id`, served as `v1`. `v1alpha1` is still served, deprecated, with the same schema. A change that breaks a user comes as a new API version. The versions and how they are stored are in [RFC0001, API versions](../../rfcs/RFC0001.md#api-versions). Moving objects from `v1alpha1` to `v1` is in [Upgrade](../runbooks/upgrade.md#from-01-to-10).

## Resources

Every resource, its scope and the RFC that owns it are in [RFC0001, Resources](../../rfcs/RFC0001.md#resources). Each site has one `Site`, one `SitePeer` per peer and one `SiteLink` per path, and one `SiteService` per application name.

| To read | Where |
|---|---|
| Every field, with its description | `kubectl --context SITE explain siteservice.spec`, or any other kind and field path. The Go types are in [`api/v1/`](../../api/v1/). |
| The CRDs | `strait-crds.yaml` in each [release](https://github.com/takodotid/strait/releases), generated into [`config/crd/`](../../config/crd/). |
| A `SiteService` and how it routes | [Concepts](../guide/concepts.md#a-siteservice) and [RFC0002](../../rfcs/RFC0002.md). |
| A plugin's fields | [RFC0003](../../rfcs/RFC0003.md). |
| `Site`, `SitePeer`, `SiteLink`, `Export` and `Import` | [RFC0005](../../rfcs/RFC0005.md). |
| Checking manifests before applying them | `strait validate -f PATH` ([The strait command](./cli.md)). |

## Chart values

Every value of the chart, with its comment, is in [`values.yaml`](../../deploy/helm/strait/values.yaml). Its top-level keys:

| Key | Sets |
|---|---|
| `image` | Which image the chart runs ([Install](../guide/install.md#which-image-the-chart-runs)). |
| `envoy` | The gateway's Envoy. |
| `controller` | The controller's Deployment. Its memory: [Sizing the controller](../sizing.md). |
| `gateway` | The gateway's Deployment, its replicas and its disruption budget. |
| `site` | The local `Site` that the chart renders. |
| `cnpg` | The `cnpg` plugin's access to CloudNativePG. |
| `metrics` | Scraping and alert rules ([Metrics and alerts](../metrics.md)). |
| `networkPolicy` | The NetworkPolicies of the controller and the gateway ([RFC0007](../../rfcs/RFC0007.md#network-policies)). |
