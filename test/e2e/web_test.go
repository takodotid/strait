//go:build e2e

package e2e

import (
	"context"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// webOf is the name of the site's web server, its Service and its member.
func webOf(s *site) string { return "web-" + s.name }

// webFixture runs one small HTTP server per site, behind a Service of its own name.
func webFixture(ctx context.Context, s *site) error {
	if err := ensureNamespace(ctx, s.c, webNamespace, nil); err != nil {
		return err
	}
	name := webOf(s)
	labels := map[string]string{"app": name}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: webNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: new(int64),
					Containers: []corev1.Container{{
						Name:            "netexec",
						Image:           agnhostImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            []string{"netexec", "--http-port=8080"},
						Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(8080)}},
							PeriodSeconds: 2,
						},
					}},
				},
			},
		},
	}
	if err := s.c.Create(ctx, dep); err != nil {
		return err
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: webNamespace},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Name: "http", Port: 8080, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	return s.c.Create(ctx, svc)
}

// webService is the SiteService web, with the declared plugin naming active.
func webService(e *env, active string) *straitv1.SiteService {
	return &straitv1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: webNamespace},
		Spec: straitv1.SiteServiceSpec{
			Mode:   straitv1.ModeSingle,
			Port:   straitv1.ServicePort{Name: "http", Port: 8080},
			Plugin: straitv1.PluginSpec{Declared: &straitv1.DeclaredPlugin{Active: active}},
			Members: []straitv1.Member{
				{Name: webOf(e.a), Site: e.a.name, Service: &straitv1.ServicePortRef{Name: webOf(e.a), Port: 8080}},
				{Name: webOf(e.b), Site: e.b.name, Service: &straitv1.ServicePortRef{Name: webOf(e.b), Port: 8080}},
			},
		},
	}
}

// pgService is the SiteService pg. Both sites apply the same one.
func pgService(e *env) *straitv1.SiteService {
	return &straitv1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: dbNamespace},
		Spec: straitv1.SiteServiceSpec{
			Mode:   straitv1.ModeSingle,
			Port:   straitv1.ServicePort{Name: "postgres", Port: 5432},
			Plugin: straitv1.PluginSpec{CNPG: &straitv1.CNPGPlugin{Role: straitv1.CNPGRolePrimary}},
			Members: []straitv1.Member{
				{Name: memberOf(e.a), Site: e.a.name, CNPG: &straitv1.CNPGMember{Cluster: clusterOf(e.a)}},
				{Name: memberOf(e.b), Site: e.b.name, CNPG: &straitv1.CNPGMember{Cluster: clusterOf(e.b)}},
			},
			Policy: straitv1.Policy{UnknownHold: metav1.Duration{Duration: unknownHold}},
		},
	}
}

// requestReaches returns a check that a request from the site's own web server to the Service, in namespace web,
// is answered by the member.
func requestReaches(s *site, service, member string) func(ctx context.Context) (bool, string) {
	return func(ctx context.Context) (bool, string) {
		out, err := s.kubectl(ctx, 15*time.Second, "-n", webNamespace, "exec", "deploy/"+webOf(s), "--",
			"curl", "-sS", "--max-time", "3", "http://"+service+"."+webNamespace+".svc:8080/hostname")
		if err != nil {
			return false, "site " + s.name + ": " + err.Error()
		}
		host := strings.TrimSpace(out)
		return strings.HasPrefix(host, member+"-"), "site " + s.name + ": answered by " + host
	}
}
