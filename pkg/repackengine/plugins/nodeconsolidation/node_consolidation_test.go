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

package nodeconsolidation

import (
	"context"
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
	// Registers binpack's size key; OpenSession silently skips unknown plugin
	// names, so an unimported plugin would make the ordering tests vacuous.
	_ "volcano.sh/volcano/pkg/repackengine/plugins/binpack"
)

const testGPU = v1.ResourceName("nvidia.com/gpu")

type consolidationSnapshot struct {
	nodes []*schedapi.NodeInfo
}

func (s consolidationSnapshot) Nodes() []*schedapi.NodeInfo       { return s.nodes }
func (consolidationSnapshot) NodeInScope(*schedapi.NodeInfo) bool { return true }
func (consolidationSnapshot) PodGroupView(schedapi.JobID) api.PodGroupView {
	return api.PodGroupView{}
}
func (consolidationSnapshot) FeasibleRelocation(context.Context, []*api.Move, []*schedapi.TaskInfo, []*schedapi.NodeInfo) ([]*api.Move, bool) {
	return nil, false
}
func (consolidationSnapshot) HyperNodesSetByTier() map[int]sets.Set[string] {
	return map[int]sets.Set[string]{}
}
func (consolidationSnapshot) RealNodesSet() map[string]sets.Set[string] {
	return map[string]sets.Set[string]{}
}
func (consolidationSnapshot) HyperNodeTierNameMap() map[string]int {
	return map[string]int{}
}

// consolidationNode carries capacity and usage for the Domain half, which reads
// the target-resource classification.
func consolidationNode(name string, capacity, used int64) *schedapi.NodeInfo {
	resource := func(value int64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{testGPU: float64(value)}}
	}
	return &schedapi.NodeInfo{Name: name, Allocatable: resource(capacity), Used: resource(used)}
}

// The node builders below carry only the labels, taints and cordon flag the
// count half reads, so each test isolates one factor.
func zonedNode(name, zone string) *schedapi.NodeInfo {
	return &schedapi.NodeInfo{
		Name: name,
		Node: &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"zone": zone}},
		},
	}
}

func plainNode(name string) *schedapi.NodeInfo {
	return &schedapi.NodeInfo{Name: name, Node: &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}}
}

func cordonedNode(name string) *schedapi.NodeInfo {
	node := plainNode(name)
	node.Node.Spec.Unschedulable = true
	return node
}

func reservedNode(name, key, value string) *schedapi.NodeInfo {
	node := plainNode(name)
	node.Node.Spec.Taints = []v1.Taint{{Key: key, Value: value, Effect: v1.TaintEffectNoSchedule}}
	return node
}

func gpuTask(name, nodeName string, requested int64, pod *v1.Pod) *schedapi.TaskInfo {
	if pod == nil {
		pod = &v1.Pod{}
	}
	pod.Name = name
	return &schedapi.TaskInfo{
		UID: schedapi.TaskID(name), Name: name, Pod: pod,
		TransactionContext: schedapi.TransactionContext{NodeName: nodeName},
		InitResreq: &schedapi.Resource{
			ScalarResources: map[v1.ResourceName]float64{testGPU: float64(requested)},
		},
	}
}

func newTestPlugin(nodes []*schedapi.NodeInfo) *nodeConsolidationPlugin {
	return &nodeConsolidationPlugin{
		countNodeAffinity: true,
		countTaints:       true,
		countCordon:       true,
		nodes:             nodes,
		counts:            map[*schedapi.TaskInfo]allowedCount{},
	}
}

func countFor(t *testing.T, plugin *nodeConsolidationPlugin, task *schedapi.TaskInfo) int {
	t.Helper()
	result := plugin.evaluate(task)
	if !result.known {
		t.Fatalf("evaluate(%s) reported unknown, want a count", task.Name)
	}
	return result.count
}

func selectorPod(key, value string) *v1.Pod {
	return &v1.Pod{Spec: v1.PodSpec{NodeSelector: map[string]string{key: value}}}
}

func affinityPod(operator v1.NodeSelectorOperator, key string, values ...string) *v1.Pod {
	return &v1.Pod{Spec: v1.PodSpec{Affinity: &v1.Affinity{
		NodeAffinity: &v1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{{
					MatchExpressions: []v1.NodeSelectorRequirement{{Key: key, Operator: operator, Values: values}},
				}},
			},
		},
	}}}
}

