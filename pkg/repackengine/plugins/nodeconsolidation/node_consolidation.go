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

// Package nodeconsolidation is the node-level defragmentation domain plugin: the freeable unit
// is a single node (weight 1). Enable it in repack-conf's plugins list for
// "free whole nodes" repack. A future hypernode plugin can contribute larger
// units through the same Domain extension point.
//
// It also contributes the victim-order key for that product: the pod with the
// fewest schedulable nodes is simulated first, so a victim only a few nodes can
// take is tried while receiver capacity is still intact. Only the static
// pod-to-node factors are counted; leftover capacity and the constraints that
// depend on other pods (inter-pod affinity, topology spread, hostPorts) stay out
// of scope for the simulation to decide.
//
// Those factors are necessary, not sufficient, so the count is an upper bound on
// the true one: a Pod that merely looks unconstrained is simulated later than it
// deserves. That costs ordering quality, not correctness.
package nodeconsolidation

import (
	"cmp"

	v1 "k8s.io/api/core/v1"
	corev1helper "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Name is the config name for this plugin.
const Name = "nodeconsolidation"

// Each node factor decides whether one node admits a pod. All default to true;
// disabling one only raises every victim's count.
const (
	argNodeAffinity = "nodeAffinity"
	argTaints       = "taints"
	argCordon       = "cordon"
)

// argKeys is every accepted argument, so key validation and type validation
// cannot drift apart.
var argKeys = []string{argNodeAffinity, argTaints, argCordon}

// TaintTolerationComparisonOperators is alpha and off upstream. Hardcoding the
// default is safe: if a cluster enables it, a comparison-operator toleration
// stops counting here and only undercounts; the reverse would overcount.
const taintComparisonOperatorsEnabled = false

func init() {
	framework.RegisterPlugin(Name, framework.PluginRegistration{
		Factory:   newPlugin,
		Validator: validateArguments,
		Provides:  []framework.PluginCapability{framework.CapabilityDomain},
	})
}

type nodeConsolidationPlugin struct {
	countNodeAffinity bool
	countTaints       bool
	countCordon       bool

	nodes []*schedapi.NodeInfo
	// counts memoizes per victim object, not per UID: a clone keeps its UID but may
	// carry a different source node. Ordering is single-goroutine, so no lock.
	counts map[*schedapi.TaskInfo]allowedCount
}

// allowedCount is the memoized per-victim result. known is false when the task
// carries nothing to evaluate, so the count key abstains rather than reading "no
// information" as "most constrained".
type allowedCount struct {
	count int
	known bool
}

func newPlugin(arguments framework.Arguments) framework.Plugin {
	return &nodeConsolidationPlugin{
		countNodeAffinity: configuredBool(arguments, argNodeAffinity),
		countTaints:       configuredBool(arguments, argTaints),
		countCordon:       configuredBool(arguments, argCordon),
		counts:            map[*schedapi.TaskInfo]allowedCount{},
	}
}

func configuredBool(arguments framework.Arguments, key string) bool {
	value, err := arguments.Bool(key, true)
	if err != nil {
		return true
	}
	return value
}

func validateArguments(arguments framework.Arguments) error {
	if err := arguments.ValidateKeys(argKeys...); err != nil {
		return err
	}
	for _, key := range argKeys {
		if _, err := arguments.Bool(key, true); err != nil {
			return err
		}
	}
	return nil
}

func (*nodeConsolidationPlugin) Name() string { return Name }

func (p *nodeConsolidationPlugin) OnSessionOpen(ssn *framework.Session) {
	resourceName := ssn.Resource()
	ssn.AddDomainFn(func(snapshot framework.Snapshot) []api.FreeableUnit {
		nodes := snapshot.Nodes()
		out := make([]api.FreeableUnit, 0, len(nodes))
		for _, n := range nodes {
			if n == nil || !snapshot.NodeInScope(n) {
				continue // scope.nodes gates drain targets; out-of-scope = receiver only
			}
			// Empty target-resource nodes must remain idle, while fully occupied
			// nodes are already compact. Only fragmented, partially occupied nodes
			// are meaningful node-consolidation drain targets.
			if api.ClassifyTargetResourceNode(n, resourceName) != api.TargetResourceNodePartial {
				continue
			}
			out = append(out, api.FreeableUnit{Level: "node", Nodes: []string{n.Name}, Weight: 1})
		}
		return out
	})

	// A session opened only to validate plugin composition carries no snapshot, and
	// this plugin opens in that path now that it provides the mandatory Domain.
	// With no nodes the count key abstains.
	if snapshot := ssn.Snapshot(); snapshot != nil {
		p.nodes = snapshot.Nodes()
	}
	ssn.AddVictimOrderFn(Name, p.compare)
}

func (*nodeConsolidationPlugin) OnSessionClose(*framework.Session) {}

// compare orders victims by allowed-receiver count ascending. It abstains, leaving
// the pair to the next registered plugin and the framework's UID tie-break, when
// every node factor is disabled or either side cannot be evaluated.
func (p *nodeConsolidationPlugin) compare(left, right *schedapi.TaskInfo) int {
	if !p.countNodeAffinity && !p.countTaints && !p.countCordon {
		return 0
	}
	leftCount := p.count(left)
	rightCount := p.count(right)
	if !leftCount.known || !rightCount.known {
		return 0
	}
	return cmp.Compare(leftCount.count, rightCount.count)
}

// count returns the memoized allowed-receiver count for a task.
func (p *nodeConsolidationPlugin) count(task *schedapi.TaskInfo) allowedCount {
	if task == nil {
		return allowedCount{}
	}
	if cached, ok := p.counts[task]; ok {
		return cached
	}
	result := p.evaluate(task)
	p.counts[task] = result
	return result
}

// evaluate counts the nodes outside the task's current node that admit its pod;
// the drained node is never a receiver.
func (p *nodeConsolidationPlugin) evaluate(task *schedapi.TaskInfo) allowedCount {
	if task.Pod == nil || len(p.nodes) == 0 {
		return allowedCount{}
	}
	required := nodeaffinity.GetRequiredNodeAffinity(task.Pod)
	count := 0
	for _, node := range p.nodes {
		if node == nil || node.Node == nil || node.Name == task.NodeName {
			continue
		}
		if p.nodeAdmits(task.Pod, node.Node, required) {
			count++
		}
	}
	return allowedCount{count: count, known: true}
}

// nodeAdmits applies the enabled static factors through the scheduler's own
// helpers, so the count agrees with the filter stack that decides admissibility.
func (p *nodeConsolidationPlugin) nodeAdmits(pod *v1.Pod, node *v1.Node, required nodeaffinity.RequiredNodeAffinity) bool {
	if p.countCordon && node.Spec.Unschedulable {
		return false
	}
	if p.countTaints && !toleratesTaints(pod, node) {
		return false
	}
	if !p.countNodeAffinity {
		return true
	}
	// Folds spec.nodeSelector and nodeAffinity.required into one match; a
	// malformed selector surfaces as an error and is treated as not admitted.
	matches, err := required.Match(node)
	if err != nil {
		klog.V(5).InfoS("repack nodeconsolidation: unparsable nodeSelector/nodeAffinity, treating node as not admitted",
			"pod", pod.Namespace+"/"+pod.Name, "node", node.Name, "err", err)
		return false
	}
	return matches
}

// toleratesTaints mirrors the scheduler's TaintToleration filter: only
// NoSchedule and NoExecute taints reject a node.
func toleratesTaints(pod *v1.Pod, node *v1.Node) bool {
	if len(node.Spec.Taints) == 0 {
		return true
	}
	_, untolerated := corev1helper.FindMatchingUntoleratedTaint(klog.Background(), node.Spec.Taints,
		pod.Spec.Tolerations, doNotScheduleTaint, taintComparisonOperatorsEnabled)
	return !untolerated
}

func doNotScheduleTaint(taint *v1.Taint) bool {
	return taint.Effect == v1.TaintEffectNoSchedule || taint.Effect == v1.TaintEffectNoExecute
}
