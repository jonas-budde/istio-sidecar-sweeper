package sweeper

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const restartedAt = "kubectl.kubernetes.io/restartedAt"

// Workload is the top-level controller that owns a pod.
type Workload struct {
	Namespace string
	Kind      string
	Name      string
}

func (w Workload) String() string { return w.Namespace + "/" + w.Kind + "/" + w.Name }

// ImageTag returns the tag of an image reference ("" if none or digest only).
func ImageTag(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		return image[colon+1:]
	}
	return ""
}

// TopOwner resolves Pod -> ReplicaSet -> Deployment; other controllers are returned as is.
func TopOwner(ctx context.Context, cs kubernetes.Interface, p metav1.Object, ns string) (Workload, error) {
	for _, o := range p.GetOwnerReferences() {
		if o.Controller == nil || !*o.Controller {
			continue
		}
		if o.Kind == "ReplicaSet" {
			rs, err := cs.AppsV1().ReplicaSets(ns).Get(ctx, o.Name, metav1.GetOptions{})
			if err != nil {
				return Workload{}, fmt.Errorf("get replicaset %s/%s: %w", ns, o.Name, err)
			}
			for _, ro := range rs.OwnerReferences {
				if ro.Controller != nil && *ro.Controller && ro.Kind == "Deployment" {
					return Workload{ns, "Deployment", ro.Name}, nil
				}
			}
		}
		return Workload{ns, o.Kind, o.Name}, nil
	}
	return Workload{ns, "Pod", p.GetName()}, nil
}

// Workloads returns the distinct owners of the stale pods.
func Workloads(stale []StalePod) []Workload {
	seen := map[Workload]bool{}
	var out []Workload
	for _, s := range stale {
		if !seen[s.Owner] {
			seen[s.Owner] = true
			out = append(out, s.Owner)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// Options controls Restart.
type Options struct {
	Apply       bool
	Timeout     time.Duration
	AllowSingle bool
}

// Result describes what happened to one workload.
type Result struct {
	Workload Workload
	Action   string // restarted, would-restart, skipped
	Reason   string
}

// Restart triggers a rolling restart per workload, one at a time, and waits for each rollout.
// Bare pods, Jobs and single-replica workloads (unless allowed) are skipped.
func Restart(ctx context.Context, cs kubernetes.Interface, ws []Workload, o Options) ([]Result, error) {
	var res []Result
	for _, w := range ws {
		replicas, reason := restartable(ctx, cs, w)
		switch {
		case reason != "":
			res = append(res, Result{w, "skipped", reason})
			continue
		case replicas < 2 && !o.AllowSingle:
			res = append(res, Result{w, "skipped", "single replica, would cause downtime (use --allow-single-replica)"})
			continue
		case !o.Apply:
			res = append(res, Result{w, "would-restart", ""})
			continue
		}
		if err := patchRestart(ctx, cs, w); err != nil {
			return res, err
		}
		if err := waitRollout(ctx, cs, w, o.Timeout); err != nil {
			return res, fmt.Errorf("%s: %w", w, err)
		}
		res = append(res, Result{w, "restarted", ""})
	}
	return res, nil
}

// restartable returns the desired replica count and a reason if the workload must be skipped.
// A workload is only restarted when all of its replicas are ready, so a restart never starts
// from an already degraded state.
func restartable(ctx context.Context, cs kubernetes.Interface, w Workload) (int32, string) {
	switch w.Kind {
	case "Deployment":
		d, err := cs.AppsV1().Deployments(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
		if err != nil {
			return 0, err.Error()
		}
		return ptrOr(d.Spec.Replicas, 1), notReady(d.Status.ReadyReplicas, ptrOr(d.Spec.Replicas, 1))
	case "StatefulSet":
		s, err := cs.AppsV1().StatefulSets(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
		if err != nil {
			return 0, err.Error()
		}
		return ptrOr(s.Spec.Replicas, 1), notReady(s.Status.ReadyReplicas, ptrOr(s.Spec.Replicas, 1))
	case "DaemonSet":
		d, err := cs.AppsV1().DaemonSets(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
		if err != nil {
			return 0, err.Error()
		}
		return d.Status.DesiredNumberScheduled, notReady(d.Status.NumberReady, d.Status.DesiredNumberScheduled)
	}
	return 0, "owner kind " + w.Kind + " is not restarted by this tool"
}

func notReady(ready, want int32) string {
	if ready < want {
		return fmt.Sprintf("not all replicas ready (%d/%d)", ready, want)
	}
	return ""
}

func ptrOr(p *int32, d int32) int32 {
	if p == nil {
		return d
	}
	return *p
}

func patchRestart(ctx context.Context, cs kubernetes.Interface, w Workload) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`, restartedAt, time.Now().UTC().Format(time.RFC3339))
	var err error
	switch w.Kind {
	case "Deployment":
		_, err = cs.AppsV1().Deployments(w.Namespace).Patch(ctx, w.Name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	case "StatefulSet":
		_, err = cs.AppsV1().StatefulSets(w.Namespace).Patch(ctx, w.Name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	case "DaemonSet":
		_, err = cs.AppsV1().DaemonSets(w.Namespace).Patch(ctx, w.Name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	}
	return err
}

func waitRollout(ctx context.Context, cs kubernetes.Interface, w Workload, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		switch w.Kind {
		case "Deployment":
			d, err := cs.AppsV1().Deployments(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return deploymentDone(d), nil
		case "StatefulSet":
			s, err := cs.AppsV1().StatefulSets(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return s.Status.ObservedGeneration >= s.Generation && s.Status.UpdatedReplicas == ptrOr(s.Spec.Replicas, 1) && s.Status.ReadyReplicas == ptrOr(s.Spec.Replicas, 1), nil
		case "DaemonSet":
			d, err := cs.AppsV1().DaemonSets(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedNumberScheduled == d.Status.DesiredNumberScheduled && d.Status.NumberReady == d.Status.DesiredNumberScheduled, nil
		}
		return true, nil
	})
}

func deploymentDone(d *appsv1.Deployment) bool {
	want := ptrOr(d.Spec.Replicas, 1)
	return d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas == want &&
		d.Status.Replicas == want &&
		d.Status.AvailableReplicas == want
}
