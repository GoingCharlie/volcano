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

// Package networktopologyaware turns RepackRun.spec.networkTopology into
// HyperNode-block shaping: two plan-score terms (node-block progress, node-block
// distribution), one hard block-count constraint, and one receiver preference
// (nodeBlockPreserve) steering relocated pods away from the target tier's
// HyperNodes (no-H > other HyperNode > own HyperNode).
//
// It contributes no freeable unit of its own — it reuses nodeconsolidation's
// single-node unit — so all scoring anchors read IncrementalFromNodes()[0] and
// count this candidate as +1. Multi-node units would invalidate that accounting;
// units must never span HyperNodes. With networkTopology unset it registers
// nothing and the engine runs unchanged.
//
// Activation — free two blocks of 4 nodes in the tier named "accel", spreading
// them across HyperNodes:
//
//	apiVersion: repack.volcano.sh/v1alpha1
//	kind: RepackRun
//	spec:
//	  mode: Execute
//	  networkTopology:
//	    hyperNodeTierName: "accel"
//	    nodeBlockSize: 4
//	    requiredNodeBlocks: 2
//	    mode: spread
//	  goals:
//	    - resource: nvidia.com/gpu
//
// Enabled by default in repack-engine.conf; its two weights are tunable there
// (zero disables the term):
//
//	plugins:
//	- name: networktopologyaware
//	  arguments:
//	    nodeBlockProgressWeight: 1000000
//	    nodeBlockDistributionWeight: 100
package networktopologyaware

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/volcano/pkg/controllers/repack/state"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Name is the config name for this plugin.
const Name = "networktopologyaware"

// Default weights: progress dominates ordering, distribution breaks equal-progress ties.
const (
	weightNodeBlockProgress     int64 = 1000000
	weightNodeBlockDistribution int64 = 100

	argNodeBlockProgressWeight     = "nodeBlockProgressWeight"
	argNodeBlockDistributionWeight = "nodeBlockDistributionWeight"
)

func init() {
	framework.RegisterPlugin(Name, framework.PluginRegistration{
		Factory:   newPlugin,
		Validator: validateArguments,
		// Reuses the consolidation domain's single-node units instead of adding its own.
		Requires: []framework.PluginCapability{framework.CapabilityDomain},
	})
}

type networkTopologyAwarePlugin struct {
	progressWeight     int64
	distributionWeight int64
}

func newPlugin(arguments framework.Arguments) framework.Plugin {
	return &networkTopologyAwarePlugin{
		progressWeight:     configuredWeight(arguments, argNodeBlockProgressWeight, weightNodeBlockProgress),
		distributionWeight: configuredWeight(arguments, argNodeBlockDistributionWeight, weightNodeBlockDistribution),
	}
}

func configuredWeight(arguments framework.Arguments, key string, defaultValue int64) int64 {
	value, err := arguments.NonNegativeInt(key, defaultValue)
	if err != nil {
		return defaultValue
	}
	return value
}

// validateArguments rejects unknown keys; both weights must be non-negative.
func validateArguments(arguments framework.Arguments) error {
	if err := arguments.ValidateKeys(argNodeBlockProgressWeight, argNodeBlockDistributionWeight); err != nil {
		return err
	}
	for _, item := range []struct {
		key          string
		defaultValue int64
	}{
		{argNodeBlockProgressWeight, weightNodeBlockProgress},
		{argNodeBlockDistributionWeight, weightNodeBlockDistribution},
	} {
		if _, err := arguments.NonNegativeInt(item.key, item.defaultValue); err != nil {
			return err
		}
	}
	return nil
}

func (*networkTopologyAwarePlugin) Name() string { return Name }

