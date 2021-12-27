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
	"time"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/utils/injectabletime"
	"github.com/aws/karpenter/pkg/utils/node"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const LivenessTimeout = 15 * time.Minute

// Liveness is a subreconciler that deletes nodes determined to be unrecoverable
type Liveness struct {
	kubeClient client.Client
}

// Reconcile reconciles the node
func (r *Liveness) Reconcile(ctx context.Context, _ *v1alpha5.Provisioner, n *v1.Node) (reconcile.Result, error) {
	if timeSinceCreation := injectabletime.Now().Sub(n.GetCreationTimestamp().Time); timeSinceCreation < LivenessTimeout {
		return reconcile.Result{RequeueAfter: LivenessTimeout - timeSinceCreation}, nil
	}
	condition := node.GetCondition(n.Status.Conditions, v1.NodeReady)
	// If the reason is "", then the condition has never been set. We expect
	// either the kubelet to set this reason, or the kcm's
	// node-lifecycle-controller to set the status to NodeStatusNeverUpdated if
	// the kubelet cannot connect. Once the value is NodeStatusNeverUpdated and
	// the node is beyond the liveness timeout, we will delete the node.
	if condition.Reason != "" && condition.Reason != "NodeStatusNeverUpdated" {
		return reconcile.Result{}, nil
	}
	logging.FromContext(ctx).Infof("Triggering termination for node that failed to join")
	if err := r.kubeClient.Delete(ctx, n); err != nil {
		return reconcile.Result{}, fmt.Errorf("deleting node, %w", err)
	}
	return reconcile.Result{}, nil
}
