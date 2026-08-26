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
	"testing"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

// newTestPDBConsultant builds a consultant whose PDB lister is seeded from the
// fake informer store.
func newTestPDBConsultant(t *testing.T, pdbs ...*policyv1.PodDisruptionBudget) *sessionPDBConsultant {
	t.Helper()
	client := kubefake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	informer := informerFactory.Policy().V1().PodDisruptionBudgets().Informer()
	for _, pdb := range pdbs {
		if err := informer.GetStore().Add(pdb); err != nil {
			t.Fatalf("seed pdb informer: %v", err)
		}
	}
	return &sessionPDBConsultant{pdbLister: informerFactory.Policy().V1().PodDisruptionBudgets().Lister()}
}

func podLikeTask(name, namespace string, labels map[string]string) *schedapi.TaskInfo {
	return &schedapi.TaskInfo{
		Name:      name,
		Namespace: namespace,
		Job:       schedapi.JobID(namespace + "/pg"),
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		},
	}
}

func TestPDBConsultantVetoesZeroAllowance(t *testing.T) {
	c := newTestPDBConsultant(t, &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "pdb", Namespace: "ns"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
		},
		Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
	})
	evictable, ok := c.Evictable(podLikeTask("pod", "ns", map[string]string{"app": "x"}))
	if !ok {
		t.Fatal("a matched PDB must be reported (ok=true)")
	}
	if evictable {
		t.Fatal("a matched PDB with zero allowance must veto eviction")
	}
}

func TestPDBConsultantAllowsPositiveAllowance(t *testing.T) {
	c := newTestPDBConsultant(t, &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "pdb", Namespace: "ns"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
		},
		Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 3},
	})
	evictable, ok := c.Evictable(podLikeTask("pod", "ns", map[string]string{"app": "x"}))
	if !ok || !evictable {
		t.Fatalf("a matched PDB with allowance must allow eviction (evictable=%v ok=%v)", evictable, ok)
	}
}

func TestPDBConsultantIgnoresNonMatchingLabels(t *testing.T) {
	c := newTestPDBConsultant(t, &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "pdb", Namespace: "ns"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}},
		},
		Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
	})
	evictable, ok := c.Evictable(podLikeTask("pod", "ns", map[string]string{"app": "x"}))
	if ok {
		t.Fatal("a non-matching PDB must not produce a PDB opinion (ok=false)")
	}
	if !evictable {
		t.Fatal("a non-matching PDB must not veto")
	}
}

func TestPDBConsultantIgnoresDisruptedPod(t *testing.T) {
	// A Pod already recorded in DisruptedPods has consumed its allowance and is
	// not vetoed (mirrors the scheduler pdb plugin).
	c := newTestPDBConsultant(t, &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "pdb", Namespace: "ns"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
		},
		Status: policyv1.PodDisruptionBudgetStatus{
			DisruptionsAllowed: 0,
			DisruptedPods:      map[string]metav1.Time{"pod": {}},
		},
	})
	evictable, ok := c.Evictable(podLikeTask("pod", "ns", map[string]string{"app": "x"}))
	if !ok || !evictable {
		t.Fatalf("an already-disrupted Pod must not be vetoed (evictable=%v ok=%v)", evictable, ok)
	}
}

func TestPDBConsultantNoLabelsNoPDB(t *testing.T) {
	c := newTestPDBConsultant(t, &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "pdb", Namespace: "ns"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
		},
		Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
	})
	// A Pod with no labels cannot match any PDB → no opinion, never vetoed.
	evictable, ok := c.Evictable(podLikeTask("pod", "ns", nil))
	if ok {
		t.Fatal("a label-less Pod must not match any PDB (ok=false)")
	}
	if !evictable {
		t.Fatal("a label-less Pod must not be vetoed")
	}
}
