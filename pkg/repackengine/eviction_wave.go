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

// Execute drives eviction as a batch WAVE: one durable InProgress barrier, one
// batch of sequential Eviction API calls, one durable outcome write. Retryable
// results (PDB 429 / apiserver transient / network uncertainty) stay in
// PodEvictionInProgress and are retried with in-memory backoff; the Eviction API
// remains the final PDB arbiter and no plan/PDB is parsed. Replacement placement
// for the accepted subset always precedes the next wave so accepted Pods recover
// first and restore PDB allowance.
package repackengine

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	"volcano.sh/repack-controller/pkg/placement"
	state "volcano.sh/repack-controller/pkg/state"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

// evictionAttemptResult classifies a single Eviction API attempt. No new
// CRD enum is introduced: retryable attempts stay in PodEvictionInProgress and
// are re-collected by a later wave.
type evictionAttemptResult int

const (
	evictionAccepted evictionAttemptResult = iota
	evictionRetryable
	evictionPermanentFailure
	evictionVictimGone
)

// evictionWaveResult aggregates one wave's in-memory classification. It is
// persisted exactly once per wave (two status updates total: the InProgress
// barrier and the outcome; a stable-PDB wave costs zero writes).
type evictionWaveResult struct {
	accepted          []int // Eviction API returned nil
	recoveredAccepted []int // original pod gone/terminating after the durable intent
	retryable         []int // PDB 429 / transient / network uncertainty -> stay InProgress
	rejected          []int // permanent (403/422) -> Rejected
	victimGone        map[int]string
}

func newEvictionWaveResult() evictionWaveResult {
	return evictionWaveResult{victimGone: map[int]string{}}
}

// evictionRetryState is the in-memory per-relocation backoff. Never
// written to the CRD: a crash loses it, and the UID precondition + Eviction API
// make an immediate single re-attempt safe.
type evictionRetryState struct {
	attempts int
	nextTime time.Time
}

// evictionBackoffSequence is the fixed doubling schedule capped at 30s. Backoff
// is applied after a retryable result; the first attempt is not delayed.
var evictionBackoffSequence = []time.Duration{
	1 * time.Second, 2 * time.Second, 4 * time.Second,
	8 * time.Second, 16 * time.Second, 30 * time.Second,
}

const evictionBackoffJitterFraction = 0.2

// executeEvictionWave runs one batch of due evictions. It replaces the
// old per-Pod executePreparedEvictions loop.
func (e *Engine) executeEvictionWave(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	generation int64,
	targetResource v1.ResourceName,
) error {
	return e.executeEvictionWaveWithClient(
		ctx, run, generation, targetResource, e.schedulerCache.Client())
}

