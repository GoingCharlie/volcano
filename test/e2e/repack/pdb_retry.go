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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

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

	// E2E-3: an accepted eviction's replacement is placed BEFORE the retryable
	// (PDB-blocked) victim is retried to the eviction deadline. A durable
	// eviction journal is injected directly (bypassing planning, which the
	// pdbaware plugin would filter the blocked victim out of), so the
	// execute-time PDB retry semantics stay observable: the open victim is
	// accepted and placed first, the protected one stays InProgress and is
	// retried until the deadline, then rejected — the Run fails
	// BenefitNotRealized and the protected Pod survives.
	It("places the accepted replacement before retrying the PDB-blocked victim to the deadline", func() {
		DeferCleanup(withEvictionRetryTimeout(ctx, 25*time.Second))

		open := occupyNativeDeployment(ctx, "e3-open", nodes[0], "move", 2)
		protected := occupyNativeDeployment(ctx, "e3-protected", nodes[0], "move", 2)
		defer deleteNativeWorkloads(ctx, open, protected)

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

		// Freeze the engine, inject a durable plan+Pending journal that includes
		// BOTH victims — the blocked one enters the plan because the journal is
		// written directly, not via pdbaware-filtered planning — then resume.
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()
		run := prepareEvictionJournal(ctx, "e3-retry", []string{nodes[0]}, []evictionJournalVictim{
			{podGroupName: open.podGroup, podName: open.podName, podUID: open.podUID, fromNode: nodes[0], toNode: nodes[1], cards: 2},
			{podGroupName: protected.podGroup, podName: protected.podName, podUID: protected.podUID, fromNode: nodes[0], toNode: nodes[1], cards: 2},
		})
		defer deleteRun(ctx, run.Name)
		restoreEngine()

		// The accepted replacement is placed while the protected victim stays
		// blocked (placement priority over the next wave).
		Eventually(func() bool {
			openPlaced, protectedBlocked := false, false
			for _, relocation := range getRun(ctx, run.Name).Status.Relocations {
				switch relocation.PodGroupName {
				case open.podGroup:
					openPlaced = relocation.Eviction.Phase == repackv1alpha1.PodEvictionAccepted &&
						relocation.Placement.Phase == repackv1alpha1.PodPlacementPlaced
				case protected.podGroup:
					protectedBlocked = relocation.Eviction.Phase == repackv1alpha1.PodEvictionInProgress
				}
			}
			return openPlaced && protectedBlocked
		}, repackTimeout, repackPoll).Should(BeTrue(),
			"the unprotected replacement must be placed while the protected victim stays blocked")

		// The protected victim is retried until the deadline, then rejected: the
		// planned node cannot be freed, so the Run fails without touching it.
		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackFailed))
		Expect(completeReason(got)).To(Equal("BenefitNotRealized"))
		protectedPod, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(
			context.TODO(), protected.podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(protectedPod.Status.Phase).To(Equal(v1.PodRunning))
		Expect(protectedPod.Spec.NodeName).To(Equal(nodes[0]),
			"the PDB-protected workload must never be evicted")
	})

	// E2E-4: while all evictions are PDB-blocked, restart the engine. The new
	// instance must resume the SAME victim (UID precondition — no double-evict)
	// and keep retrying until the deadline, without bypassing the PDB. The
	// victims enter the plan via a directly injected durable journal (pdbaware
	// would filter them out of a freshly planned plan), so the execute-time
	// restart-recovery semantics stay observable.
	It("resumes a PDB-blocked eviction after an Engine restart without double-evicting", func() {
		DeferCleanup(withEvictionRetryTimeout(ctx, 60*time.Second))

		jobA := occupyNativeDeployment(ctx, "e4-a", nodes[0], "move", 4)
		jobB := occupyNativeDeployment(ctx, "e4-b", nodes[1], "move", 2)
		defer deleteNativeWorkloads(ctx, jobA, jobB)

		blockAll := intstr.FromInt(0)
		for _, workload := range []*nativeWorkload{jobA, jobB} {
			pdbName := "e4-block-" + workload.deployment.Name
			_, err := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Create(context.TODO(),
				&policyv1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{Name: pdbName},
					Spec: policyv1.PodDisruptionBudgetSpec{
						MaxUnavailable: &blockAll,
						Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{nativeWorkloadLabel: workload.deployment.Name}},
					},
				}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			waitPDBAllowance(ctx, pdbName, 0)
		}

		// Inject a durable journal with both PDB-blocked victims, then resume.
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()
		run := prepareEvictionJournal(ctx, "e4-restart", []string{nodes[0]}, []evictionJournalVictim{
			{podGroupName: jobA.podGroup, podName: jobA.podName, podUID: jobA.podUID, fromNode: nodes[0], toNode: nodes[2], cards: 4},
			{podGroupName: jobB.podGroup, podName: jobB.podName, podUID: jobB.podUID, fromNode: nodes[1], toNode: nodes[2], cards: 2},
		})
		defer deleteRun(ctx, run.Name)
		restoreEngine()

		// Reach the blocked state: durable InProgress victims that the PDB
		// refuses to let the API evict (the pods themselves are never deleted).
		Eventually(func() bool {
			r := getRun(ctx, run.Name)
			if len(r.Status.Relocations) == 0 {
				return false
			}
			for _, relocation := range r.Status.Relocations {
				if relocation.Eviction.Phase != repackv1alpha1.PodEvictionInProgress {
					return false
				}
			}
			return true
		}, repackTimeout, repackPoll).Should(BeTrue(), "every planned victim must be PDB-blocked InProgress")
		victimUIDs := map[types.UID]bool{}
		for _, relocation := range getRun(ctx, run.Name).Status.Relocations {
			Expect(relocation.VictimPodUID).NotTo(BeEmpty())
			victimUIDs[relocation.VictimPodUID] = true
		}
		for victimUID := range victimUIDs {
			Expect(podStillRunningWithUID(ctx, ctx.Namespace, victimUID)).To(BeTrue(),
				"a blocked eviction must never delete its victim")
		}

		// Restart the engine (scale to 0, then back to 1).
		restart := pauseRepackEngine(ctx)
		restart()

		// After restart the engine resumes the SAME durable victims: the journal
		// keeps the original UIDs and the API-level UID precondition prevents any
		// double-evict of a same-name replacement.
		Eventually(func() repackv1alpha1.PodEvictionPhase {
			r := getRun(ctx, run.Name)
			if len(r.Status.Relocations) == 0 {
				return ""
			}
			return r.Status.Relocations[0].Eviction.Phase
		}, repackTimeout, repackPoll).Should(Or(
			Equal(repackv1alpha1.PodEvictionInProgress),
			Equal(repackv1alpha1.PodEvictionRejected)),
			"restarted engine must resume the blocked eviction")
		for _, relocation := range getRun(ctx, run.Name).Status.Relocations {
			Expect(victimUIDs[relocation.VictimPodUID]).To(BeTrue(),
				"the resumed journal must keep targeting the original victim instances")
		}
		for victimUID := range victimUIDs {
			Expect(podStillRunningWithUID(ctx, ctx.Namespace, victimUID)).To(BeTrue(),
				"restart must not double-evict a victim")
		}

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackFailed))
		Expect(completeReason(got)).To(Equal("EvictionFailed"))
		for victimUID := range victimUIDs {
			Expect(podStillRunningWithUID(ctx, ctx.Namespace, victimUID)).To(BeTrue(),
				"PDB was never bypassed across the restart")
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
