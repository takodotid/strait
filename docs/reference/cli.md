# The strait command

`strait` reads the state of Strait at one site and explains it. It is read-only. [Install](../guide/install.md#the-cli) puts it on the `PATH`, with `kubectl-strait` next to it: installed as `kubectl-strait` (`kubectl-strait.exe` on Windows), the same commands run as `kubectl strait`.

## Commands

| Command | Shows |
|---|---|
| `strait status` | The site and its controller's version, its peers, and where every application name points. |
| `strait explain SITESERVICE` | Why one name points where it does: the reason, every member's observation, every site's declaration. |
| `strait links` | Every link to a peer, its health, and which link carries the traffic. |
| `strait peers` | Every peer, and whether its observations can be read. |
| `strait validate -f PATH` | Checks manifests offline, with the rules of the CRDs. |
| `strait --skill` | Prints the skill document for AI agents. `strait --skill > <skills dir>/strait/SKILL.md` installs it. |
| `strait version` | The CLI's version and commit. `strait --version` prints them on one line. |
| `strait completion SHELL` | The completion script for `bash`, `zsh`, `fish` or `powershell`. |

`strait --help`, `strait COMMAND --help` and the man pages in the release archive cover every command and flag. The design is in [RFC0006](../../rfcs/RFC0006.md).

## Flags

| Flag | Does |
|---|---|
| `--context` | The kubeconfig context to read. Each command that reads a cluster reads one site, so pass `--context` for each. |
| `--kubeconfig` | The kubeconfig file. |
| `-n`, `--namespace` | The namespace of namespaced objects. |
| `-A`, `--all-namespaces` | Read namespaced objects in every namespace. |
| `-o`, `--output` | `table`, `json` or `yaml`, for every command that prints state, a validation or a version. |

## Exit status

| Status | When |
|---|---|
| 0 | Success. Validation warnings alone exit 0. |
| 1 | A validation error or a failed read. |
| 2 | A usage error. |

## Output on a terminal

On a terminal, `strait explain` marks the route with a symbol: ✔ a route to a confirmed member, ▲ a route kept while a member is `Unknown` or the controller awaits its peers, ■ a route to nowhere. Colours are off when `NO_COLOR` is set.

## Shell completion

Completion covers commands, flags, the values of `-o`, the kubeconfig's contexts, and the names of namespaces and `SiteService`s.

| Shell | Setup |
|---|---|
| bash | Needs the `bash-completion` package. Add `source <(strait completion bash)` to `~/.bashrc`. |
| zsh | Add `source <(strait completion zsh)` to `~/.zshrc`, after `compinit`. |
| fish | `strait completion fish > ~/.config/fish/completions/strait.fish` |
| PowerShell | Add `strait completion powershell \| Out-String \| Invoke-Expression` to `$PROFILE`. |

### The kubectl plugin

kubectl 1.26 or later runs a program named `kubectl_complete-strait` to complete `kubectl strait`. Put a link to the binary with that name on the `PATH`:

```sh
ln -s "$(command -v kubectl-strait)" ~/.local/bin/kubectl_complete-strait
```

On Windows, put a copy of `kubectl-strait.exe` named `kubectl_complete-strait.exe` next to it.

## On Windows

| Difference | Detail |
|---|---|
| Exit status of `kubectl strait` | 1 on every failure. kubectl adds the line `Error: exit status N`, because on Windows it runs a plugin as a child process. `strait.exe` exits as [RFC0006](../../rfcs/RFC0006.md) requirement 11 says. |
| Man pages | Windows has no `man`. `strait --help` and `strait COMMAND --help` hold the same text. |
| Skill document | Copy `SKILL.md` from the archive into `<skills dir>\strait\`. In Windows PowerShell 5.1, `strait --skill > SKILL.md` writes UTF-16LE, not the UTF-8 that `strait` prints. |