// executeEvictionWaveWithClient is the testable core; kubernetesClient is the
// Eviction API client.
func (e *Engine) executeEvictionWaveWithClient(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	generation int64,
	targetResource v1.ResourceName,
	kubernetesClient kubernetes.Interface,
) error {
	if run == nil || run.Status.Plan == nil {
		return fmt.Errorf("resume evictions: durable plan is missing")
	}
	evictionHook := hooksFor(run, kubernetesClient).Evict
	if evictionHook == nil {
		return e.fail(ctx, run, generation, state.ReasonEvictionFailed,
			fmt.Errorf("resume evictions: eviction hook is not configured"))
	}

	wave := e.collectEvictionWave(run)
	if len(wave) == 0 {
		// No victim is due: either everything is in backoff (schedule the next
		// retry) or the retry deadline has passed (handled by executeFinalize).
		e.scheduleEvictionRetry(run)
		return nil
	}
	klog.V(4).InfoS("repack: eviction wave collected",
		"run", run.Name, "waveSize", len(wave), "resource", targetResource)

	// First durable barrier: batch-advance Pending -> InProgress once.
	if e.markEvictionWaveInProgress(run, wave) {
		if err := e.updateStatus(ctx, run); err != nil {
			return fmt.Errorf("persist eviction wave InProgress barrier: %w", err)
		}
	}

	result := newEvictionWaveResult()
	for _, victim := range wave {
		e.executeWaveVictim(ctx, run, victim, evictionHook, kubernetesClient, &result)
	}

	if e.applyEvictionWaveResult(run, result) {
		if err := e.updateStatus(ctx, run); err != nil {
			return fmt.Errorf("persist eviction wave outcome: %w", err)
		}
	}

	acceptedTotal := len(result.accepted) + len(result.recoveredAccepted)
	e.observeWave(run, result, len(wave))
	// Aggregated block events: emit EvictionBlocked on the first wave that
	// hits a temporary refusal (partial or total) and EvictionUnblocked on the
	// first wave that clears it.
	if len(result.retryable) > 0 {
		if e.markEvictionBlocked(run.Name) {
			e.recordRunEvent(run, v1.EventTypeWarning, eventReasonEvictionBlocked,
				fmt.Sprintf("Eviction temporarily blocked for %d Pods (PDB or apiserver limit); retries will back off.", len(result.retryable)))
		}
	} else if e.clearEvictionBlocked(run.Name) {
		e.recordRunEvent(run, v1.EventTypeNormal, eventReasonEvictionUnblocked,
			"Eviction blocking cleared; retried Pods were accepted.")
	}
	switch {
	case acceptedTotal > 0:
		// Prioritize replacement placement before the next wave: accepted
		// Pods recover first, which restores PDB allowance for the retryable ones.
		message := placementProgressMessage(run, targetResource)
		state.MarkRunning(run, state.ReasonReconcilingPlacements, message)
		if err := e.updateStatus(ctx, run); err != nil {
			return err
		}
		e.recordRunEvent(run, v1.EventTypeNormal, eventReasonReconcilingPlacements,
			"Accepted replacements are being placed before the next eviction wave; PDB allowance is restored first.")
		e.recordRunEvent(run, v1.EventTypeNormal, eventReasonEvictionsIssued,
			fmt.Sprintf("Eviction API accepted %d Pods; awaiting replacement placement before the next eviction wave.", acceptedTotal))
		e.workQueue.Add(run.Name)
		return nil
	case len(result.retryable) > 0:
		// PDB (or apiserver) temporarily blocked the whole wave. Stay InProgress,
		// back off, and do NOT fail the run.
		e.scheduleEvictionRetry(run)
		return nil
	default:
		// No acceptance and nothing retryable: every attempted victim was
		// permanently rejected or already gone. Route to finalization so the Run
		// fails with a precise outcome instead of looping.
		e.workQueue.Add(run.Name)
		return nil
	}
}

// collectEvictionWave gathers the due victims of the next wave: unfinished
// evictions that are neither in backoff nor past the retry deadline.
func (e *Engine) collectEvictionWave(run *repackv1alpha1.RepackRun) []plannedVictim {
	if run == nil || run.Status.Plan == nil || e.retryDeadlinePassed(run) {
		return nil
	}
	now := e.now()
	var wave []plannedVictim
	for _, victim := range plannedVictims(run) {
		relocation := &run.Status.Relocations[victim.relocationIndex]
		if evictionOutcomeIsFinal(relocation.Eviction.Phase) {
			continue
		}
		if retryState, ok := e.evictionRetryState(run.Name, victim.relocationIndex); ok && now.Before(retryState.nextTime) {
			continue // in backoff
		}
		wave = append(wave, victim)
	}
	return wave
}

// markEvictionWaveInProgress batch-advances Pending -> InProgress for the wave.
// Already-InProgress retry items are not rewritten (no extra status update).
func (e *Engine) markEvictionWaveInProgress(run *repackv1alpha1.RepackRun, wave []plannedVictim) bool {
	changed := false
	for _, victim := range wave {
		relocation := &run.Status.Relocations[victim.relocationIndex]
		switch relocation.Eviction.Phase {
		case "", repackv1alpha1.PodEvictionPending:
			relocation.Eviction.Phase = repackv1alpha1.PodEvictionInProgress
			relocation.Eviction.Message = "Eviction intent is durable; the Eviction API request may now be issued."
			changed = true
		}
	}
	return changed
}