// nodeBlockSession holds the per-session topology precompute shared by the
// callbacks. It is built once in OnSessionOpen and must not change during the pass.
type nodeBlockSession struct {
	targetTier int
	// size is nodeBlockSize, clamped to >= 1.
	size int
	// requiredBlocks is the hard admission floor (requiredNodeBlocks).
	requiredBlocks int
	// mode is the block distribution preference ("" when unset).
	mode             repackv1alpha1.RepackBlockMode
	hyperNodesInTier []string
	// nodeToHyperNode maps each real node at targetTier to its HyperNode (at most
	// one); nodes outside the tier are absent.
	nodeToHyperNode map[string]string
	// idleInHyperNode / busyInHyperNode count Empty (zero target-resource usage) and
	// Partial nodes per HyperNode; Unavailable/Full are excluded on purpose.
	idleInHyperNode map[string]int
	busyInHyperNode map[string]int
	// maxBlocksInHyperNode is the tier max of floor((idle+busy)/size): spread mode's
	// least-preferred raw score for nodes outside any HyperNode.
	maxBlocksInHyperNode int
}

func (p *networkTopologyAwarePlugin) OnSessionOpen(ssn *framework.Session) {
	run := ssn.Run()
	runName := ""
	if run != nil {
		runName = run.Name
	}
	if run == nil || run.Spec.NetworkTopology == nil {
		klog.V(4).InfoS("repack networktopologyaware: networkTopology unset, plugin inactive", "run", runName)
		return
	}
	blockSession, ok := buildNodeBlockSession(ssn, run.Spec.NetworkTopology)
	if !ok {
		// Nothing to plan against, but warn: the user did configure networkTopology.
		topology := run.Spec.NetworkTopology
		klog.Warningf("repack networktopologyaware: target tier unresolvable (tier=%s tierName=%s), block shaping inactive; run=%s",
			tierString(topology), tierNameString(topology), runName)
		return
	}
	klog.V(4).InfoS("repack networktopologyaware: block shaping enabled",
		"run", runName, "tier", blockSession.targetTier, "blockSize", blockSession.size,
		"requiredBlocks", blockSession.requiredBlocks, "mode", blockSession.mode,
		"hyperNodeCount", len(blockSession.hyperNodesInTier))
	p.registerNodeBlockProgressScore(ssn, blockSession)
	if blockSession.mode == repackv1alpha1.RepackBlockModeBinpack || blockSession.mode == repackv1alpha1.RepackBlockModeSpread {
		p.registerNodeBlockDistributionScore(ssn, blockSession) // binpack/spread only
	}
	p.registerBlockCountConstraint(ssn, blockSession)
	p.registerNodeBlockReceiverPreference(ssn, blockSession)
}

// tierString / tierNameString render the pointer tier identifiers for logs
// without allocating when nil.
func tierString(topology *repackv1alpha1.NetworkTopology) string {
	if topology.HyperNodeTier == nil {
		return "<unset>"
	}
	return fmt.Sprintf("%d", *topology.HyperNodeTier)
}

func tierNameString(topology *repackv1alpha1.NetworkTopology) string {
	if topology.HyperNodeTierName == nil {
		return "<unset>"
	}
	return *topology.HyperNodeTierName
}

