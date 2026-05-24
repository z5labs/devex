package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dagger/kubernetes/internal/dagger"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	sigyaml "sigs.k8s.io/yaml"
)

// Client is a client-go-backed Kubernetes client. Each method opens a
// fresh REST + dynamic client per call so the function is stateless
// from Dagger's perspective.
type Client struct {
	// +private
	KubeconfigFile *dagger.File
}

// Client constructs a client-go-backed Kubernetes client that targets
// the cluster described by the supplied kubeconfig. No I/O happens at
// construction time. Works against a local KindCluster or any
// reachable remote cluster — the kubeconfig is the only handle the
// client needs.
//
// +cache="session"
func (k *Kubernetes) Client(kubeconfig *dagger.File) *Client {
	return &Client{KubeconfigFile: kubeconfig}
}

// Client returns a client-go Client that targets this cluster. Starts
// the control-plane service if it isn't already running so the client's
// first API call doesn't race with cluster readiness.
//
// +cache="never"
func (c *Cluster) Client(ctx context.Context) (*Client, error) {
	kc, err := c.Kubeconfig(ctx)
	if err != nil {
		return nil, err
	}
	return &Client{KubeconfigFile: kc}, nil
}

// Kubeconfig returns the kubeconfig file this client is configured to use.
//
// +cache="never"
func (c *Client) Kubeconfig() *dagger.File {
	return c.KubeconfigFile
}