func TestNodeConsolidationContributesOnlyPartiallyOccupiedNodes(t *testing.T) {
	snapshot := consolidationSnapshot{nodes: []*schedapi.NodeInfo{
		consolidationNode("unavailable", 0, 0),
		consolidationNode("empty", 8, 0),
		consolidationNode("partial", 8, 4),
		consolidationNode("full", 8, 8),
	}}
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot: snapshot,
		Resource: testGPU,
	}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	units := ssn.FreeableUnits()
	if len(units) != 1 || !reflect.DeepEqual(units[0].Nodes, []string{"partial"}) {
		t.Fatalf("freeable units=%+v, want only the partially occupied node", units)
	}
}

// The two-node sets below each isolate one factor so a failure names its cause.

func TestEvaluateCountsEachStaticFactor(t *testing.T) {
	t.Run("nodeSelector", func(t *testing.T) {
		plugin := newTestPlugin([]*schedapi.NodeInfo{zonedNode("source", "z0"), zonedNode("other", "z0"), zonedNode("target", "z1")})
		task := gpuTask("victim", "source", 1, selectorPod("zone", "z1"))
		if got := countFor(t, plugin, task); got != 1 {
			t.Fatalf("count=%d, want 1 (only the z1 node)", got)
		}
	})

	t.Run("nodeAffinity required In", func(t *testing.T) {
		plugin := newTestPlugin([]*schedapi.NodeInfo{zonedNode("source", "z0"), zonedNode("other", "z0"), zonedNode("target", "z1")})
		task := gpuTask("victim", "source", 1, affinityPod(v1.NodeSelectorOpIn, "zone", "z1"))
		if got := countFor(t, plugin, task); got != 1 {
			t.Fatalf("count=%d, want 1 (only the z1 node)", got)
		}
	})

	t.Run("nodeAffinity required NotIn", func(t *testing.T) {
		plugin := newTestPlugin([]*schedapi.NodeInfo{zonedNode("source", "z0"), zonedNode("other", "z0"), zonedNode("target", "z1")})
		task := gpuTask("victim", "source", 1, affinityPod(v1.NodeSelectorOpNotIn, "zone", "z1"))
		if got := countFor(t, plugin, task); got != 1 {
			t.Fatalf("count=%d, want 1 (only the node outside z1)", got)
		}
	})

	t.Run("taints", func(t *testing.T) {
		nodes := []*schedapi.NodeInfo{plainNode("source"), plainNode("free"), reservedNode("reserved", "dedicated", "gpu")}
		plugin := newTestPlugin(nodes)
		if got := countFor(t, plugin, gpuTask("victim", "source", 1, nil)); got != 1 {
			t.Fatalf("count=%d, want 1 (the NoSchedule node rejects an untolerating pod)", got)
		}
		tolerating := gpuTask("victim", "source", 1, nil)
		tolerating.Pod.Spec.Tolerations = []v1.Toleration{{Key: "dedicated", Operator: v1.TolerationOpEqual, Value: "gpu", Effect: v1.TaintEffectNoSchedule}}
		if got := countFor(t, plugin, tolerating); got != 2 {
			t.Fatalf("count=%d, want 2 (the tolerating pod admits both free nodes)", got)
		}
	})

	t.Run("cordon", func(t *testing.T) {
		plugin := newTestPlugin([]*schedapi.NodeInfo{plainNode("source"), plainNode("open"), cordonedNode("cordoned")})
		if got := countFor(t, plugin, gpuTask("victim", "source", 1, nil)); got != 1 {
			t.Fatalf("count=%d, want 1 (a cordoned node is not a candidate)", got)
		}
	})

	t.Run("source node is never a candidate", func(t *testing.T) {
		plugin := newTestPlugin([]*schedapi.NodeInfo{plainNode("source"), plainNode("other")})
		if got := countFor(t, plugin, gpuTask("victim", "source", 1, nil)); got != 1 {
			t.Fatalf("count=%d, want 1 (the drained node itself must not be counted)", got)
		}
	})
}

func TestEvaluateSkipsDisabledFactors(t *testing.T) {
	nodes := []*schedapi.NodeInfo{plainNode("source"), reservedNode("reserved", "dedicated", "gpu"), cordonedNode("cordoned")}

	all := newTestPlugin(nodes)
	if got := countFor(t, all, gpuTask("victim", "source", 1, nil)); got != 0 {
		t.Fatalf("all factors on: count=%d, want 0", got)
	}

	none := newTestPlugin(nodes)
	none.countTaints, none.countCordon = false, false
	if got := countFor(t, none, gpuTask("victim", "source", 1, nil)); got != 2 {
		t.Fatalf("taints and cordon off: count=%d, want 2", got)
	}
}