// executeWaveVictim observes the original Pod (UID precondition) and issues one
// Eviction API call, classifying the result in memory only.
func (e *Engine) executeWaveVictim(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	victim plannedVictim,
	evictionHook func(*engineapi.Move) error,
	kubernetesClient kubernetes.Interface,
	result *evictionWaveResult,
) {
	relocation := &run.Status.Relocations[victim.relocationIndex]
	pod, err := kubernetesClient.CoreV1().Pods(victim.namespace).Get(ctx, victim.podName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if relocation.Eviction.Phase == repackv1alpha1.PodEvictionInProgress {
			result.recoveredAccepted = append(result.recoveredAccepted, victim.relocationIndex)
		} else {
			result.victimGone[victim.relocationIndex] = "Victim Pod was absent before this eviction attempt."
		}
		return
	case err != nil:
		// A transient read error is itself retryable: keep InProgress and back off.
		result.retryable = append(result.retryable, victim.relocationIndex)
		return
	}

	originalInstanceGone := relocation.VictimPodUID != "" && pod.UID != relocation.VictimPodUID
	originalInstanceTerminating := pod.DeletionTimestamp != nil &&
		(relocation.VictimPodUID == "" || pod.UID == relocation.VictimPodUID)
	if originalInstanceGone || originalInstanceTerminating {
		if relocation.Eviction.Phase == repackv1alpha1.PodEvictionInProgress {
			result.recoveredAccepted = append(result.recoveredAccepted, victim.relocationIndex)
		} else {
			result.victimGone[victim.relocationIndex] =
				"The planned victim was already terminating or had been replaced before this eviction attempt."
		}
		return
	}

	if relocation.VictimPodUID == "" {
		relocation.VictimPodUID = pod.UID
	}
	task := schedapi.NewTaskInfo(pod.DeepCopy())
	task.Job = schedapi.JobID(victim.namespace + "/" + victim.podGroupName)
	move := &engineapi.Move{Task: task, From: victim.sourceNode, To: victim.targetNode}
	switch classifyEvictionError(evictionHook(move)) {
	case evictionAccepted:
		result.accepted = append(result.accepted, victim.relocationIndex)
	case evictionRetryable:
		result.retryable = append(result.retryable, victim.relocationIndex)
	case evictionPermanentFailure:
		result.rejected = append(result.rejected, victim.relocationIndex)
	case evictionVictimGone:
		result.victimGone[victim.relocationIndex] =
			"The Eviction API reported that the planned victim Pod was not found."
	}
}

// classifyEvictionError maps an Eviction API error to the internal result
// class. 429 is not assumed to come from PDB specifically — apiserver
// rate-limiting also backs off the same way.
func classifyEvictionError(err error) evictionAttemptResult {
	switch {
	case err == nil:
		return evictionAccepted
	case errors.Is(err, engineframework.ErrVictimNotFound), apierrors.IsNotFound(err):
		return evictionVictimGone
	case apierrors.IsTooManyRequests(err),
		apierrors.IsTimeout(err),
		apierrors.IsServerTimeout(err),
		apierrors.IsServiceUnavailable(err):
		return evictionRetryable
	case apierrors.IsForbidden(err),
		apierrors.IsInvalid(err):
		return evictionPermanentFailure
	default:
		// Transient / network-uncertain errors (the request may or may not have
		// reached the apiserver). Retrying is safe: the UID precondition and the
		// pre-attempt GET ensure a same-name replacement is never re-evicted.
		return evictionRetryable
	}
}