// buildNodeBlockSession resolves the target tier, the node->HyperNode index and
// the session-start idle/busy counts. ok=false when the tier holds no HyperNode.
func buildNodeBlockSession(ssn *framework.Session, topology *repackv1alpha1.NetworkTopology) (*nodeBlockSession, bool) {
	snapshot := ssn.Snapshot()
	targetTier, ok := resolveTargetTier(snapshot, topology)
	if !ok {
		return nil, false
	}
	// The apiserver defaults nodeBlockSize to 1 and never below; defend nil/<1
	// anyway, since direct informer and test inputs bypass it.
	size := 1
	if topology.NodeBlockSize != nil {
		size = *topology.NodeBlockSize
	}
	if size < 1 {
		size = 1
	}
	blockSession := &nodeBlockSession{
		targetTier:      targetTier,
		size:            size,
		requiredBlocks:  topology.RequiredNodeBlocks,
		mode:            topology.Mode,
		nodeToHyperNode: make(map[string]string),
		idleInHyperNode: make(map[string]int),
		busyInHyperNode: make(map[string]int),
	}
	if blockSession.requiredBlocks < 0 {
		blockSession.requiredBlocks = 0 // defensive: the CRD already guarantees non-negative
	}

	hyperNodesByTier := snapshot.HyperNodesSetByTier()
	realNodesSet := snapshot.RealNodesSet()
	blockSession.hyperNodesInTier = sets.List(hyperNodesByTier[targetTier])
	if len(blockSession.hyperNodesInTier) == 0 {
		return nil, false
	}

	// On overlap keep the first hit, warn, and never count a node twice.
	for _, hyperNode := range blockSession.hyperNodesInTier {
		for node := range realNodesSet[hyperNode] {
			if existing, taken := blockSession.nodeToHyperNode[node]; taken && existing != hyperNode {
				klog.Warningf("HyperNode-aware repack: node %s belongs to both %s and %s at tier %d; keeping %s", node, existing, hyperNode, targetTier, existing)
				continue
			}
			blockSession.nodeToHyperNode[node] = hyperNode
		}
	}

	// ClassifyTargetResourceNode keeps the empty/freeable split identical to
	// nodeconsolidation; Unavailable and Full nodes count for neither.
	resource := ssn.Resource()
	// Index by name once so classification is O(T), not O(T x C).
	nodeByName := make(map[string]*schedapi.NodeInfo, len(snapshot.Nodes()))
	for _, n := range snapshot.Nodes() {
		if n != nil && n.Name != "" {
			nodeByName[n.Name] = n
		}
	}
	for nodeName, hyperNode := range blockSession.nodeToHyperNode {
		nodeInfo := nodeByName[nodeName]
		switch api.ClassifyTargetResourceNode(nodeInfo, resource) {
		case api.TargetResourceNodeEmpty:
			blockSession.idleInHyperNode[hyperNode]++
		case api.TargetResourceNodePartial:
			blockSession.busyInHyperNode[hyperNode]++
		}
	}
	for _, hyperNode := range blockSession.hyperNodesInTier {
		if blocks := (blockSession.idleInHyperNode[hyperNode] + blockSession.busyInHyperNode[hyperNode]) / blockSession.size; blocks > blockSession.maxBlocksInHyperNode {
			blockSession.maxBlocksInHyperNode = blocks
		}
	}
	runName := ""
	if run := ssn.Run(); run != nil {
		runName = run.Name
	}
	klog.V(5).InfoS("repack networktopologyaware: tier block session built",
		"run", runName, "tier", blockSession.targetTier, "blockSize", blockSession.size,
		"requiredBlocks", blockSession.requiredBlocks, "mode", blockSession.mode,
		"hyperNodes", blockSession.hyperNodesInTier, "nodeCount", len(blockSession.nodeToHyperNode),
		"idleInHyperNode", blockSession.idleInHyperNode, "busyInHyperNode", blockSession.busyInHyperNode, "maxBlocksInHyperNode", blockSession.maxBlocksInHyperNode)
	return blockSession, true
}

// resolveTargetTier maps the run's tier identifier to a numeric tier.
func resolveTargetTier(snapshot framework.Snapshot, topology *repackv1alpha1.NetworkTopology) (int, bool) {
	if topology.HyperNodeTier != nil {
		return *topology.HyperNodeTier, true
	}
	if topology.HyperNodeTierName != nil {
		tier, ok := snapshot.HyperNodeTierNameMap()[*topology.HyperNodeTierName]
		return tier, ok
	}
	return 0, false
}

// freedInHyperNode counts the plan's freed nodes inside hyperNode; nodes elsewhere count 0.
func freedInHyperNode(freedNodes []string, hyperNode string, nodeToHyperNode map[string]string) int {
	count := 0
	for _, node := range freedNodes {
		if nodeToHyperNode[node] == hyperNode {
			count++
		}
	}
	return count
}

// blockReachable is the one reachability test shared by both block terms: can the
// anchor HyperNode reach a complete block once this plan's frees land?
func blockReachable(freeInHyperNode, freeableInHyperNode, size int) bool {
	if size < 1 {
		size = 1
	}
	r := freeInHyperNode % size
	return r == 0 || freeableInHyperNode >= size-r
}

