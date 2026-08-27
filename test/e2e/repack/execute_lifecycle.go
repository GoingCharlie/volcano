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

package repack

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"

	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

var _ = Describe("Repack Execute, scope, maxPerRun & lifecycle", Serial, func() {
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

	// C6: Execute actually commits — it evicts and writes durable relocations.
	It("Execute evicts and records relocations", func() {
		moving := occupyNativeDeployment(ctx, "exec-moving", nodes[0], "move", 4)
		staying := occupyNativeDeployment(ctx, "exec-staying", nodes[1], "stay", 2)
		defer deleteNativeWorkloads(ctx, moving, staying)

		run, err := newRun("execute", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
		Expect(got.Status.Plan).NotTo(BeNil())
		Expect(got.Status.Plan.Summary).NotTo(BeNil())
		Expect(got.Status.Plan.Summary.ResolvedScope).NotTo(BeNil())
		Expect(got.Status.Plan.Summary.ResolvedScope.NodeCount).To(BeEquivalentTo(3))
		Expect(got.Status.Plan.Summary.ResolvedScope.PodGroupCount).To(BeNumerically(">=", 2))
		Expect(got.Status.Plan.Summary.FreedNodeCount).To(BeNumerically(">=", 1), "Execute must preserve the predicted node-freeing benefit")
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MetricsVerified).To(BeTrue())
		Expect(got.Status.Result.FreedNodeCount).To(BeNumerically(">=", 1), "Execute must report nodes actually free after replacement binding")
		Expect(got.Status.Result.FreedNodes).To(Equal(got.Status.Plan.FreedNodes),
			"successful Execute must verify the exact planned freed-node set, not only the count")
		Expect(got.Status.Result.FragAfterPercent).To(BeNumerically("<=", got.Status.Plan.Summary.FragBeforePercent),
			"Execute terminal status must report the remeasured cluster fragmentation")
		Expect(got.Status.Relocations).NotTo(BeEmpty(), "Execute must record placement relocations")
		waitRunEventReasons(ctx, got,
			"PlanComputed", "ExecutePrepared", "EvictionsIssued", "ReconcilingPlacements", "ExecutionCompleted")
	})

	It("executes the replacement protocol for a real vcjob", func() {
		occupyMovableVCJob(ctx, "vcjob-moving", nodes[0], 4)
		occupyMovableVCJob(ctx, "vcjob-staying", nodes[1], 2)

		run, err := newRun("execute-vcjob", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
		Expect(got.Status.Relocations).NotTo(BeEmpty())
		for _, nomination := range got.Status.Relocations {
			Expect(nomination.Placement.Phase).To(Equal(repackv1alpha1.PodPlacementPlaced))
			Expect(nomination.Placement.ReplacementPodName).NotTo(BeEmpty())
			Expect(nomination.Placement.ActualNodeName).To(Equal(nomination.Placement.SelectedNodeName))
		}
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MetricsVerified).To(BeTrue())
	})

	It("reports a verified zero result for an Execute no-op", func() {
		occupy(ctx, "execute-noop", nodes[0], 4)

		run, err := newRun("execute-noop", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("NoFragmentation"))
		Expect(got.Status.Plan).NotTo(BeNil())
		Expect(got.Status.Plan.Moves).To(BeEmpty())
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MovedCardCount).To(BeEquivalentTo(0))
		Expect(got.Status.Result.FreedNodeCount).To(BeEquivalentTo(0))
		Expect(got.Status.Result.FragAfterPercent).To(Equal(got.Status.Plan.Summary.FragBeforePercent))
		Expect(got.Status.Result.MetricsVerified).To(BeTrue())
		Expect(got.Status.Relocations).To(BeEmpty())
	})

	// C8: after an Execute finishes, a second Execute within the cooldown window
	// remains Pending with Progressing=False/ExecuteCooldownActive. (K=1
	// concurrent AnotherRunActive is timing-racy in e2e — the gate logic itself
	// is unit-tested in state.EvaluateGate.)
	It("gates a second Execute during the cooldown window", func() {
		moving := occupyNativeDeployment(ctx, "cooldown-moving", nodes[0], "move", 4)
		staying := occupyNativeDeployment(ctx, "cooldown-staying", nodes[1], "stay", 2)
		defer deleteNativeWorkloads(ctx, moving, staying)

		first, err := newRun("cooldown-1", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, first.Name)
		waitTerminal(ctx, first.Name)

		second, err := newRun("cooldown-2", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, second.Name)
		Expect(waitConditionReason(ctx, second.Name, "Progressing", metav1.ConditionFalse)).
			To(Equal("ExecuteCooldownActive"))
	})

	// C9: pdbaware vetoes every PDB-blocked victim at planning time, so when ALL
	// candidates are protected the Run produces no plan, never evicts, and
	// completes normally. PDB is never bypassed: the protected pods keep running.
	It("plans and evicts nothing when all candidates are PDB-blocked", func() {
		jobA := occupy(ctx, "pdb-a", nodes[0], 4)
		jobB := occupy(ctx, "pdb-b", nodes[1], 2)
		// Every movable fixture pod is protected, so pdbaware must exclude every
		// candidate and no plan is produced.
		blockAll := intstr.FromInt(0)
		for _, job := range []*batchv1alpha1.Job{jobA, jobB} {
			pdbName := "block-" + job.Name
			_, err := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Create(context.TODO(),
				&policyv1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{Name: pdbName},
					Spec: policyv1.PodDisruptionBudgetSpec{
						MaxUnavailable: &blockAll,
						Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"volcano.sh/job-name": job.Name}},
					},
				}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			waitPDBAllowance(ctx, pdbName, 0)
		}

		run, err := newRun("execfail", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		// No plan is produced (every candidate is PDB-blocked), so the Run
		// completes normally without attempting any eviction.
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(SatisfyAny(
			Equal("NoFragmentation"),
			Equal("InsufficientImprovement")))
		Expect(got.Status.Relocations).To(BeEmpty(),
			"PDB-blocked victims must never be planned or evicted")
		// PDB was never bypassed: both protected pods are still running.
		Expect(runningPodCount(ctx)).To(Equal(2), "blocked victims must never be evicted")
	})

	It("plans only the unprotected workload and leaves the PDB-blocked one untouched", func() {
		// accepted sits alone on nodes[0] (moving it frees the node), while the
		// PDB-blocked workload shares the "move" scope on nodes[1]. pdbaware must
		// exclude the blocked workload from the plan, so only accepted is moved.
		accepted := occupyNativeDeployment(ctx, "partial-accepted", nodes[0], "move", 2)
		blocked := occupyNativeDeployment(ctx, "partial-blocked", nodes[1], "move", 2)
		defer deleteNativeWorkloads(ctx, accepted, blocked)

		blockAll := intstr.FromInt(0)
		_, err := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Create(context.TODO(),
			&policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "block-partial"},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MaxUnavailable: &blockAll,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
						nativeWorkloadLabel: blocked.deployment.Name,
					}},
				},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		waitPDBAllowance(ctx, "block-partial", 0)

		run, err := newRun("partial-pdb", repackv1alpha1.RepackModeExecute).
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

		// The unprotected workload is evicted and placed; the PDB-blocked one is
		// never planned (it never appears in the journal).
		Eventually(func() bool {
			placed := false
			for _, relocation := range getRun(ctx, run.Name).Status.Relocations {
				Expect(relocation.PodGroupName).NotTo(Equal(blocked.podGroup),
					"a PDB-blocked workload must never be planned or evicted")
				if relocation.PodGroupName == accepted.podGroup {
					placed = relocation.Eviction.Phase == repackv1alpha1.PodEvictionAccepted &&
						relocation.Placement.Phase == repackv1alpha1.PodPlacementPlaced
				}
			}
			return placed
		}, repackTimeout, repackPoll).Should(BeTrue(),
			"the unprotected workload must be evicted and placed")

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
		for _, relocation := range got.Status.Relocations {
			Expect(relocation.PodGroupName).NotTo(Equal(blocked.podGroup),
				"a PDB-blocked workload must never be planned or evicted")
		}
		// PDB was never bypassed: the protected pod stays Running on its node.
		protectedPod, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(
			context.TODO(), blocked.podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(protectedPod.Status.Phase).To(Equal(v1.PodRunning))
		Expect(protectedPod.Spec.NodeName).To(Equal(nodes[1]),
			"the PDB-protected workload must never be evicted")
	})

	// E16: scope.nodes.exclude — an excluded node is never a drain target, so it is
	// not freed even if it could be.
	It("scope.nodes.exclude keeps a node from being drained", func() {
		occupy(ctx, "sc-a", nodes[0], 4)
		occupy(ctx, "sc-b", nodes[1], 2)

		scope := &repackv1alpha1.RepackScope{
			Nodes: &repackv1alpha1.RepackSelectorTerm{
				Exclude: &repackv1alpha1.RepackSelector{Names: []string{nodes[0], nodes[1]}},
			},
		}
		run, err := newRun("scope-nodes", repackv1alpha1.RepackModeDryRun).goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		// Both occupied nodes are excluded from draining -> nothing to free.
		Expect(got.Status.Plan.Summary.FreedNodeCount).To(BeEquivalentTo(0))
		Expect(got.Status.Plan.FreedNodes).NotTo(ContainElement(nodes[0]))
		Expect(got.Status.Plan.FreedNodes).NotTo(ContainElement(nodes[1]))
	})

	// E14: scope.podGroups.include by exact name — only the selected gang may move.
	It("scope.podGroups.include limits which gangs move", func() {
		occupy(ctx, "inc-a", nodes[0], 4)
		selectedJob := occupy(ctx, "inc-b", nodes[1], 2)
		selectedPodGroup := podGroupNameForOwner(ctx, selectedJob.UID)

		// Include the known 2-card PodGroup, which can move onto inc-a's node and
		// therefore must produce a non-empty plan. This avoids a vacuous pass.
		scope := &repackv1alpha1.RepackScope{
			PodGroups: &repackv1alpha1.RepackSelectorTerm{
				Include: &repackv1alpha1.RepackSelector{Names: []string{selectedPodGroup}},
			},
		}
		run, err := newRun("scope-pg", repackv1alpha1.RepackModeDryRun).goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Plan.Moves).NotTo(BeEmpty(), "the selected movable PodGroup must produce a plan")
		// Every move must belong to the included PodGroup.
		for _, m := range got.Status.Plan.Moves {
			Expect(ctx.Namespace + "/" + m.PodGroupName).To(Equal(selectedPodGroup))
		}
	})

	It("moves all replicas of a scoped vcjob PodGroup when minAvailable is one", func() {
		multi := occupyVCJobReplicas(ctx, "gang-minavailable", nodes[0], 2, 2, 1)
		occupy(ctx, "gang-receiver", nodes[1], 4)
		selectedPodGroup := podGroupNameForOwner(ctx, multi.UID)
		scope := &repackv1alpha1.RepackScope{
			PodGroups: &repackv1alpha1.RepackSelectorTerm{
				Include: &repackv1alpha1.RepackSelector{Names: []string{selectedPodGroup}},
			},
			Nodes: &repackv1alpha1.RepackSelectorTerm{
				Include: &repackv1alpha1.RepackSelector{Names: []string{nodes[0]}},
			},
		}

		run, err := newRun("gang-minavailable", repackv1alpha1.RepackModeDryRun).
			goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(completeReason(got)).To(Equal("RepackRecommended"))
		Expect(got.Status.Plan.Moves).To(HaveLen(1))
		Expect(ctx.Namespace + "/" + got.Status.Plan.Moves[0].PodGroupName).To(Equal(selectedPodGroup))
		Expect(got.Status.Plan.Moves[0].Pods).To(HaveLen(2),
			"whole-PodGroup movement must include both replicas despite minAvailable=1")
		Expect(got.Status.Plan.Moves[0].Cards).To(BeEquivalentTo(4))
	})

	// E14b/C6b: labels from a generic ReplicaSet-owned workload are projected to
	// its automatic PodGroup, and its replacement retains that PodGroup because
	// it is created by the same ReplicaSet.
	It("selects a native workload by PodGroup labels and places its replacement", func() {
		moving := occupyNativeDeployment(ctx, "native-moving", nodes[0], "move", 4)
		staying := occupyNativeDeployment(ctx, "native-staying", nodes[1], "stay", 2)
		defer func() {
			_ = ctx.Kubeclient.AppsV1().Deployments(ctx.Namespace).Delete(context.TODO(), moving.deployment.Name, metav1.DeleteOptions{})
			_ = ctx.Kubeclient.AppsV1().Deployments(ctx.Namespace).Delete(context.TODO(), staying.deployment.Name, metav1.DeleteOptions{})
		}()

		Eventually(func() map[string]string {
			pg, getErr := ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Get(context.TODO(), moving.podGroup, metav1.GetOptions{})
			if getErr != nil {
				return nil
			}
			return pg.Labels
		}, repackTimeout, repackPoll).Should(HaveKeyWithValue(nativeScopeLabel, "move"), "automatic PodGroup must expose pod-template labels")

		scope := &repackv1alpha1.RepackScope{PodGroups: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nativeScopeLabel: "move"}}},
		}}
		run, err := newRun("native-selector", repackv1alpha1.RepackModeExecute).goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
		Expect(got.Status.Plan.Moves).NotTo(BeEmpty())
		for _, move := range got.Status.Plan.Moves {
			Expect(move.PodGroupName).To(Equal(moving.podGroup), "only the selector-matched native PodGroup may move")
		}
		Expect(got.Status.Relocations).To(HaveLen(1), "one relocated native replica needs one nomination")
		nomination := got.Status.Relocations[0]
		Expect(nomination.PodGroupName).To(Equal(moving.podGroup))
		Expect(nomination.Placement.SelectedNodeName).NotTo(BeEmpty(), "engine must persist its live receiver selection")
		Expect(got.Status.Plan.FreedNodes).NotTo(ContainElement(nomination.Placement.SelectedNodeName), "engine must not select a node this run frees")
		Expect(nomination.Placement.ActualNodeName).To(Equal(nomination.Placement.SelectedNodeName), "controller must record the actual placement")
		Eventually(func() bool {
			pods, listErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: nativeWorkloadLabel + "=" + moving.deployment.Name,
			})
			if listErr != nil || len(pods.Items) != 1 {
				return false
			}
			replacement := pods.Items[0]
			return replacement.Name != moving.podName &&
				replacement.Annotations["scheduling.k8s.io/group-name"] == moving.podGroup &&
				replacement.Spec.NodeName == nomination.Placement.SelectedNodeName
		}, repackTimeout, repackPoll).Should(BeTrue(), "Deployment replacement must retain its ReplicaSet-derived PodGroup")
	})

	// F18: maxPerRun.podGroups caps the number of gangs a single run relocates.
	It("maxPerRun.podGroups caps moved gangs", func() {
		occupy(ctx, "cap-a", nodes[0], 2)
		occupy(ctx, "cap-b", nodes[1], 2)
		occupy(ctx, "cap-c", nodes[2], 2)

		one := int32(1)
		run, err := newRun("maxperrun", repackv1alpha1.RepackModeDryRun).goal(npuResource).
			maxPerRun(&repackv1alpha1.MaxPerRun{PodGroups: &one}).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("RepackRecommended"))
		Expect(got.Status.Plan.Summary.FreedNodeCount).To(BeEquivalentTo(1))
		Expect(got.Status.Plan.Moves).To(HaveLen(1), "maxPerRun.podGroups=1 must allow exactly one gang move")
	})

	// F19: maxPerRun.resources is measured in user-facing whole accelerator
	// cards. The only useful relocation here moves a 2-card gang.
	It("maxPerRun.resources caps moved accelerator cards", func() {
		occupy(ctx, "resource-cap-a", nodes[0], 4)
		occupy(ctx, "resource-cap-b", nodes[1], 2)

		blocked, err := newRun("resource-cap-blocked", repackv1alpha1.RepackModeDryRun).goal(npuResource).
			maxPerRun(&repackv1alpha1.MaxPerRun{
				Resources: v1.ResourceList{npuResource: resource.MustParse("1")},
			}).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, blocked.Name)
		blockedRun := waitTerminal(ctx, blocked.Name)
		Expect(blockedRun.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(blockedRun)).To(Equal("InsufficientImprovement"))
		Expect(blockedRun.Status.Plan.Moves).To(BeEmpty())

		admitted, err := newRun("resource-cap-admitted", repackv1alpha1.RepackModeDryRun).goal(npuResource).
			maxPerRun(&repackv1alpha1.MaxPerRun{
				Resources: v1.ResourceList{npuResource: resource.MustParse("2")},
			}).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, admitted.Name)
		admittedRun := waitTerminal(ctx, admitted.Name)
		Expect(admittedRun.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(admittedRun)).To(Equal("RepackRecommended"))
		Expect(admittedRun.Status.Plan.Summary.FreedNodeCount).To(BeEquivalentTo(1))
		Expect(admittedRun.Status.Plan.Moves).To(HaveLen(1))
	})

	// G20: a finished run with a short TTL is GC-deleted by the controller.
	It("TTL GC deletes a finished run", func() {
		occupy(ctx, "ttl", nodes[0], 4) // clean -> Succeeded quickly

		run, err := newRun("ttl-gc", repackv1alpha1.RepackModeDryRun).goal(npuResource).ttl(10).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		waitTerminal(ctx, run.Name)

		err = wait.PollUntilContextTimeout(context.TODO(), repackPoll, repackTimeout, false,
			func(c context.Context) (bool, error) {
				_, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Get(c, run.Name, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				return false, nil
			})
		Expect(err).NotTo(HaveOccurred(), "run should be GC-deleted after its TTL")
	})
})

func deleteNativeWorkloads(ctx *e2eutil.TestContext, workloads ...*nativeWorkload) {
	for _, workload := range workloads {
		if workload == nil {
			continue
		}
		if workload.deployment != nil {
			_ = ctx.Kubeclient.AppsV1().Deployments(ctx.Namespace).Delete(context.TODO(), workload.deployment.Name, metav1.DeleteOptions{})
		}
		if workload.statefulSet != nil {
			_ = ctx.Kubeclient.AppsV1().StatefulSets(ctx.Namespace).Delete(context.TODO(), workload.statefulSet.Name, metav1.DeleteOptions{})
		}
	}
}

// Note on coverage gaps that are intentionally NOT e2e-tested here:
//   - K=1 concurrent (AnotherRunActive): the window is too small to observe
//     reliably (Execute is open-loop-fast); covered by state.EvaluateGate UT and
//     the engine's TestRequeueGatedRuns / gate concurrency UT.
//   - /metrics and /healthz endpoints: the engine has no Service, so scraping its
//     pod requires port-forward; covered by the cmd wiring + UT.
