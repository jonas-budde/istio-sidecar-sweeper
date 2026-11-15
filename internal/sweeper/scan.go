package sweeper

import (
	"context"
	"fmt"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const proxyContainer = "istio-proxy"

// StalePod is a pod whose sidecar differs from what injection would produce now.
type StalePod struct {
	Namespace string
	Name      string
	HaveRev   string
	WantRev   string
	Have      string
	Want      string
	Owner     Workload
}

// NamespaceRevision returns the revision a namespace is injected with ("" if not injected).
func NamespaceRevision(ns *corev1.Namespace) string {
	if r := ns.Labels["istio.io/rev"]; r != "" {
		return r
	}
	if ns.Labels["istio-injection"] == "enabled" {
		return "default"
	}
	return ""
}

// PodRevision returns the revision a pod was injected by.
func PodRevision(p *corev1.Pod) string {
	if r := p.Annotations["istio.io/rev"]; r != "" {
		return r
	}
	if r := p.Labels["istio.io/rev"]; r != "" {
		return r
	}
	return "default"
}

// ProxyImage returns the istio-proxy image of a pod, or "" if it has no sidecar.
func ProxyImage(p *corev1.Pod) string {
	for _, list := range [][]corev1.Container{p.Spec.Containers, p.Spec.InitContainers} {
		for _, c := range list {
			if c.Name == proxyContainer {
				return c.Image
			}
		}
	}
	return ""
}

// Scan lists pods whose sidecar is out of date compared to the revision their
// namespace is currently labeled with. ns == "" scans all namespaces.
// Scan also returns warnings for namespaces that cannot be checked, for example when their
// revision no longer exists (typically after the previous revision was removed by an upgrade).
func Scan(ctx context.Context, cs kubernetes.Interface, istioNS, ns string, exclude ...string) ([]StalePod, []string, error) {
	nsList, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	wantRev := map[string]string{}
	for i := range nsList.Items {
		if r := NamespaceRevision(&nsList.Items[i]); r != "" && (ns == "" || ns == nsList.Items[i].Name) && !slices.Contains(exclude, nsList.Items[i].Name) {
			wantRev[nsList.Items[i].Name] = r
		}
	}

	expected := map[string]Expected{}
	var out []StalePod
	var warnings []string
	for name, rev := range wantRev {
		want, ok := expected[rev]
		if !ok {
			if want, err = LoadExpected(ctx, cs, istioNS, rev); err != nil {
				if apierrors.IsNotFound(err) {
					warnings = append(warnings, fmt.Sprintf("namespace %s is labeled with revision %q, but its injector config does not exist; relabel the namespace and restart its workloads", name, rev))
					continue
				}
				return nil, nil, err
			}
			expected[rev] = want
		}
		pods, err := cs.CoreV1().Pods(name).List(ctx, metav1.ListOptions{FieldSelector: "status.phase=Running"})
		if err != nil {
			return nil, nil, err
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			img := ProxyImage(p)
			if img == "" || p.DeletionTimestamp != nil {
				continue
			}
			have, haveRev := ImageTag(img), PodRevision(p)
			if have == want.Tag && haveRev == rev {
				continue
			}
			owner, err := TopOwner(ctx, cs, p, name)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, StalePod{name, p.Name, haveRev, rev, have, want.Tag, owner})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	sort.Strings(warnings)
	return out, warnings, nil
}