// nodeBlockProgressScore scores the anchor HyperNode's block progress once this
// candidate's frees land: freeInHyperNode = idle + plan-freed, freeableInHyperNode
// = busy - plan-freed.
func nodeBlockProgressScore(freeInHyperNode, freeableInHyperNode, size int) int64 {
	if size < 1 {
		size = 1
	}
	r := freeInHyperNode % size
	switch {
	case r == 0:
		return int64(size) // candidate completes a block
	case !blockReachable(freeInHyperNode, freeableInHyperNode, size):
		return 0 // a complete block is unreachable
	default:
		return int64(r) // block reachable: closer to full wins
	}
}

// nodeBlockDistributionScore: binpack prefers more blocks (concentrate), spread fewer.
func nodeBlockDistributionScore(mode repackv1alpha1.RepackBlockMode, blocks int) int64 {
	switch mode {
	case repackv1alpha1.RepackBlockModeBinpack:
		return int64(blocks)
	case repackv1alpha1.RepackBlockModeSpread:
		return -int64(blocks)
	}
	return 0
}

// nodeBlockDistributionFloor is the raw score for an anchor that is no block
// candidate: outside every HyperNode, or inside one already at its block ceiling,
// where draining every remaining node cannot fill a block. Neither says where a
// block should go, so both take one below the mode's real minimum.
func nodeBlockDistributionFloor(mode repackv1alpha1.RepackBlockMode, maxBlocksInHyperNode int) int64 {
	switch mode {
	case repackv1alpha1.RepackBlockModeBinpack:
		return -1 // one below the real minimum raw 0 (zero-block H)
	case repackv1alpha1.RepackBlockModeSpread:
		return -int64(maxBlocksInHyperNode) - 1 // one below the real minimum raw -maxBlocksInHyperNode
	}
	return 0
}

// totalBlocksInTier sums floor((idle + freed) / size) over the tier's HyperNodes.
func totalBlocksInTier(idleInHyperNode, freedByHyperNode map[string]int, hyperNodesInTier []string, size int) int {
	if size < 1 {
		size = 1
	}
	total := 0
	for _, hyperNode := range hyperNodesInTier {
		total += (idleInHyperNode[hyperNode] + freedByHyperNode[hyperNode]) / size
	}
	return total
}

func (p *networkTopologyAwarePlugin) registerNodeBlockProgressScore(ssn *framework.Session, blockSession *nodeBlockSession) {
	ssn.AddPlanScoreFn("nodeBlockProgress", p.progressWeight, func(_ *api.PlanContext, plan *api.CandidatePlan) int64 {
		anchor := plan.IncrementalFromNodes()
		if len(anchor) == 0 {
			return 0
		}
		// Single-node unit: the anchor is the unique node this candidate frees.
		hyperNode, ok := blockSession.nodeToHyperNode[anchor[0]]
		if !ok {
			return 0 // no HyperNode: least preferred
		}
		freedCount := freedInHyperNode(plan.FreedNodes(), hyperNode, blockSession.nodeToHyperNode)
		return nodeBlockProgressScore(
			blockSession.idleInHyperNode[hyperNode]+freedCount, // idle + plan-freed, counting this candidate
			blockSession.busyInHyperNode[hyperNode]-freedCount, // busy - plan-freed: what is left to drain
			blockSession.size,
		)
	})
}

func (p *networkTopologyAwarePlugin) registerNodeBlockDistributionScore(ssn *framework.Session, blockSession *nodeBlockSession) {
	ssn.AddPlanScoreFn("nodeBlockDistribution", p.distributionWeight, func(_ *api.PlanContext, plan *api.CandidatePlan) int64 {
		anchor := plan.IncrementalFromNodes()
		if len(anchor) == 0 {
			return 0
		}
		hyperNode, ok := blockSession.nodeToHyperNode[anchor[0]]
		if !ok {
			// No H: no block to account for.
			return nodeBlockDistributionFloor(blockSession.mode, blockSession.maxBlocksInHyperNode)
		}
		freedCount := freedInHyperNode(plan.FreedNodes(), hyperNode, blockSession.nodeToHyperNode)
		freeInHyperNode := blockSession.idleInHyperNode[hyperNode] + freedCount
		if !blockReachable(freeInHyperNode, blockSession.busyInHyperNode[hyperNode]-freedCount, blockSession.size) {
			// At its block ceiling: same floor as a no-H anchor.
			return nodeBlockDistributionFloor(blockSession.mode, blockSession.maxBlocksInHyperNode)
		}
		return nodeBlockDistributionScore(blockSession.mode, freeInHyperNode/blockSession.size)
	})
}

