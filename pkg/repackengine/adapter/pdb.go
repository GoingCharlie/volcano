/*
Copyright 2026 The Volcano Authors.

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

package adapter

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	policylisters "k8s.io/client-go/listers/policy/v1"
	"k8s.io/klog/v2"

	repackapi "volcano.sh/volcano/pkg/repackengine/api"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
)

// sessionPDBConsultant implements api.PDBConsultant against the scheduler
// session's informer factory, reading the disruption controller's computed
// PodDisruptionBudget status. The allowance arithmetic mirrors the scheduler's
// own pdb plugin (pkg/scheduler/plugins/pdb): for a task's Pod it finds every
// PDB whose selector matches the Pod's labels in the same namespace, skips
// Pods already recorded in DisruptedPods, and decrements each matching PDB's
// DisruptionsAllowed; the task is evictable only while every matching PDB
// still has allowance.
type sessionPDBConsultant struct {
	pdbLister policylisters.PodDisruptionBudgetLister
}

// NewSessionPDBConsultant builds a PDBConsultant from a live scheduler session.
// It returns nil when the session carries no informer factory (e.g. a fake
// session in tests), in which case callers keep the noop consultant.
func NewSessionPDBConsultant(ssn *schedframework.Session) repackapi.PDBConsultant {
	if ssn == nil || ssn.InformerFactory() == nil {
		return nil
	}
	return &sessionPDBConsultant{
		pdbLister: ssn.InformerFactory().Policy().V1().PodDisruptionBudgets().Lister(),
	}
}

// Evictable reports whether task may currently be disrupted. ok=false means no
// effective PDB matched (never vetoes).
func (c *sessionPDBConsultant) Evictable(task *schedapi.TaskInfo) (bool, bool) {
	if c == nil || task == nil || task.Pod == nil || c.pdbLister == nil {
		return false, false
	}
	pod := task.Pod
	// A Pod with no labels cannot match any PDB.
	if len(pod.Labels) == 0 {
		return true, false
	}
	pdbs, err := c.pdbLister.PodDisruptionBudgets(pod.Namespace).List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "repack: list PodDisruptionBudgets for planning-time PDB check",
			"namespace", pod.Namespace, "pod", pod.Name)
		return false, false
	}
	matchedAny := false
	for _, pdb := range pdbs {
		if pdb.Namespace != pod.Namespace {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil || selector.Empty() {
			continue
		}
		if !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		matchedAny = true
		// Already disrupted by the API server: it has already consumed allowance.
		if _, exist := pdb.Status.DisruptedPods[pod.Name]; exist {
			continue
		}
		if pdb.Status.DisruptionsAllowed <= 0 {
			return false, true // a matched PDB with no allowance vetoes
		}
	}
	if !matchedAny {
		return true, false // no PDB matched → no PDB opinion
	}
	return true, true // every matched PDB still has allowance
}

var _ repackapi.PDBConsultant = (*sessionPDBConsultant)(nil)