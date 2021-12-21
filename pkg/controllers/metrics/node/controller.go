/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package node

import (
	"context"
	"fmt"
	"strings"

	"knative.dev/pkg/logging"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	podutil "github.com/aws/karpenter/pkg/utils/pod"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	resourceType     = "resource_type"
	nodeName         = "node_name"
	nodeProvisioner  = "provisioner"
	nodeZone         = "zone"
	nodeArchitecture = "arch"
	nodeCapacityType = "capacity_type"
	nodeInstanceType = "instance_type"
	nodePhase        = "phase"
)

var (
	allocatableGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "allocatable",
			Help:      "Node allocatable",
		},
		labelNames(),
	)
	podRequestsGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "total_pod_requests",
			Help:      "Node total pod requests",
		},
		labelNames(),
	)
	podLimitsGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "total_pod_limits",
			Help:      "Node total pod limits",
		},
		labelNames(),
	)
	daemonRequestsGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "total_daemon_requests",
			Help:      "Node total daemon requests",
		},
		labelNames(),
	)
	daemonLimitsGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "total_daemon_limits",
			Help:      "Node total daemon limits",
		},
		labelNames(),
	)
	overheadGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "system_overhead",
			Help:      "Node system daemon overhead",
		},
		labelNames(),
	)
)

func init() {
	crmetrics.Registry.MustRegister(allocatableGaugeVec)
	crmetrics.Registry.MustRegister(podRequestsGaugeVec)
	crmetrics.Registry.MustRegister(podLimitsGaugeVec)
	crmetrics.Registry.MustRegister(daemonRequestsGaugeVec)
	crmetrics.Registry.MustRegister(daemonLimitsGaugeVec)
	crmetrics.Registry.MustRegister(overheadGaugeVec)
}

func labelNames() []string {
	return []string{
		resourceType,
		nodeName,
		nodeProvisioner,
		nodeZone,
		nodeArchitecture,
		nodeCapacityType,
		nodeInstanceType,
		nodePhase,
	}
}

type Controller struct {
	KubeClient    client.Client
	LabelSliceMap map[types.NamespacedName][]prometheus.Labels
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		KubeClient:    kubeClient,
		LabelSliceMap: make(map[types.NamespacedName][]prometheus.Labels),
	}
}

// Reconcile executes a termination control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named("nodemetrics").With("node", req.Name))
	// Remove the previous gauge after node labels are updated
	c.deleteGauges(req.NamespacedName)
	// Retrieve node from reconcile request
	node := &v1.Node{}
	if err := c.KubeClient.Get(ctx, req.NamespacedName, node); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	if err := c.updateGauges(ctx, node); err != nil {
		logging.FromContext(ctx).Errorf("Failed to update gauges: %s", err.Error())
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	return controllerruntime.
		NewControllerManagedBy(m).
		Named("nodemetrics").
		For(&v1.Node{}).
		Watches(
			// Reconcile all nodes related to a provisioner when it changes.
			&source.Kind{Type: &v1alpha5.Provisioner{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) (requests []reconcile.Request) {
				nodes := &v1.NodeList{}
				if err := c.KubeClient.List(ctx, nodes, client.MatchingLabels(map[string]string{v1alpha5.ProvisionerNameLabelKey: o.GetName()})); err != nil {
					logging.FromContext(ctx).Errorf("Failed to list nodes when mapping expiration watch events, %s", err.Error())
					return requests
				}
				for _, node := range nodes.Items {
					requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
				}
				return requests
			}),
		).
		Watches(
			// Reconcile nodes where pods have changed
			&source.Kind{Type: &v1.Pod{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) (requests []reconcile.Request) {
				if name := o.(*v1.Pod).Spec.NodeName; name != "" {
					requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
				}
				return requests
			}),
		).
		Complete(c)
}

func (c *Controller) deleteGauges(nodeNamespacedName types.NamespacedName) {
	if labelSlice, ok := c.LabelSliceMap[nodeNamespacedName]; ok {
		for _, labels := range labelSlice {
			allocatableGaugeVec.Delete(labels)
			podRequestsGaugeVec.Delete(labels)
			podLimitsGaugeVec.Delete(labels)
			daemonRequestsGaugeVec.Delete(labels)
			daemonLimitsGaugeVec.Delete(labels)
			overheadGaugeVec.Delete(labels)
		}
	}
	c.LabelSliceMap[nodeNamespacedName] = []prometheus.Labels{}

}

// generateLabels creates the labels using the current state of the pod
func (c *Controller) generateLabels(node *v1.Node, resourceTypeName string) prometheus.Labels {
	metricLabels := prometheus.Labels{}
	metricLabels[resourceType] = resourceTypeName
	metricLabels[nodeName] = node.GetName()
	if provisionerName, ok := node.Labels[v1alpha5.ProvisionerNameLabelKey]; !ok {
		metricLabels[nodeProvisioner] = "N/A"
	} else {
		metricLabels[nodeProvisioner] = provisionerName
	}
	metricLabels[nodeZone] = node.Labels[v1.LabelTopologyZone]
	metricLabels[nodeArchitecture] = node.Labels[v1.LabelArchStable]
	if capacityType, ok := node.Labels[v1alpha5.LabelCapacityType]; !ok {
		metricLabels[nodeCapacityType] = "N/A"
	} else {
		metricLabels[nodeCapacityType] = capacityType
	}
	metricLabels[nodeInstanceType] = node.Labels[v1.LabelInstanceTypeStable]
	metricLabels[nodePhase] = string(node.Status.Phase)
	return metricLabels
}

