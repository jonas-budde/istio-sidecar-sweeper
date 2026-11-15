package sweeper

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Expected is the proxy image an injection would use right now for a revision.
type Expected struct {
	Revision string
	Image    string
	Tag      string
}

type injectorValues struct {
	Global struct {
		Hub     string `json:"hub"`
		Tag     string `json:"tag"`
		Variant string `json:"variant"`
		Proxy   struct {
			Image string `json:"image"`
		} `json:"proxy"`
	} `json:"global"`
}

// ParseInjectorValues reads the "values" key of an istio-sidecar-injector ConfigMap.
func ParseInjectorValues(rev, values string) (Expected, error) {
	var v injectorValues
	if err := json.Unmarshal([]byte(values), &v); err != nil {
		return Expected{}, fmt.Errorf("parse injector values: %w", err)
	}
	if v.Global.Tag == "" {
		return Expected{}, fmt.Errorf("injector values for revision %q have no global.tag", rev)
	}
	// AKS sets global.proxy.image to a full reference such as .../proxyv2:v1.29.10-1.
	if img := v.Global.Proxy.Image; ImageTag(img) != "" {
		return Expected{Revision: rev, Image: img, Tag: ImageTag(img)}, nil
	}
	name := v.Global.Proxy.Image
	if name == "" {
		name = "proxyv2"
	}
	tag := v.Global.Tag
	if v.Global.Variant != "" {
		tag += "-" + v.Global.Variant
	}
	return Expected{
		Revision: rev,
		Image:    strings.TrimRight(v.Global.Hub, "/") + "/" + name + ":" + tag,
		Tag:      tag,
	}, nil
}

// LoadExpected returns the expected proxy image for a revision ("default" for the unrevisioned injector).
func LoadExpected(ctx context.Context, cs kubernetes.Interface, istioNS, rev string) (Expected, error) {
	name := "istio-sidecar-injector"
	if rev != "" && rev != "default" {
		name += "-" + rev
	}
	cm, err := cs.CoreV1().ConfigMaps(istioNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Expected{}, fmt.Errorf("get configmap %s/%s: %w", istioNS, name, err)
	}
	return ParseInjectorValues(rev, cm.Data["values"])
}