func TestEvaluateReportsUnknownWithoutPodSpec(t *testing.T) {
	plugin := newTestPlugin([]*schedapi.NodeInfo{plainNode("source"), plainNode("other")})
	specless := gpuTask("victim", "source", 1, nil)
	specless.Pod = nil
	if result := plugin.evaluate(specless); result.known {
		t.Fatal("a task without a pod spec must report unknown, not a count")
	}
}

// The memo key is the task object, not its UID: a plan-state clone keeps the UID
// while carrying a different source node, and the source node is excluded from
// the count.
func TestCountMemoizesPerTaskObjectNotUID(t *testing.T) {
	plugin := newTestPlugin([]*schedapi.NodeInfo{zonedNode("source", "z1"), zonedNode("other", "z0")})
	onSource := gpuTask("victim", "source", 1, selectorPod("zone", "z1"))
	onOther := gpuTask("victim", "other", 1, selectorPod("zone", "z1"))
	if onSource == onOther || onSource.UID != onOther.UID {
		t.Fatalf("test setup: want distinct objects sharing a UID, got %q vs %q", onSource.UID, onOther.UID)
	}

	if got := plugin.count(onSource); !got.known || got.count != 0 {
		t.Fatalf("count on source=%v, want a known 0: the z1 node is the source", got)
	}
	if got := plugin.count(onOther); !got.known || got.count != 1 {
		t.Fatalf("count on other=%v, want a known 1: the memo must not reuse the source-node answer", got)
	}
}

// nodes gives the constrained victim one admissible receiver and every other
// victim two, and the loose victim the larger request, so the two ordering keys
// disagree and each test can name which key it expects to decide.
func constrainedAndLooseVictims(t *testing.T) (plugin *nodeConsolidationPlugin, constrained, loose *schedapi.TaskInfo) {
	t.Helper()
	nodes := []*schedapi.NodeInfo{zonedNode("source", "z0"), zonedNode("plain", "z0"), zonedNode("target", "z1")}
	return newTestPlugin(nodes),
		gpuTask("constrained", "source", 1, selectorPod("zone", "z1")),
		gpuTask("loose", "source", 8, nil)
}

func TestCompareOrdersFewestAllowedReceiversFirst(t *testing.T) {
	plugin, constrained, loose := constrainedAndLooseVictims(t)

	if got := plugin.compare(constrained, loose); got >= 0 {
		t.Fatalf("compare(constrained, loose)=%d, want <0: fewer allowed receivers sorts first", got)
	}
	if got := plugin.compare(loose, constrained); got <= 0 {
		t.Fatalf("compare(loose, constrained)=%d, want >0", got)
	}
}

// The count key must win outright, not merely act as a tie-break: the loose
// victim requests eight times the cards and still sorts second.
func TestCompareIgnoresRequestSize(t *testing.T) {
	plugin, constrained, loose := constrainedAndLooseVictims(t)
	if got := api.Scalar(loose.InitResreq, testGPU); got <= api.Scalar(constrained.InitResreq, testGPU) {
		t.Fatalf("test setup: loose requests %v, want more than constrained", got)
	}
	if got := plugin.compare(constrained, loose); got >= 0 {
		t.Fatalf("compare=%d, want <0: the constrained victim wins despite its smaller request", got)
	}
	// Equal counts abstain in both directions: request size is binpack's key.
	small := gpuTask("small", "source", 1, nil)
	if got := plugin.compare(loose, small); got != 0 {
		t.Fatalf("compare(loose, small)=%d, want 0: the counts tie and this plugin owns no size key", got)
	}
	if got := plugin.compare(small, loose); got != 0 {
		t.Fatalf("compare(small, loose)=%d, want 0: the counts tie and this plugin owns no size key", got)
	}
}

// With every factor disabled the count key abstains, leaving the pair to the next
// registered plugin and then to the framework's UID tie-break.
func TestCompareAbstainsWhenEveryFactorIsDisabled(t *testing.T) {
	plugin, constrained, loose := constrainedAndLooseVictims(t)
	plugin.countNodeAffinity, plugin.countTaints, plugin.countCordon = false, false, false

	if got := plugin.compare(constrained, loose); got != 0 {
		t.Fatalf("compare(constrained, loose)=%d, want 0 (abstain)", got)
	}
	if got := plugin.compare(loose, constrained); got != 0 {
		t.Fatalf("compare(loose, constrained)=%d, want 0 (abstain)", got)
	}
}

