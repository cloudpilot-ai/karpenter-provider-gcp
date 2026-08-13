/*
Copyright 2025 The CloudPilot AI Authors.

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

package events

import (
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
)

// PreemptionNoticeReceived records that GCE signaled an imminent Spot preemption
// for a node, ahead of the shutdown signal. The event is attached to the Node
// rather than the NodeClaim so it survives in `kubectl describe node` while the
// NodeClaim is being torn down.
func PreemptionNoticeReceived(node *corev1.Node) (evts []events.Event) {
	evts = append(evts, events.Event{
		InvolvedObject: node,
		Type:           corev1.EventTypeWarning,
		Reason:         "PreemptionNoticeReceived",
		Message:        "GCE signaled an imminent Spot preemption for the Node",
		DedupeValues:   []string{string(node.UID)},
	})
	return evts
}

func TerminatingOnInterruption(nodeClaim *karpv1.NodeClaim) (evts []events.Event) {
	evts = append(evts, events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeWarning,
		Reason:         "TerminatingOnInterruption",
		Message:        "Interruption triggered termination for the NodeClaim",
		DedupeValues:   []string{string(nodeClaim.UID)},
	})
	return evts
}
