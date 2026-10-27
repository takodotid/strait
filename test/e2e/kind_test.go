//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// preflight fails at once when the host lacks a tool, the pinned kind, the wireguard module or the Strait image.
func (e *env) preflight(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, tool := range []string{"docker", "kind", "kubectl", "helm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required on PATH: %v", tool, err)
		}
	}
	out, err := run(ctx, 30*time.Second, filepath.Join(e.dir, "kubeconfig-none"), nil, "kind", "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "kind "+kindVersion+" ") {
		t.Fatalf("kind %s is required, since its default node image is the pinned one; found: %s", kindVersion, strings.TrimSpace(out))
	}
	// The gateway's wireguard links need the module on the host kernel; the gateway agent does not load it (RFC0005).
	if _, err := os.Stat("/sys/module/wireguard"); err != nil {
		t.Fatal("the wireguard kernel module is not loaded on this host; load it with `sudo modprobe wireguard`")
	}
	if _, err := e.docker(ctx, 30*time.Second, "image", "inspect", "--format", "{{.Id}}", e.image); err != nil {
		t.Fatalf("the Strait image %s is not in the local docker image store; build it with `make images`: %v", e.image, err)
	}
}

// ensureNetwork creates a docker network when it is absent, and checks its subnet and internal flag when it exists.
func (e *env) ensureNetwork(ctx context.Context, name, subnet string, internal bool) error {
	inspect := func() (string, error) {
		return e.docker(ctx, 30*time.Second, "network", "inspect", "--format", "{{range .IPAM.Config}}{{.Subnet}} {{end}}{{.Internal}}", name)
	}
	out, err := inspect()
	if err != nil {
		args := []string{"network", "create", "--driver", "bridge", "--subnet", subnet}
		if internal {
			args = append(args, "--internal")
		}
		if _, cerr := e.docker(ctx, time.Minute, append(args, name)...); cerr != nil {
			// Another run may have created it at the same time.
			if out, err = inspect(); err != nil {
				return cerr
			}
		} else if out, err = inspect(); err != nil {
			return err
		}
	}
	want := fmt.Sprintf("%s %t", subnet, internal)
	if strings.TrimSpace(out) != want {
		return fmt.Errorf("docker network %s is %q, want %q (subnet, internal)", name, strings.TrimSpace(out), want)
	}
	return nil
}

// clusterExists reports whether kind knows the cluster.
func (e *env) clusterExists(ctx context.Context, s *site) (bool, error) {
	out, err := run(ctx, 30*time.Second, s.kubeconfig, nil, "kind", "get", "clusters")
	if err != nil {
		return false, err
	}
	for _, c := range strings.Fields(out) {
		if c == s.cluster {
			return true, nil
		}
	}
	return false, nil
}

// deleteCluster deletes the site's kind cluster if it exists. A paused node is unpaused first.
func (e *env) deleteCluster(ctx context.Context, s *site) error {
	ok, err := e.clusterExists(ctx, s)
	if err != nil || !ok {
		return err
	}
	e.unpause(ctx, s)
	_, err = run(ctx, 3*time.Minute, s.kubeconfig, nil, "kind", "delete", "cluster", "--name", s.cluster, "--kubeconfig", s.kubeconfig)
	return err
}

// nodes returns the site's node containers.
func (e *env) nodes(ctx context.Context, s *site) ([]string, error) {
	out, err := run(ctx, 30*time.Second, s.kubeconfig, nil, "kind", "get", "nodes", "--name", s.cluster)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// createCluster creates the site's kind cluster on strait-net, then attaches its node to strait-direct at the pinned address.
func (e *env) createCluster(ctx context.Context, s *site) error {
	cfg := fmt.Sprintf(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  podSubnet: %s
  serviceSubnet: %s
nodes:
  - role: control-plane
`, s.podCIDR, s.serviceCIDR)
	path := filepath.Join(e.dir, "kind-"+s.name+".yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		return err
	}
	cmd := []string{"KIND_EXPERIMENTAL_DOCKER_NETWORK=" + netWAN, "kind", "create", "cluster", "--name", s.cluster, "--config", path, "--kubeconfig", s.kubeconfig, "--wait", "180s"}
	if _, err := run(ctx, 6*time.Minute, s.kubeconfig, nil, "env", cmd...); err != nil {
		return err
	}
	nodes, err := e.nodes(ctx, s)
	if err != nil {
		return err
	}
	if len(nodes) != 1 {
		return fmt.Errorf("cluster %s has nodes %v, want one", s.cluster, nodes)
	}
	s.node = nodes[0]
	holders, err := e.docker(ctx, 30*time.Second, "network", "inspect", "--format", "{{range .Containers}}{{.Name}}={{.IPv4Address}} {{end}}", netDirect)
	if err != nil {
		return err
	}
	for _, h := range strings.Fields(holders) {
		name, addr, _ := strings.Cut(h, "=")
		if strings.HasPrefix(addr, s.directIP+"/") && name != s.node {
			return fmt.Errorf("%s on %s is held by container %s; free it before running the e2e tests", s.directIP, netDirect, name)
		}
	}
	if _, err := e.docker(ctx, time.Minute, "network", "connect", "--ip", s.directIP, netDirect, s.node); err != nil {
		return err
	}
	out, err := e.docker(ctx, 30*time.Second, "inspect", "--format", fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, netWAN), s.node)
	if err != nil {
		return err
	}
	s.wanIP = strings.TrimSpace(out)
	if s.wanIP == "" {
		return fmt.Errorf("node %s has no address on %s", s.node, netWAN)
	}
	return s.connect()
}

// saveImages pulls the images that are not local yet and saves them all into one single-platform archive.
// `kind load docker-image` fails with docker's containerd image store, so images go through an archive.
func (e *env) saveImages(ctx context.Context, images []string) (string, error) {
	platform := "linux/" + runtime.GOARCH
	err := parallel(func() error {
		var fns []func() error
		for _, img := range images {
			fns = append(fns, func() error {
				if _, err := e.docker(ctx, 30*time.Second, "image", "inspect", "--format", "{{.Id}}", img); err == nil {
					return nil
				}
				_, err := e.docker(ctx, 10*time.Minute, "pull", "--quiet", "--platform", platform, img)
				return err
			})
		}
		return parallel(fns...)
	})
	if err != nil {
		return "", err
	}
	path := filepath.Join(e.out, "images.tar")
	args := append([]string{"save", "--platform", platform, "-o", path}, images...)
	if _, err := e.docker(ctx, 10*time.Minute, args...); err != nil {
		return "", err
	}
	return path, nil
}

// loadImages loads an image archive into the site's node.
func (e *env) loadImages(ctx context.Context, s *site, archive string) error {
	_, err := run(ctx, 10*time.Minute, s.kubeconfig, nil, "kind", "load", "image-archive", "--name", s.cluster, archive)
	return err
}

// fetchCNPGManifest downloads the pinned CloudNativePG manifest and checks its digest.
func (e *env) fetchCNPGManifest(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cnpgManifestURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", cnpgManifestURL, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != cnpgManifestSHA256 {
		return "", fmt.Errorf("%s has sha256 %s, want %s", cnpgManifestURL, got, cnpgManifestSHA256)
	}
	path := filepath.Join(e.dir, "cnpg.yaml")
	return path, os.WriteFile(path, b, 0o644)
}