// Apply applies every YAML document in the manifest file to the
// cluster via server-side apply. Multi-document YAML is supported —
// the file is split on `---` separators and each document is decoded
// into an `unstructured.Unstructured`, mapped to its GVR via discovery,
// then applied with field manager `devex-kubernetes-module`.
//
// Malformed manifest YAML surfaces a wrapped decode error.
//
// +cache="never"
func (c *Client) Apply(ctx context.Context, manifest *dagger.File) error {
	dyn, mapper, err := c.load(ctx)
	if err != nil {
		return err
	}
	docs, err := decodeManifestDocs(ctx, manifest)
	if err != nil {
		return err
	}
	for _, obj := range docs {
		mapping, err := restMappingFor(mapper, obj)
		if err != nil {
			return err
		}
		ri := dynamicResource(dyn, mapping, obj)
		if _, err := ri.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{
			FieldManager: "devex-kubernetes-module",
			Force:        true,
		}); err != nil {
			return fmt.Errorf("apply %s/%s: %w",
				obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

// Delete deletes every resource named by every YAML document in the
// manifest file. Missing resources (404) are treated as success — the
// post-condition of Delete is "this resource does not exist".
//
// +cache="never"
func (c *Client) Delete(ctx context.Context, manifest *dagger.File) error {
	dyn, mapper, err := c.load(ctx)
	if err != nil {
		return err
	}
	docs, err := decodeManifestDocs(ctx, manifest)
	if err != nil {
		return err
	}
	for _, obj := range docs {
		mapping, err := restMappingFor(mapper, obj)
		if err != nil {
			return err
		}
		ri := dynamicResource(dyn, mapping, obj)
		if err := ri.Delete(ctx, obj.GetName(), metav1.DeleteOptions{}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("delete %s/%s: %w",
				obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

// Get returns the named resource as YAML. `resource` accepts the
// kubectl-style GVR shorthand (`"namespaces"`, `"pods"`,
// `"deployments.apps"`); the discovery RESTMapper resolves it to the
// preferred GVR. Empty `namespace` is meaningful only for cluster-scoped
// resources; namespaced resources require a non-empty namespace.
//
// Unknown resource shorthand returns an explicit "unknown resource"
// error.
//
// +cache="never"
func (c *Client) Get(ctx context.Context, resource string,
	// +default=""
	namespace string,
	name string,
) (string, error) {
	dyn, mapper, err := c.load(ctx)
	if err != nil {
		return "", err
	}
	gvr, namespaced, err := resolveGVR(mapper, resource)
	if err != nil {
		return "", err
	}
	ri := namespacedResource(dyn, gvr, namespaced, namespace)
	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get %s/%s: %w", resource, name, err)
	}
	out, err := sigyaml.Marshal(obj.Object)
	if err != nil {
		return "", fmt.Errorf("marshal yaml: %w", err)
	}
	return string(out), nil
}

// List returns the metadata.name of every resource of the given
// shorthand. Empty `namespace` lists across all namespaces for
// namespaced resources, or returns every cluster-scoped resource for
// cluster-scoped shorthands like `nodes` or `namespaces`.
//
// +cache="never"
func (c *Client) List(ctx context.Context, resource string,
	// +default=""
	namespace string,
) ([]string, error) {
	dyn, mapper, err := c.load(ctx)
	if err != nil {
		return nil, err
	}
	gvr, namespaced, err := resolveGVR(mapper, resource)
	if err != nil {
		return nil, err
	}
	ri := namespacedResource(dyn, gvr, namespaced, namespace)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", resource, err)
	}
	out := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, item.GetName())
	}
	return out, nil
}

// WaitForReady blocks until the named resource reports a condition
// indicating readiness, or `timeout` elapses. For Deployments the
// `Available: True` condition counts; for everything else the `Ready`
// condition counts. timeout is parsed via time.ParseDuration.
//
// +cache="never"
func (c *Client) WaitForReady(ctx context.Context, resource string,
	// +default=""
	namespace string,
	name string,
	// +default="60s"
	timeout string,
) error {
	dur, err := time.ParseDuration(timeout)
	if err != nil {
		return fmt.Errorf("parse timeout %q: %w", timeout, err)
	}
	dyn, mapper, err := c.load(ctx)
	if err != nil {
		return err
	}
	gvr, namespaced, err := resolveGVR(mapper, resource)
	if err != nil {
		return err
	}
	ri := namespacedResource(dyn, gvr, namespaced, namespace)
	conditionType := "Ready"
	if gvr.Group == "apps" && gvr.Resource == "deployments" {
		conditionType = "Available"
	}
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, dur, true,
		func(pollCtx context.Context) (bool, error) {
			obj, err := ri.Get(pollCtx, name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			return hasCondition(obj, conditionType, string(corev1.ConditionTrue)), nil
		})
}

// load returns a dynamic client + RESTMapper backed by a fresh REST
// config loaded from the client's *dagger.File. Each call opens fresh
// connections so the method is stateless.
func (c *Client) load(ctx context.Context) (dynamic.Interface, meta.RESTMapper, error) {
	if c.KubeconfigFile == nil {
		return nil, nil, fmt.Errorf("client has no kubeconfig configured")
	}
	contents, err := c.KubeconfigFile.Contents(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(contents))
	if err != nil {
		return nil, nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	// Tighten timeouts: client-go's default is 0 (no timeout), which
	// would let a misconfigured kubeconfig hang every method
	// indefinitely. 30s is well above any well-formed Kubernetes API
	// round-trip but well below Dagger's session timeout.
	cfg.Timeout = 30 * time.Second
	return buildClients(cfg)
}

// buildClients constructs the dynamic client + RESTMapper used by
// every method. Split out from load() so future TLS / mTLS variants
// can override the *rest.Config and reuse this plumbing.
func buildClients(cfg *rest.Config) (dynamic.Interface, meta.RESTMapper, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build dynamic client: %w", err)
	}
	return dyn, mapper, nil
}

// decodeManifestDocs reads the manifest file, splits multi-doc YAML on
// the standard `---` separator, and decodes each document into an
// Unstructured. Malformed YAML surfaces a wrapped decode error.
func decodeManifestDocs(ctx context.Context, manifest *dagger.File) ([]*unstructured.Unstructured, error) {
	if manifest == nil {
		return nil, fmt.Errorf("manifest is nil")
	}
	raw, err := manifest.Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(raw), 4096)
	var out []*unstructured.Unstructured
	for {
		var u unstructured.Unstructured
		if err := dec.Decode(&u); err != nil {
			if err.Error() == "EOF" || strings.HasSuffix(err.Error(), "EOF") {
				break
			}
			return nil, fmt.Errorf("decode manifest yaml: %w", err)
		}
		if len(u.Object) == 0 {
			continue
		}
		if u.GetKind() == "" {
			return nil, fmt.Errorf("manifest document missing 'kind'")
		}
		out = append(out, &u)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("manifest decoded to zero documents")
	}
	return out, nil
}

// resolveGVR turns a kubectl-style shorthand (`namespaces`, `pods`,
// `deployments.apps`) into a `schema.GroupVersionResource` + a flag
// indicating whether the resource is namespace-scoped. The mapper
// consults discovery to pick the preferred version.
func resolveGVR(mapper meta.RESTMapper, resource string) (schema.GroupVersionResource, bool, error) {
	gr := schema.ParseGroupResource(resource)
	mapping, err := mapper.RESTMapping(schema.GroupKind{
		Group: gr.Group,
		Kind:  "", // empty Kind triggers the resource-based path below
	})
	// If the kind-based mapping doesn't fit (empty Kind), fall back to
	// resource-based discovery.
	if err != nil || mapping == nil {
		gvr, err := mapper.ResourceFor(gr.WithVersion(""))
		if err != nil {
			return schema.GroupVersionResource{}, false, fmt.Errorf("unknown resource %q: %w", resource, err)
		}
		m, err := mapper.RESTMapping(schema.GroupKind{Group: gvr.Group}, gvr.Version)
		if err == nil {
			return gvr, m.Scope.Name() == meta.RESTScopeNameNamespace, nil
		}
		// Fall back to assuming namespace-scoped if we can't determine.
		return gvr, true, nil
	}
	return mapping.Resource, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// restMappingFor returns the REST mapping for an arbitrary
// Unstructured object — used by Apply/Delete which decode the GVK from
// the manifest itself.
func restMappingFor(mapper meta.RESTMapper, obj *unstructured.Unstructured) (*meta.RESTMapping, error) {
	gvk := obj.GroupVersionKind()
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("rest mapping for %s: %w", gvk, err)
	}
	return mapping, nil
}

// dynamicResource returns a NamespaceableResourceInterface ready for
// Apply/Delete, scoped to the object's namespace when relevant.
func dynamicResource(dyn dynamic.Interface, mapping *meta.RESTMapping, obj *unstructured.Unstructured) dynamic.ResourceInterface {
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = metav1.NamespaceDefault
		}
		return dyn.Resource(mapping.Resource).Namespace(ns)
	}
	return dyn.Resource(mapping.Resource)
}

// namespacedResource returns a ResourceInterface for Get/List/WaitForReady.
// Empty namespace on a namespaced resource means "all namespaces".
func namespacedResource(dyn dynamic.Interface, gvr schema.GroupVersionResource, namespaced bool, namespace string) dynamic.ResourceInterface {
	if !namespaced {
		return dyn.Resource(gvr)
	}
	if namespace == "" {
		return dyn.Resource(gvr).Namespace(metav1.NamespaceAll)
	}
	return dyn.Resource(gvr).Namespace(namespace)
}

// hasCondition checks whether the object's
// status.conditions contains an entry with the given type set to the
// given status.
func hasCondition(obj *unstructured.Unstructured, condType, condStatus string) bool {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		t, _ := cm["type"].(string)
		s, _ := cm["status"].(string)
		if t == condType && s == condStatus {
			return true
		}
	}
	return false
}
