package scheduler

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Standard in-cluster service-account mount.
const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// GPUResource is the extended resource name the NVIDIA device plugin uses.
const GPUResource = "nvidia.com/gpu"

// KubernetesSource reads the collector's own node and the pods on it from the
// API server, using the pod's service account. Needs RBAC: get nodes, list pods.
type KubernetesSource struct {
	nodeName  string
	baseURL   string
	tokenFile string
	client    *http.Client
}

// NewKubernetes builds a client from the in-cluster environment.
func NewKubernetes(nodeName string) (*KubernetesSource, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a Kubernetes pod (KUBERNETES_SERVICE_HOST unset)")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("service account CA: no certificates found")
	}
	return &KubernetesSource{
		nodeName:  nodeName,
		baseURL:   "https://" + net.JoinHostPort(host, port),
		tokenFile: saDir + "/token",
		client: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

func (k *KubernetesSource) Kind() string { return Kubernetes }

// Minimal views of the API objects; only the fields we read.
type k8sNode struct {
	Status struct {
		Allocatable map[string]string `json:"allocatable"`
		NodeInfo    struct {
			OSImage string `json:"osImage"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

type k8sContainer struct {
	Name      string `json:"name"`
	Resources struct {
		Limits   map[string]string `json:"limits"`
		Requests map[string]string `json:"requests"`
	} `json:"resources"`
}

type k8sPodList struct {
	Items []struct {
		Metadata struct {
			UID       string `json:"uid"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Containers     []k8sContainer `json:"containers"`
			InitContainers []k8sContainer `json:"initContainers"`
		} `json:"spec"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Name        string `json:"name"`
				ContainerID string `json:"containerID"` // "containerd://<id>"
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// Query implements Source.
func (k *KubernetesSource) Query(ctx context.Context) (Snapshot, error) {
	snap := Snapshot{Workloads: map[string]Workload{}}

	var node k8sNode
	if err := k.get(ctx, "/api/v1/nodes/"+url.PathEscape(k.nodeName), &node); err != nil {
		return snap, err
	}
	snap.NodeOS = node.Status.NodeInfo.OSImage
	if v, ok := node.Status.Allocatable[GPUResource]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			snap.Node.GPUsAllocatable = &n
		}
	}

	var pods k8sPodList
	q := url.Values{"fieldSelector": {"spec.nodeName=" + k.nodeName}}
	if err := k.get(ctx, "/api/v1/pods?"+q.Encode(), &pods); err != nil {
		return snap, err
	}
	allocated := 0
	for _, p := range pods.Items {
		w := Workload{Name: p.Metadata.Name, Namespace: p.Metadata.Namespace, Containers: map[string]string{}}
		for _, cs := range p.Status.ContainerStatuses {
			if _, id, ok := strings.Cut(cs.ContainerID, "://"); ok {
				w.Containers[id] = cs.Name
			}
		}
		snap.Workloads[p.Metadata.UID] = w
		if p.Status.Phase != "Succeeded" && p.Status.Phase != "Failed" {
			allocated += podGPUs(p.Spec.Containers, p.Spec.InitContainers)
		}
	}
	snap.Node.GPUsAllocated = &allocated
	return snap, nil
}

// podGPUs is the pod's effective GPU request: the larger of the sum over app
// containers and the largest init container (they run before, not alongside).
func podGPUs(containers, inits []k8sContainer) int {
	gpus := func(c k8sContainer) int {
		v, ok := c.Resources.Limits[GPUResource]
		if !ok {
			v = c.Resources.Requests[GPUResource]
		}
		n, _ := strconv.Atoi(v)
		return n
	}
	sum, maxInit := 0, 0
	for _, c := range containers {
		sum += gpus(c)
	}
	for _, c := range inits {
		maxInit = max(maxInit, gpus(c))
	}
	return max(sum, maxInit)
}

func (k *KubernetesSource) get(ctx context.Context, path string, into any) error {
	// Bound service-account tokens rotate: read the file on every request.
	token, err := os.ReadFile(k.tokenFile)
	if err != nil {
		return fmt.Errorf("read service account token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("kubernetes API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		hint := ""
		if resp.StatusCode == http.StatusForbidden {
			hint = " (the collector's service account needs get on nodes and list on pods)"
		}
		return fmt.Errorf("kubernetes API GET %s: %s%s: %s", path, resp.Status, hint, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("kubernetes API GET %s: decode: %w", path, err)
	}
	return nil
}
