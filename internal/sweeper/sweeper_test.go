package sweeper

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func ptr[T any](v T) *T { return &v }

func injector(rev, tag string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "istio-sidecar-injector-" + rev, Namespace: "aks-istio-system"},
		Data:       map[string]string{"values": `{"global":{"hub":"mcr.example/istio","tag":"` + tag + `","proxy":{"image":"proxyv2"}}}`},
	}
}

func pod(name, rev, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "app", Annotations: map[string]string{"istio.io/rev": rev},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc", Controller: ptr(true)}},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}, {Name: "istio-proxy", Image: image}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func fixture() *fake.Clientset {
	return fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app", Labels: map[string]string{"istio.io/rev": "asm-1-30"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "plain"}},
		injector("asm-1-30", "1.30.2"),
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "app",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", Controller: ptr(true)}}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app"}, Spec: appsv1.DeploymentSpec{Replicas: ptr(int32(2))}, Status: appsv1.DeploymentStatus{ReadyReplicas: 2}},
		pod("old-patch", "asm-1-30", "mcr.example/istio/proxyv2:1.30.1"),
		pod("old-rev", "asm-1-29", "mcr.example/istio/proxyv2:1.30.2"),
		pod("current", "asm-1-30", "mcr.example/istio/proxyv2:1.30.2"),
	)
}

func TestScan(t *testing.T) {
	got, _, err := Scan(context.Background(), fixture(), "aks-istio-system", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "old-patch" || got[1].Name != "old-rev" {
		t.Fatalf("want old-patch and old-rev, got %+v", got)
	}
	if got[0].Owner != (Workload{"app", "Deployment", "web"}) {
		t.Fatalf("owner = %v", got[0].Owner)
	}
}

func TestScanWarnsOnRemovedRevision(t *testing.T) {
	cs := fixture()
	cs.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "old", Labels: map[string]string{"istio.io/rev": "asm-1-29"}}}, metav1.CreateOptions{})
	got, warnings, err := Scan(context.Background(), cs, "aks-istio-system", "")
	if err != nil || len(got) != 2 || len(warnings) != 1 {
		t.Fatalf("got %v, %v, %v", got, warnings, err)
	}
}

func TestRestartDryRunAndApply(t *testing.T) {
	cs := fixture()
	stale, _, _ := Scan(context.Background(), cs, "aks-istio-system", "")
	ws := Workloads(stale)
	if len(ws) != 1 {
		t.Fatalf("want 1 workload, got %v", ws)
	}

	res, err := Restart(context.Background(), cs, ws, Options{Timeout: time.Second})
	if err != nil || res[0].Action != "would-restart" {
		t.Fatalf("dry run: %v %v", res, err)
	}
	d, _ := cs.AppsV1().Deployments("app").Get(context.Background(), "web", metav1.GetOptions{})
	if _, ok := d.Spec.Template.Annotations[restartedAt]; ok {
		t.Fatal("dry run must not patch")
	}
}

func TestRestartSkipsUnreadyWorkload(t *testing.T) {
	cs := fixture()
	d, _ := cs.AppsV1().Deployments("app").Get(context.Background(), "web", metav1.GetOptions{})
	d.Status.ReadyReplicas = 1
	cs.AppsV1().Deployments("app").UpdateStatus(context.Background(), d, metav1.UpdateOptions{})

	res, _ := Restart(context.Background(), cs, []Workload{{"app", "Deployment", "web"}}, Options{Apply: true, Timeout: time.Second})
	if res[0].Action != "skipped" || res[0].Reason != "not all replicas ready (1/2)" {
		t.Fatalf("got %v", res)
	}
}

func TestScanExcludesNamespaces(t *testing.T) {
	got, _, _ := Scan(context.Background(), fixture(), "aks-istio-system", "", "app")
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestRestartSkipsSingleReplica(t *testing.T) {
	cs := fixture()
	d, _ := cs.AppsV1().Deployments("app").Get(context.Background(), "web", metav1.GetOptions{})
	d.Spec.Replicas = ptr(int32(1))
	cs.AppsV1().Deployments("app").Update(context.Background(), d, metav1.UpdateOptions{})

	res, _ := Restart(context.Background(), cs, []Workload{{"app", "Deployment", "web"}}, Options{Apply: true, Timeout: time.Second})
	if res[0].Action != "skipped" {
		t.Fatalf("got %v", res)
	}
}

func TestParseInjectorValuesAKSFormat(t *testing.T) {
	got, err := ParseInjectorValues("asm-1-29", `{"global":{"hub":"mcr.microsoft.com/oss/v2/istio","tag":"1.29.10-1","variant":"distroless","proxy":{"image":"mcr.microsoft.com/oss/v2/istio/proxyv2:v1.29.10-1"}}}`)
	if err != nil || got.Tag != "v1.29.10-1" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestImageTag(t *testing.T) {
	for in, want := range map[string]string{
		"mcr.io/istio/proxyv2:1.30.2":           "1.30.2",
		"localhost:5000/proxyv2":                "",
		"localhost:5000/proxyv2:1.2-distroless": "1.2-distroless",
		"proxyv2@sha256:abc":                    "",
	} {
		if got := ImageTag(in); got != want {
			t.Errorf("ImageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProviderByName(t *testing.T) {
	if p, err := ProviderByName("azure"); err != nil || p.IstioNamespace != "aks-istio-system" {
		t.Fatalf("azure: %+v, %v", p, err)
	}
	if _, err := ProviderByName("nope"); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