func (p *networkTopologyAwarePlugin) registerBlockCountConstraint(ssn *framework.Session, blockSession *nodeBlockSession) {
	runName := ""
	if run := ssn.Run(); run != nil {
		runName = run.Name
	}
	ssn.AddConstraintFn(func(_ *api.PlanContext, plan *api.RepackPlan) (bool, string) {
		// requiredBlocks==0 (default): pure soft guidance, always admit.
		if blockSession.requiredBlocks == 0 {
			return true, ""
		}
		if plan == nil {
			return false, ""
		}
		freedByHyperNode := make(map[string]int, len(blockSession.hyperNodesInTier))
		for _, node := range plan.FreedNodes {
			if hyperNode, ok := blockSession.nodeToHyperNode[node]; ok {
				freedByHyperNode[hyperNode]++
			}
		}
		total := totalBlocksInTier(blockSession.idleInHyperNode, freedByHyperNode, blockSession.hyperNodesInTier, blockSession.size)
		admitted := total >= blockSession.requiredBlocks
		klog.V(4).InfoS("repack networktopologyaware: block-count gate", "run", runName,
			"requiredBlocks", blockSession.requiredBlocks, "blockSize", blockSession.size,
			"freedNodeCount", len(plan.FreedNodes), "freedByHyperNode", freedByHyperNode,
			"completeBlocks", total, "admitted", admitted)
		if admitted {
			return true, ""
		}
		// Report the block-specific reason, not the fragmentation-improvement one.
		return false, state.ReasonRequiredNodeBlocksNotMet
	})
}

// registerNodeBlockReceiverPreference steers relocated pods away from the target
// tier's HyperNodes: no-HyperNode ({3}) > another HyperNode ({2}) > own HyperNode
// ({1}), abstaining ({}) when the candidate frees no node.
//
// It only reorders receivers — firstFeasibleReceiver always takes the first feasible
// one — so it cannot make a plan infeasible, and the block-count gate (which counts
// freed nodes, not destinations) is unaffected. Registering in the Topology phase
// keeps the stability policies ahead of this key; filling those never hurts the
// block pool, since they could not be drained anyway.
func (p *networkTopologyAwarePlugin) registerNodeBlockReceiverPreference(ssn *framework.Session, blockSession *nodeBlockSession) {
	ssn.AddReceiverPreferenceFn("nodeBlockPreserve", framework.ReceiverPreferencePhaseTopology,
		func(_ *api.PlanContext, candidate *framework.PlanningCandidate, receiver *framework.ReceiverCandidate) framework.ReceiverPreference {
			anchors := candidate.Plan.IncrementalFromNodes()
			if len(anchors) == 0 {
				return framework.ReceiverPreference{} // no anchor: abstain
			}
			// Set form kept for generality; a single-node unit collapses this to one.
			ownHs := make(map[string]bool, len(anchors))
			for _, n := range anchors {
				if h, ok := blockSession.nodeToHyperNode[n]; ok {
					ownHs[h] = true
				}
			}
			receiverHyperNode, inTier := blockSession.nodeToHyperNode[receiver.Node.Name]
			switch {
			case !inTier:
				return framework.ReceiverPreference{3} // export the load outside the tier
			case ownHs[receiverHyperNode]:
				return framework.ReceiverPreference{1}
			default:
				return framework.ReceiverPreference{2}
			}
		})
}

func (*networkTopologyAwarePlugin) OnSessionClose(*framework.Session) {}
