# RFCs

Design decisions live in [`rfcs/`](../../rfcs/). Each RFC states what must be true, as requirements that a test, a dry run, a review or a look at a cluster can check. When the code and an RFC disagree, the RFC wins.

| RFC | Covers |
|---|---|
| [RFC0001](../../rfcs/RFC0001.md) | Scope, glossary, resources and components. |
| [RFC0002](../../rfcs/RFC0002.md) | `SiteService`, modes, policies, and the decision table. |
| [RFC0003](../../rfcs/RFC0003.md) | The plugin contract, `cnpg` and `declared`. |
| [RFC0004](../../rfcs/RFC0004.md) | The observation exchange. |
| [RFC0005](../../rfcs/RFC0005.md) | The data plane: links, transports, the gateway, endpoint-direct. |
| [RFC0006](../../rfcs/RFC0006.md) | The CLI. |
| [RFC0007](../../rfcs/RFC0007.md) | Security: permissions, credentials, links, releases. |

Each RFC's limitations are gathered on [Limitations](../limitations.md). The end-to-end tests, their kind fixture and their scenarios are in [`test/e2e/README.md`](../../test/e2e/README.md).
