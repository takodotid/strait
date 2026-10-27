//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// setup builds the fixture: two kind clusters joined by strait-net and strait-direct, CloudNativePG and Strait at both
// sites, the links between them, cluster-a as the primary of the topology and cluster-b as its replica, and SiteService pg.
func (e *env) setup(ctx context.Context, t *testing.T) {
	start := time.Now()
	e.preflight(ctx, t)
	for _, n := range []struct {
		name, subnet string
		internal     bool
	}{{netWAN, netWANSubnet, false}, {netDirect, netDirectSubnet, true}} {
		if err := e.ensureNetwork(ctx, n.name, n.subnet, n.internal); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range e.sites() {
		_ = os.RemoveAll(filepath.Join(e.out, s.name))
	}
	// Clusters left by an earlier run.
	if err := parallel(
		func() error { return e.deleteCluster(ctx, e.a) },
		func() error { return e.deleteCluster(ctx, e.b) },
	); err != nil {
		t.Fatal(err)
	}

	var archive, manifest string
	err := parallel(
		func() error { return e.createCluster(ctx, e.a) },
		func() error { return e.createCluster(ctx, e.b) },
		func() (err error) {
			archive, err = e.saveImages(ctx, []string{e.image, envoyRepository + ":" + envoyTag, postgresImage, cnpgImage, agnhostImage})
			return err
		},
		func() (err error) { manifest, err = e.fetchCNPGManifest(ctx); return err },
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range e.sites() {
		t.Logf("site %s: node %s, strait-net %s, strait-direct %s", s.name, s.node, s.wanIP, s.directIP)
	}
	e.times.record(t, "setup/clusters", time.Since(start))

	mark := time.Now()
	err = parallel(func() error { return e.loadImages(ctx, e.a, archive) }, func() error { return e.loadImages(ctx, e.b, archive) })
	_ = os.Remove(archive)
	if err != nil {
		t.Fatal(err)
	}
	e.times.record(t, "setup/images", time.Since(mark))

	// CloudNativePG first: the controller watches its kinds only when their CRDs exist at start.
	mark = time.Now()
	password := randomPassword()
	perSite := func(s *site) func() error {
		return func() error {
			if err := installCNPG(ctx, s, manifest); err != nil {
				return fmt.Errorf("site %s: cloudnative-pg: %w", s.name, err)
			}
			if err := e.installStrait(ctx, s); err != nil {
				return fmt.Errorf("site %s: strait: %w", s.name, err)
			}
			if err := waitCNPG(ctx, s); err != nil {
				return fmt.Errorf("site %s: cloudnative-pg: %w", s.name, err)
			}
			if err := dbFixture(ctx, s, password); err != nil {
				return fmt.Errorf("site %s: db: %w", s.name, err)
			}
			if err := webFixture(ctx, s); err != nil {
				return fmt.Errorf("site %s: web: %w", s.name, err)
			}
			return nil
		}
	}
	if err := parallel(perSite(e.a), perSite(e.b)); err != nil {
		t.Fatal(err)
	}
	e.times.record(t, "setup/install", time.Since(mark))

	mark = time.Now()
	err = parallel(
		func() error {
			if err := e.connectSites(ctx); err != nil {
				return err
			}
			for _, s := range e.sites() {
				if _, last, err := poll(ctx, 3*time.Minute, time.Second, func(ctx context.Context) (bool, string) { return e.linksSteady(ctx, s) }); err != nil {
					return fmt.Errorf("links: %w; last: %s", err, last)
				}
			}
			e.times.record(t, "setup/links", time.Since(mark))
			return nil
		},
		func() error {
			// cluster-a needs cluster-b's Secrets only once it is demoted, and the demotion stalls without them.
			if err := e.createCNPGCluster(ctx, e.a); err != nil {
				return err
			}
			if err := copySecrets(ctx, e.a, e.b, clusterOf(e.a)); err != nil {
				return err
			}
			if err := e.createCNPGCluster(ctx, e.b); err != nil {
				return err
			}
			if err := copySecrets(ctx, e.b, e.a, clusterOf(e.b)); err != nil {
				return err
			}
			e.times.record(t, "setup/postgres", time.Since(mark))
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range e.sites() {
		if _, err := s.kubectl(ctx, 4*time.Minute, "-n", dbNamespace, "wait", "--for=condition=Ready", "pod/"+clientPod, "--timeout=3m"); err != nil {
			t.Fatal(err)
		}
		if err := s.c.Create(ctx, pgService(e)); err != nil {
			t.Fatalf("site %s: SiteService pg: %v", s.name, err)
		}
	}
	e.times.record(t, "setup/total", time.Since(start))
}

// dump writes every Strait resource, the Strait pods' logs and the CloudNativePG Clusters of each site into _output/e2e/<site>/.
func (e *env) dump(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, s := range e.sites() {
		if s.c == nil {
			continue
		}
		dir := filepath.Join(e.out, s.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Logf("dump: %v", err)
			continue
		}
		save := func(file string, args ...string) {
			out, err := s.kubectl(ctx, time.Minute, args...)
			if err != nil {
				out += "\n" + err.Error()
			}
			_ = os.WriteFile(filepath.Join(dir, file), []byte(out), 0o644)
		}
		save("strait.yaml", "get", "siteservices,sitelinks,sitepeers,sites,exports,imports", "-A", "-o", "yaml")
		save("cnpg-clusters.yaml", "-n", dbNamespace, "get", "clusters.postgresql.cnpg.io", "-o", "yaml")
		save("pods.txt", "get", "pods", "-A", "-o", "wide")
		save("events.txt", "get", "events", "-A", "--sort-by=.lastTimestamp")
		var pods corev1.PodList
		if err := s.c.List(ctx, &pods, client.InNamespace(straitNamespace)); err != nil {
			t.Logf("dump: site %s: %v", s.name, err)
			continue
		}
		for _, p := range pods.Items {
			for _, c := range p.Spec.Containers {
				save(fmt.Sprintf("logs-%s-%s.txt", p.Name, c.Name), "-n", straitNamespace, "logs", p.Name, "-c", c.Name)
			}
			for _, cs := range p.Status.ContainerStatuses {
				if cs.RestartCount > 0 {
					save(fmt.Sprintf("logs-%s-%s-previous.txt", p.Name, cs.Name), "-n", straitNamespace, "logs", p.Name, "-c", cs.Name, "--previous")
				}
			}
		}
	}
	t.Logf("dumped the fixture's state into %s", e.out)
}

// noForbidden fails if a Strait controller or gateway agent logged a forbidden request: least-privilege RBAC must be enough.
func (e *env) noForbidden(ctx context.Context, t *testing.T) {
	for _, s := range e.sites() {
		var pods corev1.PodList
		if err := s.c.List(ctx, &pods, client.InNamespace(straitNamespace)); err != nil {
			t.Fatal(err)
		}
		checked := 0
		for _, p := range pods.Items {
			container := map[string]string{"controller": "controller", "gateway": "agent"}[p.Labels["app.kubernetes.io/component"]]
			if container == "" {
				continue
			}
			logs := []string{}
			args := [][]string{{"-n", straitNamespace, "logs", p.Name, "-c", container}}
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Name == container && cs.RestartCount > 0 {
					args = append(args, []string{"-n", straitNamespace, "logs", p.Name, "-c", container, "--previous"})
				}
			}
			for _, a := range args {
				out, err := s.kubectl(ctx, time.Minute, a...)
				if err != nil {
					t.Fatal(err)
				}
				logs = append(logs, strings.Split(out, "\n")...)
			}
			var hits []string
			for _, line := range logs {
				if strings.Contains(strings.ToLower(line), "forbidden") {
					hits = append(hits, line)
				}
			}
			if len(hits) > 0 {
				t.Errorf("site %s: %s/%s logged %d forbidden requests, first: %s", s.name, p.Name, container, len(hits), hits[0])
			}
			checked++
		}
		if checked == 0 {
			t.Errorf("site %s: no Strait pod found", s.name)
		}
		t.Logf("site %s: %d Strait containers logged no forbidden request", s.name, checked)
	}
}