// applyEvictionWaveResult durably applies one wave's outcome to the in-memory
// journal and reports whether any persisted field changed (callers persist
// once). Retryable victims stay InProgress; their message is updated only on
// the first block so stable PDB refusal costs zero status writes.
func (e *Engine) applyEvictionWaveResult(run *repackv1alpha1.RepackRun, result evictionWaveResult) bool {
	changed := false
	now := e.now()

	for _, index := range result.accepted {
		relocation := &run.Status.Relocations[index]
		relocation.Eviction.Phase = repackv1alpha1.PodEvictionAccepted
		relocation.Eviction.Message = "Eviction API accepted the planned victim."
		e.refreshPlacementDeadline(relocation, now)
		e.clearEvictionRetry(run.Name, index)
		changed = true
	}
	for _, index := range result.recoveredAccepted {
		relocation := &run.Status.Relocations[index]
		relocation.Eviction.Phase = repackv1alpha1.PodEvictionAccepted
		relocation.Eviction.Message = "The original victim was terminating or gone after the durable eviction intent; treating the request as accepted during recovery."
		e.refreshPlacementDeadline(relocation, now)
		e.clearEvictionRetry(run.Name, index)
		changed = true
	}
	for _, index := range result.rejected {
		relocation := &run.Status.Relocations[index]
		relocation.Eviction.Phase = repackv1alpha1.PodEvictionRejected
		relocation.Eviction.Message = "Eviction permanently failed; the victim was not evicted."
		e.clearEvictionRetry(run.Name, index)
		changed = true
	}
	for _, index := range result.retryable {
		relocation := &run.Status.Relocations[index]
		e.recordEvictionRetry(run.Name, index)
		blockedMessage := "Eviction temporarily blocked; retry scheduled."
		if relocation.Eviction.Message != blockedMessage {
			relocation.Eviction.Message = blockedMessage
			changed = true
		}
	}
	for index := range result.victimGone {
		e.clearEvictionRetry(run.Name, index)
	}
	// Victims observed gone before an attempt: classify per PodGroup (indirect
	// removal vs rejected) with the same accepted-sibling rule as before.
	if classifyMissingVictims(run.Status.Relocations, result.victimGone) {
		changed = true
	}
	return changed
}

// refreshPlacementDeadline re-arms the replacement placement deadline when a
// victim transitions InProgress -> Accepted, so a long PDB wait cannot expire a
// freshly produced replacement.
func (e *Engine) refreshPlacementDeadline(relocation *repackv1alpha1.PodRelocationStatus, now time.Time) {
	if relocation == nil || e.config.NominationTTL <= 0 {
		return
	}
	t := metav1.NewTime(now.Add(e.config.NominationTTL))
	relocation.Placement.ExpirationTime = &t
}

// retryDeadlinePassed reports whether the whole-run eviction retry budget has
// been consumed (deadline = status.startTime + EvictionRetryTimeout).
func (e *Engine) retryDeadlinePassed(run *repackv1alpha1.RepackRun) bool {
	if run == nil || run.Status.StartTime == nil || e.config.EvictionRetryTimeout <= 0 {
		return false
	}
	return !e.now().Before(run.Status.StartTime.Time.Add(e.config.EvictionRetryTimeout))
}