func (c *Controller) updateGauges(ctx context.Context, node *v1.Node) error {
	podlist := &v1.PodList{}
	if err := c.KubeClient.List(ctx, podlist, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return fmt.Errorf("listing pods on node %s, %w", node.Name, err)
	}
	var daemonSetPods, pods []*v1.Pod
	for index := range podlist.Items {
		if podutil.IsOwnedByDaemonSet(&podlist.Items[index]) {
			daemonSetPods = append(daemonSetPods, &podlist.Items[index])
		} else {
			pods = append(pods, &podlist.Items[index])
		}
	}
	podRequest, podLimits := getPodsTotalRequestsAndLimits(pods)
	daemonRequest, daemonLimits := getPodsTotalRequestsAndLimits(daemonSetPods)
	systemOverhead := getSystemOverhead(node)
	allocatable := node.Status.Capacity
	if len(node.Status.Allocatable) > 0 {
		allocatable = node.Status.Allocatable
	}
	// Populate  metrics
	for gaugeVec, resourceList := range map[*prometheus.GaugeVec]v1.ResourceList{
		overheadGaugeVec:       systemOverhead,
		podRequestsGaugeVec:    podRequest,
		podLimitsGaugeVec:      podLimits,
		daemonRequestsGaugeVec: daemonRequest,
		daemonLimitsGaugeVec:   daemonLimits,
		allocatableGaugeVec:    allocatable,
	} {
		if err := c.insertGaugeValues(resourceList, node, gaugeVec); err != nil {
			logging.FromContext(ctx).Errorf("Failed to generate gauge: %w", err)
		}
	}
	return nil
}

func getSystemOverhead(node *v1.Node) v1.ResourceList {
	systemOverheads := v1.ResourceList{}
	if len(node.Status.Allocatable) > 0 {
		// calculating system daemons overhead
		for resourceName, quantity := range node.Status.Allocatable {
			overhead := node.Status.Capacity[resourceName]
			overhead.Sub(quantity)
			systemOverheads[resourceName] = overhead
		}
	}
	return systemOverheads
}

func (c *Controller) insertGaugeValues(resourceList v1.ResourceList, node *v1.Node, gaugeVec *prometheus.GaugeVec) error {

	for resourceName, quantity := range resourceList {
		resourceTypeName := strings.ReplaceAll(strings.ToLower(string(resourceName)), "-", "_")
		labels := c.generateLabels(node, resourceTypeName)
		// Register the set of labels that are generated for node
		nodeNamespacedName := types.NamespacedName{Name: node.Name}
		c.LabelSliceMap[nodeNamespacedName] = append(c.LabelSliceMap[nodeNamespacedName], labels)
		gauge, err := gaugeVec.GetMetricWith(labels)
		if err != nil {
			return fmt.Errorf("generate new gauge: %w", err)
		}
		if resourceName == v1.ResourceCPU {
			gauge.Set(float64(quantity.MilliValue()) / float64(1000))
		} else {
			gauge.Set(float64(quantity.Value()))
		}
	}
	return nil
}

// GetPodsTotalRequestsAndLimits calculates the total resource requests and limits for the pods.
// If pod overhead is non-nil, the pod overhead is added to the
// total container resource requests and to the total container limits which have a non-zero quantity.
func getPodsTotalRequestsAndLimits(pods []*v1.Pod) (reqs v1.ResourceList, limits v1.ResourceList) {
	reqs, limits = v1.ResourceList{}, v1.ResourceList{}
	for _, pod := range pods {
		// Excluding pods that are completed or failed
		if podutil.IsTerminal(pod) {
			continue
		}
		for _, container := range pod.Spec.Containers {
			// Calculate Resource Requests
			addResourceQuantity(container.Resources.Requests, reqs)
			// Calculate Resource Limits
			addResourceQuantity(container.Resources.Limits, limits)
		}
		// Add overhead for running a pod to the sum of requests and to non-zero limits:
		if pod.Spec.Overhead != nil {
			// Calculate Resource Requests
			addResourceQuantity(pod.Spec.Overhead, reqs)
			// Calculate Resource Requests
			// Add to limits only when non-zero
			for resourceName, quantity := range pod.Spec.Overhead {
				if value, ok := limits[resourceName]; ok && !value.IsZero() {
					value.Add(quantity)
					limits[resourceName] = value
				}
			}
		}
	}
	return
}

func addResourceQuantity(valueResourceList v1.ResourceList, targetResourceList v1.ResourceList) {
	for resourceName, quantity := range valueResourceList {
		if value, ok := targetResourceList[resourceName]; !ok {
			targetResourceList[resourceName] = quantity.DeepCopy()
		} else {
			value.Add(quantity)
			targetResourceList[resourceName] = value
		}
	}

}
