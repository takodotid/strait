// Package cli is the strait command line. It reads Strait's resources and explains them. It never writes.
package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/takodotid/strait/api/v1alpha1"
)

var outputs = []string{"table", "json", "yaml"}

type options struct {
	kubeContext   string
	kubeconfig    string
	namespace     string
	allNamespaces bool
	output        string
	skill         bool

	// connect returns a reader of the cluster and the namespace of the context. Tests replace it.
	connect func(o *options) (client.Reader, string, error)
	now     func() time.Time
}

// usageError is a mistake in the command line. It exits with status 2.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

var (
	// errDone ends a command that has done its work early. It exits with status 0.
	errDone = errors.New("done")
	// errFailed ends a command that has already printed why it failed. It exits with status 1.
	errFailed = errors.New("failed")
)

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError{fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())}
	}
	return nil
}

func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(n)(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

// Main runs the command line and returns the exit status.
// name is the command shown in usage lines: strait, or kubectl strait for the kubectl plugin.
func Main(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return run(NewRoot(name), args, stdin, stdout, stderr)
}

// NewRoot returns the command tree. Help, man pages and the skill document are generated from it.
func NewRoot(name string) *cobra.Command {
	return newRoot(name, &options{connect: connect, now: time.Now})
}

func run(root *cobra.Command, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	cmd, err := root.ExecuteC()
	var usage usageError
	switch {
	case err == nil, errors.Is(err, errDone):
		return 0
	case errors.Is(err, errFailed):
		return 1
	case errors.As(err, &usage):
		fmt.Fprintf(stderr, "error: %v\nRun '%s --help' for usage.\n", err, cmd.CommandPath())
		return 2
	}
	fmt.Fprintf(stderr, "error: %v\n", err)
	return 1
}

func newRoot(name string, o *options) *cobra.Command {
	root := &cobra.Command{
		Use:   "strait",
		Short: "Read and explain the state of Strait at one site",
		Long: `strait reads the state of Strait at one site and explains it.

Strait is a virtual service between Kubernetes clusters at different sites. It points one local name per dependency at the member that is active now, in its own site or another one, and carries TCP between sites.

strait is read-only. It never creates, updates, patches or deletes an object. Each site is read with its own kubeconfig context.`,
		Example: `  # Show the site, its peers and every application name.
  strait status --context site-a

  # Install the skill document for an AI agent.
  strait --skill > <skills dir>/strait/SKILL.md`,
		Args:              noArgs,
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if !slices.Contains(outputs, o.output) {
				return usageError{fmt.Errorf("--output must be one of table, json or yaml, not %q", o.output)}
			}
			if o.skill {
				if err := WriteSkill(cmd.OutOrStdout(), cmd.Root()); err != nil {
					return err
				}
				return errDone
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	if name != root.Use {
		root.Annotations = map[string]string{cobra.CommandDisplayNameAnnotation: name}
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return usageError{err}
	})

	f := root.PersistentFlags()
	f.StringVar(&o.kubeContext, "context", "", "The kubeconfig context to read. Defaults to the kubeconfig's current context.")
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "The kubeconfig file. Defaults to $KUBECONFIG, then ~/.kube/config.")
	f.StringVarP(&o.namespace, "namespace", "n", "", "The namespace of namespaced objects. Defaults to the context's namespace.")
	f.BoolVarP(&o.allNamespaces, "all-namespaces", "A", false, "Read namespaced objects in every namespace.")
	f.StringVarP(&o.output, "output", "o", "table", "The output format: table, json or yaml.")
	f.BoolVar(&o.skill, "skill", false, "Print the skill document for AI agents and exit.")

	root.AddCommand(newStatus(o), newExplain(o), newLinks(o), newPeers(o), newValidate(o))
	return root
}

// read returns a reader of the cluster and the namespace to read namespaced objects in. An empty namespace means every namespace.
func (o *options) read() (client.Reader, string, error) {
	r, contextNamespace, err := o.connect(o)
	if err != nil {
		return nil, "", err
	}
	switch {
	case o.allNamespaces:
		return r, "", nil
	case o.namespace != "":
		return r, o.namespace, nil
	}
	return r, contextNamespace, nil
}

// restConfig loads the kubeconfig and the exact context given. A context that does not exist is an error.
func restConfig(o *options) (*rest.Config, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = o.kubeconfig
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: o.kubeContext})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	ns, _, err := cc.Namespace()
	if err != nil {
		return nil, "", err
	}
	return cfg, ns, nil
}

// reader hides the write methods of a client, so that no type assertion reaches them.
type reader struct{ client.Reader }

func connect(o *options) (client.Reader, string, error) {
	ctrllog.SetLogger(logr.Discard())
	cfg, ns, err := restConfig(o)
	if err != nil {
		return nil, "", err
	}
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, "", err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, "", err
	}
	return reader{c}, ns, nil
}
