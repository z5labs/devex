// Kubernetes provides Dagger functions for spinning up Kubernetes
// clusters using upstream Kind (`kindest/node`) as privileged Dagger
// Services, and a pure-Go client (built on `k8s.io/client-go`) that
// targets either the local cluster or any reachable remote cluster
// via its kubeconfig.
//
// Scope (this story): plaintext-only, single control-plane, arbitrary
// workers. TLS-customisation, OIDC, custom CA injection, ingress TLS,
// and HA control-plane (HAProxy LB in front of N kube-apiservers) all
// land in follow-up stories.
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - cluster.go    — *Cluster + Kubernetes.KindCluster, input
//                     validation, kindest/node service plumbing,
//                     wrapper-script PID 1 that runs kubeadm init/join.
//   - kubeconfig.go — admin.conf extraction + server-URL rewrite so
//                     the kubeconfig is reachable from any container
//                     in the same Dagger session via the per-cluster
//                     WithHostname alias.
//   - client.go     — *Client + Kubernetes.Client, client-go wiring,
//                     and the Apply/Get/Delete/List/WaitForReady
//                     method set.
package main

// Kubernetes is the root namespace for every exported function in
// this module. All cluster constructors and the remote-client factory
// hang off *Kubernetes so the generated Dagger SDK surfaces them
// under `dag.Kubernetes().<Func>(...)`.
type Kubernetes struct{}
