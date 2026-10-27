# AGENTS.md

## Project overview

Strait is a virtual service between Kubernetes clusters at different sites. An application uses one local name per dependency, such as a database primary. Strait points that name at the member that is active now, in its own site or another one, and carries the traffic between sites.

- Strait routes. It never fails over, promotes, demotes or reconfigures a workload. The workload's own controller, or the operator, does that, and Strait follows.

Design decisions live in `rfcs/`. Read the RFCs that cover a subject before changing anything in it.

## Scope

Belongs here:
- the virtual service: which member each application name points at, in every site
- plugins that observe a workload's state
- the exchange of observations between sites
- the data plane between sites: links, their transports, the gateway
- the `strait` CLI, read-only

Does not belong here:
- **Failover, switchover, promotion, fencing.** These belong to the workload's own controller or the operator.
- **Multi-hop routing between sites, traffic weights, link capacity or bandwidth awareness.**
- **L7.** Strait carries TCP and never parses a protocol.
- **Pod-to-pod networking between clusters.** That is a CNI's job.

## Rules

1. **Observe, never act on a workload.**
   - A plugin reads a workload's state and nothing else.
   - It gets read-only permissions and read-only credentials.
2. **Route nowhere rather than to a guess.**
   - Two active members, or sites whose declarations disagree, empty the endpoints.
   - A policy that relaxes this is explicit, opt-in, and named in the resource's spec.
3. **Declarative first.**
   - Every behaviour is set in a resource's spec, and every observation shows in its status.
   - Nothing is configured by hand on a gateway or a node.
4. **Intent, not state.**
   - RFCs describe what should be true.
   - Observed state belongs to status, events, metrics and logs, not to an RFC.
5. **State a fact once.** A value lives in one place, and everything else references it.
6. **Plan before apply.**
   - Run a dry run or a diff first, and read it.
   - Changing a live cluster, a link's encryption, or a credential needs the operator's explicit approval.
7. **Secrets never in the repo.**
   - Secrets come from a secret store at runtime. Files hold references, never values.
   - Never print a secret into output or logs.
8. **Check facts, never assume.**
   - Verify against a live cluster or the primary source before acting. If a resource cannot be reached, stop and ask the operator.
   - Pass `--context` explicitly on every `kubectl` command.

## RFCs

### Format

One file per RFC: `rfcs/RFCNNNN.md`, numbered in order and never reused.

```markdown
<h1 style="text-align: center;">[RFC#NNNN] Title</h1>

# Abstract
# Status
- Version: X.Y
- Status: Draft | Active | Inactive
- Created: YYYY-MM-DD
- Last Updated: YYYY-MM-DD
# Table of Contents
# Overview
# Motivation          (optional: observed failures or reasons, so a rule is not undone by accident)
# Requirements
# Design
# Implementation      (when applicable: where the repo realises it, paths not copies)
# Security Considerations   (when applicable)
# Limitations
# Open Questions      (Draft only)
# References
```

An RFC MAY add a section its subject needs, such as a Glossary after the Overview.

### Writing them

- **Declarative.** An RFC states the end state and its constraints: values, boundaries, invariants. Step-by-step work goes in `docs/runbooks/`, and the RFC links to it.
- **Normative.**
  - Requirements use MUST, MUST NOT, SHOULD and MAY (RFC 2119).
  - Every MUST is checkable: by a test, by a dry run, by review of a change, or by inspecting a cluster. If it cannot be checked, rewrite it until it can.
  - Rules go in Requirements. Design describes the shape that satisfies them.
- **Scoped.** One subject per RFC. If two RFCs touch, one owns the subject and the other references it as `[RFC#NNNN]`.
- **No index.** `rfcs/` is the index, and each Status block is the source of truth.
- **Implementation conforms.** If the code and an RFC disagree, the RFC wins. Amend the RFC first, in the same change or an earlier one.

### API changes

The resources Strait serves are its contract with its users.
- A change that would break a user is a new API version. Compatible additions are fine in the same version.
- Record the operator's agreement in the change's description.

### Lifecycle

| Status | Meaning |
|---|---|
| Draft | Under discussion. The code MUST NOT rely on it yet. May carry Open Questions. |
| Active | In force. The code conforms. |
| Inactive | Retired or superseded. Kept for history. The replacing RFC says "This RFC deprecates RFC#NNNN." |

- **Promote Draft to Active only with the operator's approval**, once every Open Question is resolved and the section is removed.
- A decision deliberately deferred to a later trigger is a Limitation, not an Open Question.
- **Bump the version and Last Updated on every change.**

## Writing (docs, comments, commits, replies)

- **English only. Neutral:** refer to "the operator" or a role, never a person, and no personal pronouns.
- **Not written = doesn't exist.** Don't write what's absent, a default, general knowledge or history. Delete what's obsolete; git is the archive.
- **Dead simple.** Short sentences, everyday words. No intros, no filler, no em-dashes.
- **Tables over prose for facts.** Each table row is one record.
- **A fact lives in one doc;** link, don't copy.
- Talk to the operator in their language, just as plain.

## Commits

Conventional commits: `type(scope): subject`. The type is one of `feat`, `fix`, `docs`, `chore`, `refactor` or `test`. The scope is an area or an RFC, for example `docs(rfc0002): define the link resource`.

Commit and push only when the operator asks.

## Current state

- Started on 2026-10-27. v1 is built on the Draft RFCs in `rfcs/`.
- Commands:

| Command | Does |
|---|---|
| `make generate` | Generates deepcopy code and CRDs from `api/`, and copies them into the chart. |
| `make verify` | Fails if generated files are stale, `gofmt` or `go vet` complain. |
| `make build` | Builds every binary into `bin/`. |
| `make test` | Runs unit tests. |
| `make docs` | Writes the man pages and the skill document into `bin/`. |
| `make images` | Builds the container image. |
| `make e2e` | Runs the end-to-end tests on kind clusters named `strait-e2e-*`. |
| `make bench` | Runs the gateway benchmark on kind clusters named `strait-bench-*`. See `docs/benchmark.md`. |
