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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

// These e2e cases exercise the PDB-compatible eviction WAVE model:
// temporary 429 refusals keep the victims
// InProgress with backoff, accepted replacements are placed before the next
// wave, and the retry deadline (--repack-eviction-retry-timeout) bounds the
// wait. The Eviction API remains the final PDB arbiter — no PDB is bypassed.
var _ = Describe("Repack PDB retry & backoff", Serial, func() {
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

	// E2E-1: a 2-replica gang behind PDB maxUnavailable=1. The first eviction
	// wave accepts one replica and blocks the other (429); the accepted
	// replacement is placed first (restoring PDB allowance), then the blocked
	// victim is retried and accepted. The node is eventually freed and the Run
	// succeeds without ever bypassing the PDB.
	It("rolls a 2-replica gang through a maxUnavailable=1 PDB in waves", func() {
		DeferCleanup(withEvictionRetryTimeout(ctx, 30*time.Second))

		// A native Deployment backs the "gang": unlike a volcano Job, its owner
		// implements the scale subresource, so the k8s disruption controller can
		// compute DisruptionsAllowed (=1 for maxUnavailable=1). A PDB on volcano
		// Job pods never computes in kind ("jobs.batch.volcano.sh does not
		// implement the scale subresource") and would block every eviction.
		gang := occupyNativeDeploymentReplicas(ctx, "pdb-wave", nodes[0], "move", 2, 2)
		occupy(ctx, "pdb-wave-receiver", nodes[1], 2)
		defer deleteNativeWorkloads(ctx, gang)

		maxOne := intstr.FromInt(1)
		_, err := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Create(context.TODO(),
			&policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "wave-pdb"},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MaxUnavailable: &maxOne,
					Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{nativeWorkloadLabel: gang.deployment.Name}},
				},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			pdb, getErr := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Get(
				context.TODO(), "wave-pdb", metav1.GetOptions{})
			if getErr != nil {
				return false
			}
			// DesiredHealthy>0 proves the disruption controller actually
			// computed this PDB; only then is DisruptionsAllowed meaningful.
			return pdb.Status.DesiredHealthy > 0 && pdb.Status.DisruptionsAllowed == 1
		}, repackTimeout, repackPoll).Should(BeTrue(),
			"PDB maxUnavailable=1 must compute and allow exactly one disruption at a time")

		run, err := newRun("pdb-wave", repackv1alpha1.RepackModeExecute).
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
		Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
		Expect(got.Status.Relocations).To(HaveLen(2), "both gang replicas must be relocated")
		for _, relocation := range got.Status.Relocations {
			Expect(relocation.Placement.Phase).To(Equal(repackv1alpha1.PodPlacementPlaced))
			Expect(relocation.Placement.ActualNodeName).NotTo(Equal(nodes[0]),
				"replacement must leave the drained node")
		}
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MetricsVerified).To(BeTrue())
		Expect(got.Status.Result.FreedNodes).To(ContainElement(nodes[0]),
			"the drained node must actually be freed")

		// The PDB throttled the first wave (EvictionBlocked) and the retry
		// recovered (EvictionUnblocked) once the first replacement was placed.
		waitRunEventReasons(ctx, got, "EvictionsIssued", "EvictionBlocked", "EvictionUnblocked",
			"ReconcilingPlacements", "ExecutionCompleted")

		// Both gang replicas are Running again, none on the drained node.
		Eventually(func() bool {
			pods := runningNativePods(ctx, gang.deployment.Name)
			if len(pods) != 2 {
				return false
			}
			for _, pod := range pods {
				if pod.Spec.NodeName == nodes[0] {
					return false
				}
			}
			return true
		}, repackTimeout, repackPoll).Should(BeTrue(), "gang must recover on new nodes")
	})

	// E2E-3: one PDB-blocked workload and two open workloads in the scope. The
	// pdbaware plugin vetoes the PDB-blocked victim at PLAN time, so the plan
	// contains only the open workloads: they are evicted and placed, freeing a
	// node, while the PDB-protected workload is never planned or touched.
	It("plans only the open workloads and never touches the PDB-blocked one", func() {
		openA := occupyNativeDeployment(ctx, "e3-open-a", nodes[0], "move", 4)
		openB := occupyNativeDeployment(ctx, "e3-open-b", nodes[1], "move", 2)
		protected := occupyNativeDeployment(ctx, "e3-protected", nodes[2], "move", 2)
		defer deleteNativeWorkloads(ctx, openA, openB, protected)

		blockAll := intstr.FromInt(0)
		_, err := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Create(context.TODO(),
			&policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "e3-protect-pdb"},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MaxUnavailable: &blockAll,
					Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{nativeWorkloadLabel: protected.deployment.Name}},
				},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		waitPDBAllowance(ctx, "e3-protect-pdb", 0)

		run, err := newRun("e3-pdbaware", repackv1alpha1.RepackModeExecute).
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

		// pdbaware must keep the PDB-blocked workload out of the journal
		// entirely, while at least one open workload is evicted and placed.
		Eventually(func() bool {
			openPlaced := false
			for _, relocation := range getRun(ctx, run.Name).Status.Relocations {
				switch relocation.PodGroupName {
				case openA.podGroup, openB.podGroup:
					openPlaced = openPlaced ||
						(relocation.Eviction.Phase == repackv1alpha1.PodEvictionAccepted &&
							relocation.Placement.Phase == repackv1alpha1.PodPlacementPlaced)
				}
			}
			return openPlaced
		}, repackTimeout, repackPoll).Should(BeTrue(),
			"an open workload must be evicted and its replacement placed")

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
		// The PDB-blocked workload was never planned, so it was never touched.
		for _, relocation := range got.Status.Relocations {
			Expect(relocation.PodGroupName).NotTo(Equal(protected.podGroup),
				"a PDB-blocked workload must never be planned or evicted")
		}
		protectedPod, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(
			context.TODO(), protected.podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(protectedPod.Status.Phase).To(Equal(v1.PodRunning))
		Expect(protectedPod.Spec.NodeName).To(Equal(nodes[2]),
			"the PDB-protected workload must never be evicted")
	})

	// E2E-4: every candidate workload is fully PDB-blocked (maxUnavailable=0).
	// The pdbaware plugin excludes them all at planning time, so the Run plans
	// nothing, never evicts, and fails quickly without touching any Pod.
	It("plans and evicts nothing when every candidate is PDB-blocked", func() {
		jobA := occupy(ctx, "e4-a", nodes[0], 4)
		jobB := occupy(ctx, "e4-b", nodes[1], 2)
		blockAll := intstr.FromInt(0)
		for _, job := range []*batchv1alpha1.Job{jobA, jobB} {
			pdbName := "e4-block-" + job.Name
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

		run, err := newRun("e4-blocked", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		// pdbaware excludes every PDB-blocked victim, so no plan is produced;
		// with no plan to execute the Run completes normally (no eviction is
		// even attempted), reporting why no consolidation happened.
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(SatisfyAny(
			Equal("NoFragmentation"),
			Equal("InsufficientImprovement")))
		Expect(got.Status.Relocations).To(BeEmpty(),
			"PDB-blocked victims must never be planned or evicted")
		// Zero disturbance: both workloads are still Running.
		for _, job := range []*batchv1alpha1.Job{jobA, jobB} {
			Eventually(func() bool {
				pods, listErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).List(context.TODO(), metav1.ListOptions{
					LabelSelector: "volcano.sh/job-name=" + job.Name,
				})
				if listErr != nil || len(pods.Items) == 0 {
					return false
				}
				for i := range pods.Items {
					if pods.Items[i].Status.Phase != v1.PodRunning {
						return false
					}
				}
				return true
			}, fixtureTimeout, repackPoll).Should(BeTrue(),
				"the PDB-blocked workload must remain running")
		}
	})

	// E2E-5: while a committed replacement cannot be placed (no idle receiver),
	// the engine must NOT expand eviction for a retryable victim; when the
	// placement deadline hits, eviction stops (the victim is never touched) and
	// the Run fails PlacementTimedOut, releasing the gate.
	It("does not expand eviction while a committed replacement is pending, and stops at its placement deadline", func() {
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()

		// The committed replacement is structurally unschedulable (its
		// nodeSelector matches no node), so no idle receiver can ever exist.
		// This keeps the placement durably pending regardless of node occupancy
		// or cache timing, which the fake-NPU blockers could not guarantee after
		// an engine restart.
		run, pgName, replacement := prepareGatedPlacementUnschedulable(ctx, "e5-pending", nodes[1], []string{nodes[0]}, 30*time.Second)
		defer deleteRun(ctx, run.Name)
		Expect(hasSchedulingGate(replacement, repackv1alpha1.PlacementGateName)).To(BeTrue(),
			"webhook must synchronously gate the replacement")

		// A retryable victim linked by the plan: if the engine ever ran eviction
		// for it, it would be evicted (no PDB). Its survival proves the
		// placement-first and stop-on-placement-timeout ordering.
		victim := e2eutil.CreatePod(ctx, e2eutil.PodSpec{
			Name: "e5-victim", RestartPolicy: v1.RestartPolicyNever,
		})
		Eventually(func() bool {
			pod, getErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(context.TODO(), victim.Name, metav1.GetOptions{})
			return getErr == nil && pod.Status.Phase == v1.PodRunning
		}, fixtureTimeout, repackPoll).Should(BeTrue(), "victim pod must be running")

		current := getRun(ctx, run.Name)
		current.Status.Plan.Moves = append(current.Status.Plan.Moves, repackv1alpha1.RepackMove{
			Namespace: ctx.Namespace, PodGroupName: pgName, Cards: 1,
			Pods: []repackv1alpha1.PodMove{{Name: victim.Name, FromNode: nodes[0], ToNode: nodes[1], Cards: 1}},
		})
		expires := metav1.NewTime(time.Now().Add(30 * time.Second))
		current.Status.Relocations = append(current.Status.Relocations, repackv1alpha1.PodRelocationStatus{
			Namespace: ctx.Namespace, PodGroupName: pgName, VictimPodName: victim.Name, VictimPodUID: victim.UID,
			PlannedNodeName: nodes[1],
			Eviction:        repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionInProgress},
			Placement: repackv1alpha1.PodPlacementStatus{
				Phase: repackv1alpha1.PodPlacementWaitingForReplacement, ExpirationTime: &expires,
			},
		})
		_, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().UpdateStatus(context.TODO(), current, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred(), "append retryable relocation")

		// The committed replacement reaches the observable WaitingForNodeSelection
		// state while the engine is still paused.
		Eventually(func() repackv1alpha1.PodPlacementPhase {
			return getRun(ctx, run.Name).Status.Relocations[0].Placement.Phase
		}, repackTimeout, repackPoll).Should(Equal(repackv1alpha1.PodPlacementWaitingForNodeSelection))

		restoreEngine()

		// While the committed placement is pending, no further eviction is
		// issued: the retryable victim stays untouched.
		Consistently(func() bool {
			pod, getErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(context.TODO(), victim.Name, metav1.GetOptions{})
			return getErr == nil && pod.UID == victim.UID && pod.Status.Phase == v1.PodRunning
		}, 5*time.Second, repackPoll).Should(BeTrue(),
			"no further eviction while a committed replacement is pending")

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackFailed))
		Expect(completeReason(got)).To(Equal("PlacementTimedOut"))
		Expect(got.Status.Relocations[0].Placement.Phase).To(Equal(repackv1alpha1.PodPlacementTimedOut))

		// The deadline releases only the repack placement gate; the retryable
		// victim was never evicted.
		Eventually(func() bool {
			pod, getErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(context.TODO(), replacement.Name, metav1.GetOptions{})
			return getErr == nil && !hasSchedulingGate(pod, repackv1alpha1.PlacementGateName)
		}, repackTimeout, repackPoll).Should(BeTrue(), "placement deadline must release the gate")
		victimPod, getErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(context.TODO(), victim.Name, metav1.GetOptions{})
		Expect(getErr).NotTo(HaveOccurred())
		Expect(victimPod.UID).To(Equal(victim.UID), "the retryable victim must never be evicted")
		Expect(victimPod.Status.Phase).To(Equal(v1.PodRunning))
		assertPlacementLeaseReleased(ctx, pgName)
	})
})