// rejectUnfinishedEvictions marks every non-final victim as Rejected with the
// given operator message. Accepted replacements already in flight are untouched
// and complete normally.
func (e *Engine) rejectUnfinishedEvictions(ctx context.Context, run *repackv1alpha1.RepackRun, message string) error {
	changed := false
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		switch relocation.Eviction.Phase {
		case "", repackv1alpha1.PodEvictionPending, repackv1alpha1.PodEvictionInProgress:
			relocation.Eviction.Phase = repackv1alpha1.PodEvictionRejected
			relocation.Eviction.Message = message
			e.clearEvictionRetry(run.Name, index)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return e.updateStatus(ctx, run)
}

// markEvictionsRetryTimedOut rejects every non-final victim once the retry
// deadline has passed Accepted replacements already in flight are
// untouched and complete normally.
func (e *Engine) markEvictionsRetryTimedOut(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	e.recordRunEvent(run, v1.EventTypeWarning, eventReasonEvictionRetryTimedOut,
		"Eviction retry deadline exceeded; remaining victims were marked rejected.")
	return e.rejectUnfinishedEvictions(ctx, run,
		"Eviction retry deadline exceeded; the victim was not evicted.")
}

// stopEvictionsOnPlacementTimeout rejects unfinished evictions when a committed
// replacement timed out: no further eviction is issued so the Run can
// finalize without expanding disturbance.
func (e *Engine) stopEvictionsOnPlacementTimeout(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	return e.rejectUnfinishedEvictions(ctx, run,
		"Eviction stopped: a replacement placement timed out; no further evictions were issued.")
}

// recordEvictionRetry bumps the in-memory backoff for one relocation:
// 1s -> 2s -> 4s -> 8s -> 16s -> 30s with ±20% jitter.
func (e *Engine) recordEvictionRetry(runName string, relocationIndex int) {
	states := e.evictionRetryStatesFor(runName)
	retryState := states[relocationIndex]
	if retryState == nil {
		retryState = &evictionRetryState{}
		states[relocationIndex] = retryState
	}
	retryState.attempts++
	base := evictionBackoffSequence[len(evictionBackoffSequence)-1]
	if retryState.attempts-1 < len(evictionBackoffSequence) {
		base = evictionBackoffSequence[retryState.attempts-1]
	}
	jitter := time.Duration((rand.Float64()*2 - 1) * evictionBackoffJitterFraction * float64(base))
	retryState.nextTime = e.now().Add(base + jitter)
	metrics.ObserveEvictionRetryDelay(base.Seconds())
}

// nextDueEviction returns the delay until the next retryable victim is due.
// Any victim due right now (or without backoff state, e.g. after a restart)
// returns (0, true).
func (e *Engine) nextDueEviction(run *repackv1alpha1.RepackRun) (time.Duration, bool) {
	if run == nil || run.Status.Plan == nil {
		return 0, false
	}
	now := e.now()
	earliest := time.Duration(0)
	found := false
	for _, victim := range plannedVictims(run) {
		relocation := &run.Status.Relocations[victim.relocationIndex]
		if evictionOutcomeIsFinal(relocation.Eviction.Phase) {
			continue
		}
		if retryState, ok := e.evictionRetryState(run.Name, victim.relocationIndex); ok && now.Before(retryState.nextTime) {
			delay := retryState.nextTime.Sub(now)
			if !found || delay < earliest {
				earliest = delay
			}
			found = true
			continue
		}
		return 0, true // due now
	}
	return earliest, found
}

// scheduleEvictionRetry requeues the run at the earliest pending retry. The
// workqueue is used instead of sleep so the single worker stays responsive.
func (e *Engine) scheduleEvictionRetry(run *repackv1alpha1.RepackRun) {
	if run == nil {
		return
	}
	if delay, ok := e.nextDueEviction(run); ok {
		if delay > 0 {
			klog.V(4).InfoS("repack: eviction retry scheduled",
				"run", run.Name, "retryAfter", delay.Round(time.Millisecond))
			e.workQueue.AddAfter(run.Name, delay)
		} else {
			e.workQueue.Add(run.Name)
		}
	}
}

// observeWave records the wave-level metrics and aggregated log line.
func (e *Engine) observeWave(run *repackv1alpha1.RepackRun, result evictionWaveResult, waveSize int) {
	outcome := "complete"
	switch {
	case len(result.retryable) > 0 && len(result.accepted)+len(result.recoveredAccepted) > 0:
		outcome = "partial"
	case len(result.retryable) > 0:
		outcome = "blocked"
	}
	metrics.ObserveEvictionWave(outcome)
	for range result.accepted {
		metrics.ObserveEvictionAttempt("accepted")
	}
	for range result.recoveredAccepted {
		metrics.ObserveEvictionAttempt("accepted")
	}
	for range result.retryable {
		metrics.ObserveEvictionAttempt("too_many_requests")
	}
	for range result.rejected {
		metrics.ObserveEvictionAttempt("permanent_error")
	}
	for range result.victimGone {
		metrics.ObserveEvictionAttempt("victim_gone")
	}
	klog.V(3).InfoS("repack: eviction wave completed",
		"run", run.Name, "waveSize", waveSize,
		"accepted", len(result.accepted)+len(result.recoveredAccepted),
		"retryable", len(result.retryable),
		"rejected", len(result.rejected),
		"victimGone", len(result.victimGone),
		"outcome", outcome)
}

// ---------- pure state helpers (shared by the engine and tests) ----------

// executeAction is the single Execute-state decision: replacement
// placement always precedes further eviction.
type executeAction int

const (
	executePlacement executeAction = iota
	executeEviction
	executeWait
	executeFinalize
)

// nextExecuteAction decides what the single Execute worker does next. Placement
// work (accepted-but-unfinished replacements) wins over eviction; a timed-out
// committed placement stops any further eviction; a retryable victim that
// is not yet due yields executeWait with the precise retry delay.
func (e *Engine) nextExecuteAction(run *repackv1alpha1.RepackRun) (executeAction, time.Duration) {
	switch {
	case committedPlacementTimedOut(run):
		return executeFinalize, 0
	case hasAcceptedPlacementWork(run):
		return executePlacement, 0
	case hasUnfinishedEvictions(run):
		if e.retryDeadlinePassed(run) {
			return executeFinalize, 0
		}
		if retryAfter, any := e.nextDueEviction(run); any && retryAfter > 0 {
			return executeWait, retryAfter
		}
		return executeEviction, 0
	default:
		return executeFinalize, 0
	}
}

// hasUnfinishedEvictions reports whether any relocation still needs an eviction
// attempt (Pending, InProgress, or never started).
func hasUnfinishedEvictions(run *repackv1alpha1.RepackRun) bool {
	for index := range run.Status.Relocations {
		switch run.Status.Relocations[index].Eviction.Phase {
		case "", repackv1alpha1.PodEvictionPending, repackv1alpha1.PodEvictionInProgress:
			return true
		}
	}
	return false
}

// hasCommittedRelocations reports whether any relocation's eviction was
// accepted (Accepted or IndirectlyRemoved).
func hasCommittedRelocations(run *repackv1alpha1.RepackRun) bool {
	for index := range run.Status.Relocations {
		if placement.EvictionAllowsPlacement(&run.Status.Relocations[index]) {
			return true
		}
	}
	return false
}

// hasAcceptedPlacementWork reports whether a committed relocation still has
// unfinished replacement placement (WaitingForReplacement / WaitingForNodeSelection
// / Nominated).
func hasAcceptedPlacementWork(run *repackv1alpha1.RepackRun) bool {
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if !placement.EvictionAllowsPlacement(relocation) {
			continue
		}
		switch relocation.Placement.Phase {
		case repackv1alpha1.PodPlacementPlaced, repackv1alpha1.PodPlacementTimedOut:
		default:
			return true
		}
	}
	return false
}

