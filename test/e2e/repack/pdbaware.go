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

// These e2e cases exercise the pdbaware plugin, which vetoes planning victims
// whose PodGroup's PDB leaves no disruption allowance at PLAN time — a
// PDB-blocked Pod is never planned as a victim, so it is never evicted. The
// Eviction API remains the final arbiter at Execute time; this suite proves the
// planning-time filter itself by inspecting the produced plan.
package repack

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

var _ = Describe("Repack pdbaware planning-time PDB filtering", Serial, func() {
	var ctx *e2eutil.TestContext
	var nodes []string

	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = npuFixture(ctx, 3)
	})
	AfterEach(func() {
		recordSpecFailureDiagnostics(ctx)
		e2eutil.CleanupTestContext(ctx)
		for _, n := range nodes {
			clearNPU(ctx, n)
		}
	})

	// PDB-1: one PDB-blocked workload and two open workloads share a fragmented
	// cluster. The DryRun plan must include the open workloads and completely
	// exclude the PDB-blocked one — proving the pdbaware planning-time filter
	// (a PDB-blocked Pod never enters a plan, so it can never be evicted).
	It("excludes a PDB-blocked workload from the plan while planning the open ones", func() {
		openA := occupyNativeDeployment(ctx, "pa-open-a", nodes[0], "move", 4)
		openB := occupyNativeDeployment(ctx, "pa-open-b", nodes[1], "move", 2)
		protected := occupyNativeDeployment(ctx, "pa-protected", nodes[2], "move", 2)
		defer deleteNativeWorkloads(ctx, openA, openB, protected)

		// A maxUnavailable=0 PDB fully blocks the protected workload: the
		// disruption controller computes DisruptionsAllowed=0, which pdbaware
		// must honor at planning time.
		blockAll := intstr.FromInt(0)
		_, err := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Create(context.TODO(),
			&policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "pa-block"},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MaxUnavailable: &blockAll,
					Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{nativeWorkloadLabel: protected.deployment.Name}},
				},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		waitPDBAllowance(ctx, "pa-block", 0)

		// The scope admits all three "move" workloads as candidates; only the
		// pdbaware veto may keep the protected one out of the plan.
		run, err := newRun("pa-plan", repackv1alpha1.RepackModeDryRun).
			goal(npuResource).
			scope(&repackv1alpha1.RepackScope{
				PodGroups: &repackv1alpha1.RepackSelectorTerm{
					Include: &repackv1alpha1.RepackSelector{Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{nativeScopeLabel: "move"}}},
				},
			}).
			create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("RepackRecommended"))
		Expect(got.Status.Plan).NotTo(BeNil())

		planned := map[string]bool{}
		for _, mv := range got.Status.Plan.Moves {
			Expect(mv.PodGroupName).NotTo(Equal(protected.podGroup),
				"a PDB-blocked workload must never be planned as a victim")
			planned[mv.PodGroupName] = true
		}
		Expect(planned[openA.podGroup] || planned[openB.podGroup]).To(BeTrue(),
			"at least one open workload must be planned for consolidation")
	})
})
