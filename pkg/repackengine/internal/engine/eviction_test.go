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

package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	state "volcano.sh/repack-controller/pkg/state"

	evictionexecutor "volcano.sh/volcano/pkg/repackengine/executor/eviction"
)

func TestExecutePreparedEvictionsRecoversAcceptedRequestAfterStatusFailure(t *testing.T) {
	const (
		runName      = "run"
		namespace    = "ns"
		podGroupName = "pg"
		podName      = "victim"
		podUID       = types.UID("victim-uid")
	)
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: runName, UID: "run-uid"},
		Spec: repackv1alpha1.RepackRunSpec{
			Mode:  repackv1alpha1.RepackModeExecute,
			Goals: []repackv1alpha1.RepackGoal{{Resource: "example.com/accelerator"}},
		},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
			Conditions: []metav1.Condition{{
				Type: state.CondProgressing, Status: metav1.ConditionTrue, Reason: state.ReasonEvicting,
			}},
			Plan: &repackv1alpha1.RepackPlan{
				Summary:    &repackv1alpha1.RepackSummary{FragBeforePercent: 50, FreedNodeCount: 1, MovedCardCount: 2},
				FreedNodes: []string{"node-a"},
				Moves: []repackv1alpha1.RepackMove{{
					Namespace: namespace, PodGroupName: podGroupName, Cards: 2,
					Pods: []repackv1alpha1.PodMove{{
						Name: podName, FromNode: "node-a", ToNode: "node-b", Cards: 2,
					}},
				}},
			},
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: namespace, PodGroupName: podGroupName,
				VictimPodName: podName, VictimPodUID: podUID,
				PlannedNodeName: "node-b", Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionPending}, Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
			}},
		},
	}
	podGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: podGroupName,
		Annotations: map[string]string{
			repackv1alpha1.PlacementLeaseAnnotation: "run/run-uid",
		},
	}}
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy(), podGroup)
	statusUpdates := 0
	failAcceptedStatusOnce := true
	volcanoClient.PrependReactor("update", "repackruns", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		statusUpdates++
		updated := action.(k8stesting.UpdateAction).GetObject().(*repackv1alpha1.RepackRun)
		if failAcceptedStatusOnce &&
			updated.Status.Relocations[0].Eviction.Phase == repackv1alpha1.PodEvictionAccepted {
			failAcceptedStatusOnce = false
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: repackv1alpha1.GroupName, Resource: "repackruns"},
				runName, errors.New("simulated status outage after eviction acceptance"))
		}
		return false, nil, nil
	})

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: podName, UID: podUID,
	}}
	kubeClient := kubefake.NewSimpleClientset(pod)
	evictionCalls := 0
	kubeClient.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		evictionCalls++
		eviction := action.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		current, err := kubeClient.Tracker().Get(v1.SchemeGroupVersion.WithResource("pods"), namespace, podName)
		if err != nil {
			return true, nil, err
		}
		terminating := current.(*v1.Pod).DeepCopy()
		now := metav1.Now()
		terminating.DeletionTimestamp = &now
		if err := kubeClient.Tracker().Update(v1.SchemeGroupVersion.WithResource("pods"), terminating, namespace); err != nil {
			return true, nil, err
		}
		return true, eviction, nil
	})

	engine := &Engine{
		volcanoClient: volcanoClient,
		workQueue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string]()),
		recorder:                record.NewFakeRecorder(20),
		now:                     time.Now,
		pendingTerminalStatuses: make(map[string]*repackv1alpha1.RepackRunStatus),
	}
	t.Cleanup(engine.workQueue.ShutDown)

	result := engine.executePreparedEvictionsWithClient(
		context.Background(), run.DeepCopy(), run.Generation, "example.com/accelerator", kubeClient)
	if result.Err == nil {
		t.Fatal("first execution unexpectedly succeeded despite simulated accepted-status outage")
	}
	if evictionCalls != 1 {
		t.Fatalf("Eviction API calls = %d, want 1", evictionCalls)
	}
	persisted, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(
		context.Background(), runName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Status.Relocations[0].Eviction.Phase; got != repackv1alpha1.PodEvictionInProgress {
		t.Fatalf("phase after failed accepted write = %q, want InProgress", got)
	}

	if result := engine.executePreparedEvictionsWithClient(
		context.Background(), persisted.DeepCopy(), persisted.Generation,
		"example.com/accelerator", kubeClient); result.Err != nil {
		t.Fatalf("recovery execution failed: %v", result.Err)
	}
	if evictionCalls != 1 {
		t.Fatalf("Eviction API calls after recovery = %d, want no replay", evictionCalls)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(
		context.Background(), runName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Status.Relocations[0].Eviction.Phase; got != repackv1alpha1.PodEvictionAccepted {
		t.Fatalf("recovered eviction phase = %q, want Accepted", got)
	}
	if updated.Status.Result == nil || updated.Status.Result.MovedCardCount != 2 {
		t.Fatalf("result = %#v, want movedCardCount=2", updated.Status.Result)
	}
	if !hasProgressingReason(updated, state.ReasonReconcilingPlacements) {
		t.Fatalf("conditions = %#v, want ReconcilingPlacements", updated.Status.Conditions)
	}
	if statusUpdates < 4 {
		t.Fatalf("status updates = %d, want durable intent, outcome, result and placement barriers", statusUpdates)
	}
}

func TestClassifyMissingVictimsRequiresAcceptedSibling(t *testing.T) {
	relocations := []repackv1alpha1.PodRelocationStatus{
		{Namespace: "ns", PodGroupName: "group-a", Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}},
		{Namespace: "ns", PodGroupName: "group-a"},
		{Namespace: "ns", PodGroupName: "group-b"},
	}
	if !classifyMissingVictims(relocations, map[int]string{
		1: "Victim Pod was not found.",
		2: "Victim Pod was not found.",
	}) {
		t.Fatal("classification unexpectedly reported no change")
	}
	if got := relocations[1].Eviction.Phase; got != repackv1alpha1.PodEvictionIndirectlyRemoved {
		t.Fatalf("accepted sibling phase = %q, want IndirectlyRemoved", got)
	}
	if got := relocations[2].Eviction.Phase; got != repackv1alpha1.PodEvictionRejected {
		t.Fatalf("unrelated missing victim phase = %q, want Rejected", got)
	}
}

func hasProgressingReason(run *repackv1alpha1.RepackRun, reason string) bool {
	for index := range run.Status.Conditions {
		condition := &run.Status.Conditions[index]
		if condition.Type == state.CondProgressing &&
			condition.Status == metav1.ConditionTrue &&
			condition.Reason == reason {
			return true
		}
	}
	return false
}

// newWaveTestEngine builds an Engine with injectable time and in-memory PDB
// retry state; the caller supplies volcanoClient when a status write is needed.
func newWaveTestEngine(now time.Time) *Engine {
	return &Engine{
		workQueue:               workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		recorder:                record.NewFakeRecorder(50),
		now:                     func() time.Time { return now },
		config:                  Config{NominationTTL: 10 * time.Minute, EvictionRetryTimeout: 10 * time.Minute},
		evictionRetryStates:     make(map[string]map[int]*evictionRetryState),
		evictionBlocked:         make(map[string]bool),
		pendingTerminalStatuses: make(map[string]*repackv1alpha1.RepackRunStatus),
	}
}

// waveTestRun builds a Run whose plan links one move per relocation
// (victim-N on node-a -> node-b), so plannedVictims/collectEvictionWave see all
// of them.
func waveTestRun(evictionPhases ...repackv1alpha1.PodEvictionPhase) *repackv1alpha1.RepackRun {
	now := metav1.NewTime(time.Now())
	relocations := make([]repackv1alpha1.PodRelocationStatus, 0, len(evictionPhases))
	moves := make([]repackv1alpha1.RepackMove, 0, len(evictionPhases))
	for i, phase := range evictionPhases {
		relocations = append(relocations, repackv1alpha1.PodRelocationStatus{
			Namespace: "ns", PodGroupName: "pg",
			VictimPodName: fmt.Sprintf("victim-%d", i), VictimPodUID: types.UID(fmt.Sprintf("uid-%d", i)),
			PlannedNodeName: "node-b",
			Eviction:        repackv1alpha1.PodEvictionStatus{Phase: phase},
			Placement: repackv1alpha1.PodPlacementStatus{
				Phase:          repackv1alpha1.PodPlacementWaitingForReplacement,
				ExpirationTime: &now,
			},
		})
		moves = append(moves, repackv1alpha1.RepackMove{
			Namespace: "ns", PodGroupName: "pg", Cards: 1,
			Pods: []repackv1alpha1.PodMove{{
				Name: fmt.Sprintf("victim-%d", i), FromNode: "node-a", ToNode: "node-b", Cards: 1,
			}},
		})
	}
	start := metav1.NewTime(time.Now().Add(-time.Minute))
	return &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", UID: "run-uid"},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase:     repackv1alpha1.RepackRunning,
			StartTime: &start,
			Plan: &repackv1alpha1.RepackPlan{
				Summary:    &repackv1alpha1.RepackSummary{FragBeforePercent: 50, FreedNodeCount: 1, MovedCardCount: int64(len(moves))},
				FreedNodes: []string{"node-a"},
				Moves:      moves,
			},
			Relocations: relocations,
		},
	}
}