// committedPlacementTimedOut reports whether any committed replacement timed out.
func committedPlacementTimedOut(run *repackv1alpha1.RepackRun) bool {
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if !placement.EvictionAllowsPlacement(relocation) {
			continue
		}
		if relocation.Placement.Phase == repackv1alpha1.PodPlacementTimedOut {
			return true
		}
	}
	return false
}

// committedPlacementsComplete reports whether every committed relocation
// (Accepted/IndirectlyRemoved) has finished placement. PDB-blocked victims
// (Eviction=InProgress, Placement=WaitingForReplacement) are deliberately
// excluded so they cannot block the accepted subset.
func committedPlacementsComplete(run *repackv1alpha1.RepackRun) bool {
	found := false
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if !placement.EvictionAllowsPlacement(relocation) {
			continue
		}
		found = true
		switch relocation.Placement.Phase {
		case repackv1alpha1.PodPlacementPlaced, repackv1alpha1.PodPlacementTimedOut:
		default:
			return false
		}
	}
	return found
}

// ---------- in-memory retry/block state accessors ----------

func (e *Engine) evictionRetryStatesFor(runName string) map[int]*evictionRetryState {
	if e.evictionRetryStates == nil {
		e.evictionRetryStates = make(map[string]map[int]*evictionRetryState)
	}
	states := e.evictionRetryStates[runName]
	if states == nil {
		states = make(map[int]*evictionRetryState)
		e.evictionRetryStates[runName] = states
	}
	return states
}

func (e *Engine) evictionRetryState(runName string, relocationIndex int) (*evictionRetryState, bool) {
	states := e.evictionRetryStates[runName]
	if states == nil {
		return nil, false
	}
	retryState, ok := states[relocationIndex]
	return retryState, ok
}

func (e *Engine) clearEvictionRetry(runName string, relocationIndex int) {
	if states := e.evictionRetryStates[runName]; states != nil {
		delete(states, relocationIndex)
	}
}

// markEvictionBlocked records the first temporary block so the event is emitted
// once. Returns true on the first transition into the blocked state.
func (e *Engine) markEvictionBlocked(runName string) bool {
	if e.evictionBlocked == nil {
		e.evictionBlocked = make(map[string]bool)
	}
	if e.evictionBlocked[runName] {
		return false
	}
	e.evictionBlocked[runName] = true
	return true
}

// clearEvictionBlocked returns true on the first transition out of a block.
func (e *Engine) clearEvictionBlocked(runName string) bool {
	if e.evictionBlocked == nil {
		return false
	}
	if !e.evictionBlocked[runName] {
		return false
	}
	delete(e.evictionBlocked, runName)
	return true
}
