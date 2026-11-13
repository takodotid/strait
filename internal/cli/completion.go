package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// completionTimeout bounds each read of the cluster that a completion makes. A completion that runs out of time offers
// nothing.
const completionTimeout = 2 * time.Second

func newCompletion() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion bash|zsh|fish|powershell",
		Short: "Print the shell completion script",
		Long: `completion prints the script that lets a shell complete strait's commands, flags and values. Load it in the shell's start-up file.

It completes commands, flags, the values of -o, the contexts of the kubeconfig, and the names of namespaces and SiteServices. Names are read from the cluster of the context, with a time limit of 2s. A completion never asks for credentials: a kubeconfig whose credential plugin needs a person to log in gets no names.

For kubectl strait, kubectl runs kubectl_complete-strait for completion. Put a link of that name to the strait binary on the PATH, next to kubectl-strait.`,
		Example: `  # bash: in ~/.bashrc
  source <(strait completion bash)

  # zsh: in ~/.zshrc, after compinit
  source <(strait completion zsh)

  # fish
  strait completion fish > ~/.config/fish/completions/strait.fish

  # PowerShell: in $PROFILE
  strait completion powershell | Out-String | Invoke-Expression

  # kubectl strait, with kubectl-strait on the PATH
  ln -s "$(command -v kubectl-strait)" ~/.local/bin/kubectl_complete-strait`,
		Args:      exactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root, w := cmd.Root(), cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(w, true)
			case "zsh":
				return root.GenZshCompletion(w)
			case "fish":
				return root.GenFishCompletion(w, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(w)
			}
			return usageError{fmt.Errorf("unknown shell %q: pick bash, zsh, fish or powershell", args[0])}
		},
	}
	return cmd
}

// noCompletion offers nothing and no file names, for a command that takes no argument.
func noCompletion(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// completeOutputs offers the values of --output.
func completeOutputs(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return slices.Clone(outputs), cobra.ShellCompDirectiveNoFileComp
}

// completeContexts offers the contexts of the kubeconfig. It reads the kubeconfig file only.
func (o *options) completeContexts(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = o.kubeconfig
	cfg, err := rules.Load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for name := range cfg.Contexts {
		if strings.HasPrefix(name, toComplete) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, cobra.ShellCompDirectiveNoFileComp
}

func inNamespace(ns string) []client.ListOption {
	if ns == "" {
		return nil
	}
	return []client.ListOption{client.InNamespace(ns)}
}

// siteServices reads the SiteServices of the namespace given, or of every namespace, within completionTimeout.
func (o *options) siteServices(cmd *cobra.Command, everyNamespace bool) ([]straitv1.SiteService, error) {
	o.completing = true
	r, ns, err := o.read()
	if err != nil {
		return nil, err
	}
	if everyNamespace {
		ns = ""
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
	defer cancel()
	var list straitv1.SiteServiceList
	if err := r.List(ctx, &list, inNamespace(ns)...); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// completeNamespaces offers the namespaces that hold a SiteService.
func (o *options) completeNamespaces(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	items, err := o.siteServices(cmd, true)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, ss := range items {
		if strings.HasPrefix(ss.Namespace, toComplete) && !slices.Contains(out, ss.Namespace) {
			out = append(out, ss.Namespace)
		}
	}
	slices.Sort(out)
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completeSiteServices offers the names of the SiteServices in the namespace that explain reads.
func (o *options) completeSiteServices(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	items, err := o.siteServices(cmd, false)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, ss := range items {
		if strings.HasPrefix(ss.Name, toComplete) && !slices.Contains(out, ss.Name) {
			out = append(out, ss.Name)
		}
	}
	slices.Sort(out)
	return out, cobra.ShellCompDirectiveNoFileComp
}