func TestClassifyEvictionError(t *testing.T) {
	tooMany := apierrors.NewTooManyRequests("rate limited", 429)
	serverTimeout := apierrors.NewServerTimeout(schema.GroupResource{}, "op", 1)
	forbidden := apierrors.NewForbidden(schema.GroupResource{}, "x", errors.New("no"))
	invalid := apierrors.NewInvalid(schema.GroupKind{}, "x", nil)
	notFound := apierrors.NewNotFound(schema.GroupResource{}, "x")

	cases := []struct {
		name string
		err  error
		want evictionAttemptResult
	}{
		{"nil is accepted", nil, evictionAccepted},
		{"429 is retryable", tooMany, evictionRetryable},
		{"server timeout is retryable", serverTimeout, evictionRetryable},
		{"503 is retryable", apierrors.NewServiceUnavailable("down"), evictionRetryable},
		{"network uncertainty is retryable", errors.New("connection refused"), evictionRetryable},
		{"403 is permanent", forbidden, evictionPermanentFailure},
		{"422 is permanent", invalid, evictionPermanentFailure},
		{"404 is victim gone", notFound, evictionVictimGone},
		{"ErrVictimNotFound is victim gone", evictionexecutor.ErrVictimNotFound, evictionVictimGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyEvictionError(tc.err); got != tc.want {
				t.Fatalf("classifyEvictionError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestNextExecuteActionPriority verifies placement > eviction > wait > finalize
// and that a timed-out committed placement stops further eviction.
func TestNextExecuteActionPriority(t *testing.T) {
	fixedNow := time.Now()
	engine := newWaveTestEngine(fixedNow)

	t.Run("accepted placement work wins over pending eviction", func(t *testing.T) {
		run := waveTestRun(repackv1alpha1.PodEvictionAccepted, repackv1alpha1.PodEvictionInProgress)
		action, _ := engine.nextExecuteAction(run)
		if action != executePlacement {
			t.Fatalf("action = %v, want executePlacement", action)
		}
	})
	t.Run("pending eviction is due", func(t *testing.T) {
		run := waveTestRun(repackv1alpha1.PodEvictionPending)
		action, _ := engine.nextExecuteAction(run)
		if action != executeEviction {
			t.Fatalf("action = %v, want executeEviction", action)
		}
	})
	t.Run("retryable in backoff yields wait", func(t *testing.T) {
		run := waveTestRun(repackv1alpha1.PodEvictionInProgress)
		engine.recordEvictionRetry(run.Name, 0) // nextTime = now + ~1s
		action, retryAfter := engine.nextExecuteAction(run)
		if action != executeWait {
			t.Fatalf("action = %v, want executeWait", action)
		}
		if retryAfter <= 0 || retryAfter > 2*time.Second {
			t.Fatalf("retryAfter = %v, want ~1s", retryAfter)
		}
	})
	t.Run("all final yields finalize", func(t *testing.T) {
		run := waveTestRun(repackv1alpha1.PodEvictionAccepted)
		run.Status.Relocations[0].Placement.Phase = repackv1alpha1.PodPlacementPlaced
		action, _ := engine.nextExecuteAction(run)
		if action != executeFinalize {
			t.Fatalf("action = %v, want executeFinalize", action)
		}
	})
	t.Run("timed out committed placement stops eviction", func(t *testing.T) {
		run := waveTestRun(repackv1alpha1.PodEvictionAccepted, repackv1alpha1.PodEvictionPending)
		run.Status.Relocations[0].Placement.Phase = repackv1alpha1.PodPlacementTimedOut
		action, _ := engine.nextExecuteAction(run)
		if action != executeFinalize {
			t.Fatalf("action = %v, want executeFinalize", action)
		}
	})
	t.Run("retry deadline passed routes to finalize", func(t *testing.T) {
		run := waveTestRun(repackv1alpha1.PodEvictionInProgress)
		run.Status.StartTime = &metav1.Time{Time: fixedNow.Add(-20 * time.Minute)}
		action, _ := engine.nextExecuteAction(run)
		if action != executeFinalize {
			t.Fatalf("action = %v, want executeFinalize", action)
		}
	})
}

// TestCollectEvictionWaveRespectsBackoff: backoff and final items are skipped,
// due items are collected.
func TestCollectEvictionWaveRespectsBackoff(t *testing.T) {
	fixedNow := time.Now()
	engine := newWaveTestEngine(fixedNow)
	run := waveTestRun(
		repackv1alpha1.PodEvictionPending,    // 0: due
		repackv1alpha1.PodEvictionInProgress, // 1: in backoff -> skipped
		repackv1alpha1.PodEvictionAccepted,   // 2: final -> skipped
	)
	engine.recordEvictionRetry(run.Name, 1)
	engine.evictionRetryStates[run.Name][1].nextTime = fixedNow.Add(5 * time.Second)

	wave := engine.collectEvictionWave(run)
	if len(wave) != 1 || wave[0].relocationIndex != 0 {
		t.Fatalf("wave = %+v, want [relocationIndex 0]", wave)
	}
}

// TestExecutePreparedEvictionsPartialAcceptanceRoutesToPlacement: 3 Accepted +
// 2 PDB 429 -> accepted go to placement, retryable stay InProgress, run not
// terminal.
func TestExecutePreparedEvictionsPartialAcceptanceRoutesToPlacement(t *testing.T) {
	fixedNow := time.Now()
	engine := newWaveTestEngine(fixedNow)
	run := waveTestRun(
		repackv1alpha1.PodEvictionPending,
		repackv1alpha1.PodEvictionPending,
		repackv1alpha1.PodEvictionPending,
		repackv1alpha1.PodEvictionPending,
		repackv1alpha1.PodEvictionPending,
	)
	engine.volcanoClient = vcfake.NewSimpleClientset(run.DeepCopy())

	pods := make([]runtime.Object, 0, 5)
	for i := 0; i < 5; i++ {
		pods = append(pods, &v1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: fmt.Sprintf("victim-%d", i), UID: types.UID(fmt.Sprintf("uid-%d", i)),
		}})
	}
	kubeClient := kubefake.NewSimpleClientset(pods...)
	attempts := 0
	kubeClient.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		eviction := action.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		if eviction.Name == "victim-3" || eviction.Name == "victim-4" {
			return true, nil, apierrors.NewTooManyRequests("PDB maxUnavailable reached", 429)
		}
		return true, eviction, nil
	})

	result := engine.executePreparedEvictionsWithClient(context.Background(), run.DeepCopy(), 0, "example.com/accelerator", kubeClient)
	if result.Err != nil {
		t.Fatalf("executePreparedEvictionsWithClient: %v", result.Err)
	}
	if attempts != 5 {
		t.Fatalf("eviction attempts = %d, want 5", attempts)
	}
	persisted, err := engine.volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), "run", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	accepted, blocked := 0, 0
	for _, relocation := range persisted.Status.Relocations {
		switch relocation.Eviction.Phase {
		case repackv1alpha1.PodEvictionAccepted:
			accepted++
		case repackv1alpha1.PodEvictionInProgress:
			blocked++
		}
	}
	if accepted != 3 || blocked != 2 {
		t.Fatalf("accepted=%d blocked=%d, want 3/2", accepted, blocked)
	}
	if !hasProgressingReason(persisted, state.ReasonReconcilingPlacements) {
		t.Fatalf("conditions = %#v, want ReconcilingPlacements", persisted.Status.Conditions)
	}
}