// The engine opens a snapshot-less session to validate plugin composition, and
// this plugin is opened there because it provides the mandatory Domain. The count
// key must abstain rather than dereference the missing snapshot.
func TestSessionWithoutSnapshotOpensAndAbstains(t *testing.T) {
	ssn := framework.OpenSession(framework.SessionConfig{Resource: testGPU}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	ordered := ssn.OrderVictims([]*schedapi.TaskInfo{gpuTask("a", "source", 1, nil), gpuTask("b", "source", 1, nil)})
	if len(ordered) != 2 {
		t.Fatalf("victims=%d, want the session to open and order both", len(ordered))
	}
}

// An unevaluable victim must not be read as "admits no nodes", which would sort
// it first. The count key abstains instead.
func TestCompareAbstainsWithoutPodSpec(t *testing.T) {
	plugin, constrained, _ := constrainedAndLooseVictims(t)
	specless := gpuTask("specless", "source", 1, nil)
	specless.Pod = nil

	if got := plugin.compare(constrained, specless); got != 0 {
		t.Fatalf("compare(constrained, specless)=%d, want 0 (abstain)", got)
	}
	if got := plugin.compare(specless, constrained); got != 0 {
		t.Fatalf("compare(specless, constrained)=%d, want 0 (abstain)", got)
	}
}

// Neither plugin owns the victim order alone: each registers its own key and the
// plugin list order decides which leads. binpack therefore only settles ties the
// count key leaves behind — unless it is listed first, which inverts that.
func TestVictimOrderFollowsPluginOrder(t *testing.T) {
	nodes := []*schedapi.NodeInfo{zonedNode("source", "z0"), zonedNode("plain", "z0"), zonedNode("target", "z1")}
	snapshot := consolidationSnapshot{nodes: nodes}
	constrained := gpuTask("constrained", "source", 1, selectorPod("zone", "z1"))
	loose := gpuTask("loose", "source", 8, nil)

	order := func(options []framework.PluginOption) []string {
		ssn := framework.OpenSession(framework.SessionConfig{Snapshot: snapshot, Resource: testGPU}, options)
		defer framework.CloseSession(ssn)
		names := []string{}
		for _, victim := range ssn.OrderVictims([]*schedapi.TaskInfo{loose, constrained}) {
			names = append(names, victim.Name)
		}
		return names
	}

	countFirst := order(framework.PluginOptions(Name, "binpack"))
	if len(countFirst) != 2 || countFirst[0] != "constrained" {
		t.Fatalf("victim order with nodeconsolidation listed first=%v, want the constrained victim first", countFirst)
	}
	sizeFirst := order(framework.PluginOptions("binpack", Name))
	if len(sizeFirst) != 2 || sizeFirst[0] != "loose" {
		t.Fatalf("victim order with binpack listed first=%v, want the larger request first", sizeFirst)
	}
}

// Victims are collected by ranging a map, so the framework owes a total order
// even when no plugin has an opinion.
func TestOrderVictimsIsDeterministicWithoutPlugins(t *testing.T) {
	snapshot := consolidationSnapshot{}
	tasks := []*schedapi.TaskInfo{
		gpuTask("c", "source", 1, nil), gpuTask("a", "source", 1, nil), gpuTask("b", "source", 1, nil),
	}
	for _, permutation := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 0, 2}} {
		ssn := framework.OpenSession(framework.SessionConfig{Snapshot: snapshot, Resource: testGPU}, nil)
		shuffled := []*schedapi.TaskInfo{tasks[permutation[0]], tasks[permutation[1]], tasks[permutation[2]]}
		ordered := ssn.OrderVictims(shuffled)
		framework.CloseSession(ssn)

		for index, want := range []string{"a", "b", "c"} {
			if ordered[index].Name != want {
				t.Fatalf("permutation %v: order=[%s %s %s], want [a b c]",
					permutation, ordered[0].Name, ordered[1].Name, ordered[2].Name)
			}
		}
	}
}

func TestValidateArguments(t *testing.T) {
	if err := validateArguments(nil); err != nil {
		t.Fatalf("empty arguments rejected: %v", err)
	}
	if err := validateArguments(framework.Arguments{argTaints: false}); err != nil {
		t.Fatalf("valid arguments rejected: %v", err)
	}
	if err := validateArguments(framework.Arguments{"nodeAffinities": true}); err == nil {
		t.Fatal("a misspelled argument key should be rejected")
	}
	if err := validateArguments(framework.Arguments{"resourceRequests": false}); err == nil {
		t.Fatal("resourceRequests is binpack's key, so nodeconsolidation must reject it")
	}
	if err := validateArguments(framework.Arguments{argCordon: "true"}); err == nil {
		t.Fatal("a non-boolean argument value should be rejected")
	}
}