// TestApplyEvictionWaveResultRefreshesPlacementDeadline: InProgress ->
// Accepted re-arms the replacement deadline.
func TestApplyEvictionWaveResultRefreshesPlacementDeadline(t *testing.T) {
	fixedNow := time.Now()
	engine := newWaveTestEngine(fixedNow)
	run := waveTestRun(repackv1alpha1.PodEvictionInProgress)
	old := run.Status.Relocations[0].Placement.ExpirationTime.Time

	result := newEvictionWaveResult()
	result.accepted = []int{0}
	if !engine.applyEvictionWaveResult(run, result) {
		t.Fatal("apply reported no change")
	}
	got := run.Status.Relocations[0].Placement.ExpirationTime.Time
	if !got.After(old) {
		t.Fatalf("placement deadline not refreshed: old=%v new=%v", old, got)
	}
	if run.Status.Relocations[0].Eviction.Phase != repackv1alpha1.PodEvictionAccepted {
		t.Fatalf("eviction phase = %q, want Accepted", run.Status.Relocations[0].Eviction.Phase)
	}
}

// TestMarkEvictionsRetryTimedOutRejectsRemaining: non-final victims become
// Rejected; committed ones are untouched.
func TestMarkEvictionsRetryTimedOutRejectsRemaining(t *testing.T) {
	fixedNow := time.Now()
	engine := newWaveTestEngine(fixedNow)
	run := waveTestRun(
		repackv1alpha1.PodEvictionInProgress,
		repackv1alpha1.PodEvictionAccepted,
	)
	engine.volcanoClient = vcfake.NewSimpleClientset(run.DeepCopy())
	if err := engine.markEvictionsRetryTimedOut(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if got := run.Status.Relocations[0].Eviction.Phase; got != repackv1alpha1.PodEvictionRejected {
		t.Fatalf("phase = %q, want Rejected", got)
	}
	if got := run.Status.Relocations[1].Eviction.Phase; got != repackv1alpha1.PodEvictionAccepted {
		t.Fatalf("committed phase clobbered = %q, want Accepted", got)
	}
}

// TestStopEvictionsOnPlacementTimeout: when a committed replacement timed
// out, unfinished evictions are rejected so the Run finalizes without expanding
// disturbance; committed relocations stay untouched.
func TestStopEvictionsOnPlacementTimeout(t *testing.T) {
	fixedNow := time.Now()
	engine := newWaveTestEngine(fixedNow)
	run := waveTestRun(
		repackv1alpha1.PodEvictionInProgress,
		repackv1alpha1.PodEvictionAccepted,
	)
	run.Status.Relocations[1].Placement.Phase = repackv1alpha1.PodPlacementTimedOut
	engine.volcanoClient = vcfake.NewSimpleClientset(run.DeepCopy())
	if err := engine.stopEvictionsOnPlacementTimeout(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if got := run.Status.Relocations[0].Eviction.Phase; got != repackv1alpha1.PodEvictionRejected {
		t.Fatalf("phase = %q, want Rejected", got)
	}
	if got := run.Status.Relocations[1].Eviction.Phase; got != repackv1alpha1.PodEvictionAccepted {
		t.Fatalf("committed phase clobbered = %q, want Accepted", got)
	}
	if got := run.Status.Relocations[1].Placement.Phase; got != repackv1alpha1.PodPlacementTimedOut {
		t.Fatalf("committed placement phase = %q, want TimedOut", got)
	}
}

// TestCommittedPlacementsCompleteSkipsBlocked: the accepted subset is
// complete even while a PDB-blocked relocation still waits for eviction.
func TestCommittedPlacementsCompleteSkipsBlocked(t *testing.T) {
	run := waveTestRun(
		repackv1alpha1.PodEvictionInProgress, // blocked: placement WaitingForReplacement
		repackv1alpha1.PodEvictionAccepted,   // committed: placed
	)
	run.Status.Relocations[1].Placement.Phase = repackv1alpha1.PodPlacementPlaced
	if !committedPlacementsComplete(run) {
		t.Fatal("committed subset should be complete while blocked relocation waits")
	}
	if hasAcceptedPlacementWork(run) {
		t.Fatal("placed committed relocation should not be pending placement work")
	}
}
