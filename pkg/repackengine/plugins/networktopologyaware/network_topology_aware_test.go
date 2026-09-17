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

package networktopologyaware

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/volcano/pkg/controllers/repack/state"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/conf"
	"volcano.sh/volcano/pkg/repackengine/framework"

	_ "volcano.sh/volcano/pkg/repackengine/plugins/binpack"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/workloaddisruption"
)

// These tests pin the block-score semantics: pure-function tests call the score
// functions directly, session tests run the real OpenSession + PlanScores pipeline.

const testResource = v1.ResourceName("example.com/accelerator")

type topologySnapshot struct {
	nodes            []*schedapi.NodeInfo
	hyperNodesByTier map[int]sets.Set[string]
	realNodesSet     map[string]sets.Set[string]
	tierNames        map[string]int
}

func (s topologySnapshot) Nodes() []*schedapi.NodeInfo { return s.nodes }
func (topologySnapshot) NodeInScope(*schedapi.NodeInfo) bool {
	return true
}
func (topologySnapshot) PodGroupView(schedapi.JobID) api.PodGroupView { return api.PodGroupView{} }
func (topologySnapshot) FeasibleRelocation(context.Context, []*api.Move, []*schedapi.TaskInfo, []*schedapi.NodeInfo) ([]*api.Move, bool) {
	return nil, false
}
func (s topologySnapshot) HyperNodesSetByTier() map[int]sets.Set[string] { return s.hyperNodesByTier }
func (s topologySnapshot) RealNodesSet() map[string]sets.Set[string]     { return s.realNodesSet }
func (s topologySnapshot) HyperNodeTierNameMap() map[string]int          { return s.tierNames }

func topologyNode(name string, capacity, used int64) *schedapi.NodeInfo {
	resource := func(value int64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{testResource: float64(value)}}
	}
	return &schedapi.NodeInfo{Name: name, Allocatable: resource(capacity), Used: resource(used)}
}

func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }

// topologyRun builds a RepackRun with the given networkTopology.
func topologyRun(mode repackv1alpha1.RepackBlockMode, tier *int, tierName *string, size, required int) *repackv1alpha1.RepackRun {
	return &repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{
		NetworkTopology: &repackv1alpha1.NetworkTopology{
			HyperNodeTier:      tier,
			HyperNodeTierName:  tierName,
			NodeBlockSize:      intPtr(size),
			RequiredNodeBlocks: required,
			Mode:               mode,
		},
	}}
}

// openSession opens a session over the snapshot with the given run and options.
func openSession(snapshot framework.Snapshot, run *repackv1alpha1.RepackRun, options []framework.PluginOption) *framework.Session {
	return framework.OpenSession(framework.SessionConfig{
		Snapshot: snapshot,
		Resource: testResource,
		Run:      run,
	}, options)
}

// candidate frees exactly one node (the single-node consolidation unit).
func candidate(from string) *api.CandidatePlan {
	return api.NewCandidatePlan(nil, []*api.Move{{From: from}})
}

// findTerm returns the score term with the given name.
func findTerm(score framework.CandidatePlanScore, name string) (framework.PlanScoreTerm, bool) {
	for _, term := range score.Terms {
		if term.Name == name {
			return term, true
		}
	}
	return framework.PlanScoreTerm{}, false
}

func scoreFor(ssn *framework.Session, candidates []*api.CandidatePlan) []framework.CandidatePlanScore {
	return ssn.PlanScores(candidates)
}

func TestNodeBlockProgressScore(t *testing.T) {
	cases := []struct {
		name                                       string
		freeInHyperNode, freeableInHyperNode, size int
		want                                       int64
	}{
		{"r==0 exact block is max", 4, 0, 4, 4},
		{"r==0 with nothing idle", 0, 0, 4, 4},
		{"r==0 two full blocks", 8, 0, 4, 4},
		{"partial reachable", 1, 3, 4, 1},
		{"partial reachable two", 2, 2, 4, 2},
		{"partial reachable three", 3, 1, 4, 3},
		{"freeable just enough", 5, 3, 4, 1}, // r=1 needs 3 more, exactly 3 freeable
		{"freeable short one", 1, 2, 4, 0},   // r=1 needs 3, only 2 freeable
		{"freeable short all", 3, 0, 4, 0},   // r=3 needs 1, none freeable
		{"negative freeable", 2, -1, 4, 0},
		{"size 1 always a block", 0, 0, 1, 1},
		{"size 1 with free", 7, 3, 1, 1},
		// size<1 is normalized to 1 BEFORE the modulo, so any freeInHyperNode scores max.
		{"size 0 degrades to 1", 3, 0, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeBlockProgressScore(tc.freeInHyperNode, tc.freeableInHyperNode, tc.size); got != tc.want {
				t.Errorf("nodeBlockProgressScore(%d,%d,%d)=%d, want %d",
					tc.freeInHyperNode, tc.freeableInHyperNode, tc.size, got, tc.want)
			}
		})
	}
}

func TestNodeBlockDistributionScore(t *testing.T) {
	cases := []struct {
		name   string
		mode   repackv1alpha1.RepackBlockMode
		blocks int
		want   int64
	}{
		{"binpack concentrates more", repackv1alpha1.RepackBlockModeBinpack, 3, 3},
		{"spread disperses fewer", repackv1alpha1.RepackBlockModeSpread, 3, -3},
		{"binpack zero blocks", repackv1alpha1.RepackBlockModeBinpack, 0, 0},
		{"unknown mode neutral", repackv1alpha1.RepackBlockMode(""), 3, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeBlockDistributionScore(tc.mode, tc.blocks); got != tc.want {
				t.Errorf("nodeBlockDistributionScore(%s,%d)=%d, want %d", tc.mode, tc.blocks, got, tc.want)
			}
		})
	}
}

// A no-H anchor takes this floor; so does a HyperNode at its block ceiling.
func TestNodeBlockDistributionFloor(t *testing.T) {
	cases := []struct {
		name                 string
		mode                 repackv1alpha1.RepackBlockMode
		maxBlocksInHyperNode int
		want                 int64
	}{
		{"binpack strictly worst (below zero-block H)", repackv1alpha1.RepackBlockModeBinpack, 5, -1},
		{"spread least preferred (below max-block H)", repackv1alpha1.RepackBlockModeSpread, 5, -6},
		{"spread no-H sparse tier (maxBlocksInHyperNode=0, below zero-block H)", repackv1alpha1.RepackBlockModeSpread, 0, -1},
		{"unknown mode neutral", repackv1alpha1.RepackBlockMode(""), 5, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeBlockDistributionFloor(tc.mode, tc.maxBlocksInHyperNode); got != tc.want {
				t.Errorf("nodeBlockDistributionFloor(%s,%d)=%d, want %d",
					tc.mode, tc.maxBlocksInHyperNode, got, tc.want)
			}
		})
	}
}

// Headline invariant: for the same HyperNode, spread is the exact negation
// of binpack, so a tie in one never flips in the other.
func TestDistributionOppositeSignsPerMode(t *testing.T) {
	for _, blocks := range []int{0, 1, 4, 9} {
		bin := nodeBlockDistributionScore(repackv1alpha1.RepackBlockModeBinpack, blocks)
		spread := nodeBlockDistributionScore(repackv1alpha1.RepackBlockModeSpread, blocks)
		if bin != -spread {
			t.Errorf("blocks=%d: binpack=%d, spread=%d, want exact opposites", blocks, bin, spread)
		}
	}
}

func TestTotalBlocksInTier(t *testing.T) {
	idle := map[string]int{"hnA": 3, "hnB": 0}
	freed := map[string]int{"hnA": 1, "hnB": 0, "outside-tier": 9}
	hyperNodes := []string{"hnA", "hnB"}

	if got := totalBlocksInTier(idle, freed, hyperNodes, 2); got != 2 {
		t.Errorf("totalBlocksInTier(size=2)=%d, want 2 (hnA (3+1)/2 + hnB 0/2)", got)
	}
	// size=1: hnA (3+1)/1 + hnB 0/1 = 4; "outside-tier" is in no tier HyperNode, never counts.
	if got := totalBlocksInTier(idle, freed, hyperNodes, 1); got != 4 {
		t.Errorf("totalBlocksInTier(size=1)=%d, want 4 (hnA 4/1 + hnB 0/1)", got)
	}
	if got := totalBlocksInTier(idle, freed, nil, 2); got != 0 {
		t.Errorf("totalBlocksInTier(empty tier)=%d, want 0 (nodes outside the tier never count)", got)
	}
	// size<1 degrades to 1, so the same 4.
	if got := totalBlocksInTier(idle, freed, hyperNodes, 0); got != 4 {
		t.Errorf("totalBlocksInTier(size=0)=%d, want 4 (size<1 degrades to 1)", got)
	}
}

func TestOnSessionOpenRegistersNothingWithoutTopology(t *testing.T) {
	snapshot := topologySnapshot{
		nodes:            []*schedapi.NodeInfo{topologyNode("a1", 8, 4)},
		hyperNodesByTier: map[int]sets.Set[string]{2: sets.New[string]("hnA")},
		realNodesSet:     map[string]sets.Set[string]{"hnA": sets.New[string]("a1")},
	}
	runCases := []struct {
		name string
		run  *repackv1alpha1.RepackRun
	}{
		{"run is nil", nil},
		{"networkTopology unset", &repackv1alpha1.RepackRun{}},
		{"neither tier field set", topologyRun(repackv1alpha1.RepackBlockModeBinpack, nil, nil, 4, 0)},
		{"numeric tier does not exist", topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(99), nil, 4, 0)},
		{"tierName does not exist", topologyRun(repackv1alpha1.RepackBlockModeBinpack, nil, strPtr("missing"), 4, 0)},
		{"tier exists but has no HyperNode", topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(3), nil, 4, 0)},
	}
	for _, tc := range runCases {
		t.Run(tc.name, func(t *testing.T) {
			ssn := openSession(snapshot, tc.run, framework.PluginOptions(Name))
			defer framework.CloseSession(ssn)
			scores := scoreFor(ssn, []*api.CandidatePlan{candidate("a1")})
			if len(scores[0].Terms) != 0 {
				t.Errorf("no topology -> terms=%v, want none registered", scores[0].Terms)
			}
		})
	}
}

func TestOnSessionOpenRegistrationDependsOnMode(t *testing.T) {
	snapshot := topologySnapshot{
		nodes: []*schedapi.NodeInfo{
			topologyNode("a1", 8, 4), topologyNode("b1", 8, 4), topologyNode("b2", 8, 0),
		},
		hyperNodesByTier: map[int]sets.Set[string]{2: sets.New[string]("hnA", "hnB")},
		realNodesSet: map[string]sets.Set[string]{
			"hnA": sets.New[string]("a1"),
			"hnB": sets.New[string]("b1", "b2"),
		},
	}
	wantTerms := map[string][]string{
		"":        {"nodeBlockProgress"},
		"binpack": {"nodeBlockProgress", "nodeBlockDistribution"},
		"spread":  {"nodeBlockProgress", "nodeBlockDistribution"},
	}
	for mode, want := range wantTerms {
		run := topologyRun(repackv1alpha1.RepackBlockMode(mode), intPtr(2), nil, 4, 0)
		ssn := openSession(snapshot, run, framework.PluginOptions(Name))
		defer framework.CloseSession(ssn)

		scores := scoreFor(ssn, []*api.CandidatePlan{candidate("a1")})
		var got []string
		for _, term := range scores[0].Terms {
			got = append(got, term.Name)
		}
		if len(got) != len(want) {
			t.Errorf("mode %q terms=%v, want %v", mode, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("mode %q terms=%v, want %v", mode, got, want)
				break
			}
		}
	}
}

// Topology for the anchoring tests — tier 2: hnA -> a1..a4 (all Partial),
// hnB -> b1 (Partial) + b2 (Empty), plus "outside", which belongs to no HyperNode.
func anchorSnapshot() topologySnapshot {
	nodes := []*schedapi.NodeInfo{}
	for _, name := range []string{"a1", "a2", "a3", "a4", "b1"} {
		nodes = append(nodes, topologyNode(name, 8, 4)) // Partial
	}
	nodes = append(nodes, topologyNode("b2", 8, 0), topologyNode("outside", 8, 4))
	return topologySnapshot{
		nodes:            nodes,
		hyperNodesByTier: map[int]sets.Set[string]{2: sets.New[string]("hnA", "hnB")},
		realNodesSet: map[string]sets.Set[string]{
			"hnA": sets.New[string]("a1", "a2", "a3", "a4"),
			"hnB": sets.New[string]("b1", "b2"),
		},
	}
}

func TestBlockScoreRawValuesAnchorOnTheSingleFreedNode(t *testing.T) {
	snapshot := anchorSnapshot()
	run := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(2), nil, 4, 0)
	ssn := openSession(snapshot, run, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	// hnA idle 0 / busy 4; hnB idle 1 (b2) / busy 1 (b1); size 4.
	// P_A (a1): freeInHyperNode 1, freeable 3 -> progress 1, blocks 0.
	// P_B (b1): freeInHyperNode 2, freeable 0 -> progress 0; hnB's idle+busy 2 < size,
	// so it cannot fill a block and takes the same distribution -1 as a no-H anchor.
	// P_X (outside): progress 0, distribution -1.
	candidates := []*api.CandidatePlan{candidate("a1"), candidate("b1"), candidate("outside")}
	scores := scoreFor(ssn, candidates)

	wantRaw := map[string]map[string]int64{
		"a1":      {"nodeBlockProgress": 1, "nodeBlockDistribution": 0},
		"b1":      {"nodeBlockProgress": 0, "nodeBlockDistribution": -1},
		"outside": {"nodeBlockProgress": 0, "nodeBlockDistribution": -1},
	}
	for i, cand := range candidates {
		anchor := cand.IncrementalFromNodes()[0]
		for termName, want := range wantRaw[anchor] {
			term, ok := findTerm(scores[i], termName)
			if !ok {
				t.Errorf("candidate %s: term %q missing", anchor, termName)
				continue
			}
			if term.Raw != want {
				t.Errorf("candidate %s: %s raw=%d, want %d", anchor, termName, term.Raw, want)
			}
		}
	}
}

// The package doc's example selects the tier by name, so a by-name lookup must
// build the same session as the numeric tier.
func TestHyperNodeTierNameResolvesLikeTheNumericTier(t *testing.T) {
	byName := anchorSnapshot()
	byName.tierNames = map[string]int{"accel": 2}

	byTierSSN := openSession(anchorSnapshot(),
		topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(2), nil, 4, 0), framework.PluginOptions(Name))
	defer framework.CloseSession(byTierSSN)
	byNameSSN := openSession(byName,
		topologyRun(repackv1alpha1.RepackBlockModeBinpack, nil, strPtr("accel"), 4, 0), framework.PluginOptions(Name))
	defer framework.CloseSession(byNameSSN)

	byTierScores := scoreFor(byTierSSN, []*api.CandidatePlan{candidate("a1"), candidate("b1"), candidate("outside")})
	byNameScores := scoreFor(byNameSSN, []*api.CandidatePlan{candidate("a1"), candidate("b1"), candidate("outside")})

	for i := range byTierScores {
		if len(byTierScores[i].Terms) != 2 || len(byNameScores[i].Terms) != 2 {
			t.Fatalf("candidate %d: terms by tier=%v by name=%v, want both block terms",
				i, byTierScores[i].Terms, byNameScores[i].Terms)
		}
		for _, termName := range []string{"nodeBlockProgress", "nodeBlockDistribution"} {
			byTierTerm, _ := findTerm(byTierScores[i], termName)
			byNameTerm, ok := findTerm(byNameScores[i], termName)
			if !ok {
				t.Errorf("candidate %d: term %q missing under tierName", i, termName)
				continue
			}
			if byNameTerm.Raw != byTierTerm.Raw {
				t.Errorf("candidate %d: %s raw=%d under tierName, want %d (same as the numeric tier)",
					i, termName, byNameTerm.Raw, byTierTerm.Raw)
			}
		}
	}
}

// A no-H candidate must score worst under spread: progress 0 and the floor
// -(maxBlocksInHyperNode+1), strictly below any HyperNode that can fill a block.
// A HyperNode at its block ceiling shares that floor.
func TestNoHyperNodeCandidateScoresWorstUnderSpread(t *testing.T) {
	snapshot := anchorSnapshot()
	run := topologyRun(repackv1alpha1.RepackBlockModeSpread, intPtr(2), nil, 4, 0)
	ssn := openSession(snapshot, run, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	candidates := []*api.CandidatePlan{candidate("a1"), candidate("b1"), candidate("outside")}
	scores := scoreFor(ssn, candidates)

	// maxBlocksInHyperNode = max(4/4, 2/4) = 1, so outside takes -2 while the live hnA
	// candidate takes 0; hnB's idle+busy 2 < size 4, so it is at its block ceiling too
	// and takes the same -2.
	outside, ok := findTerm(scores[2], "nodeBlockDistribution")
	if !ok {
		t.Fatal("outside candidate distribution term missing")
	}
	if outside.Raw != -2 {
		t.Errorf("outside candidate distribution raw=%d, want -(maxBlocksInHyperNode+1)=-2", outside.Raw)
	}
	if scores[0].Total <= scores[2].Total {
		t.Errorf("the live-HyperNode candidate (%d) must strictly beat the no-H candidate (%d)",
			scores[0].Total, scores[2].Total)
	}
	if scores[1].Total != scores[2].Total {
		t.Errorf("hnB at its block ceiling (%d) and the no-H candidate (%d) must tie: neither can host a block",
			scores[1].Total, scores[2].Total)
	}
	if scores[1].Total >= scores[0].Total {
		t.Errorf("hnA candidate total=%d must beat hnB candidate total=%d",
			scores[0].Total, scores[1].Total)
	}
}

// Binpack counterpart: outside's -1 sits strictly below the live zero-block H's 0,
// so the a1 candidate (progress 1) beats it; hnB, at its block ceiling, ties it.
func TestNoHyperNodeCandidateScoresWorstUnderBinpack(t *testing.T) {
	snapshot := anchorSnapshot()
	run := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(2), nil, 4, 0)
	ssn := openSession(snapshot, run, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	candidates := []*api.CandidatePlan{candidate("a1"), candidate("b1"), candidate("outside")}
	scores := scoreFor(ssn, candidates)

	outside, ok := findTerm(scores[2], "nodeBlockDistribution")
	if !ok {
		t.Fatal("outside candidate distribution term missing")
	}
	if outside.Raw != -1 {
		t.Errorf("outside candidate distribution raw=%d, want -1 (below the live zero-block H's 0)", outside.Raw)
	}
	if scores[0].Total <= scores[2].Total {
		t.Errorf("live-HyperNode candidate a1 (%d) must beat the no-H candidate (%d)",
			scores[0].Total, scores[2].Total)
	}
	// b1's HyperNode is at its block ceiling too, so its -1 ties the no-H floor.
	if scores[1].Total != scores[2].Total {
		t.Errorf("hnB at its block ceiling (%d) and the no-H candidate (%d) must tie",
			scores[1].Total, scores[2].Total)
	}
}

// Topology for these tests — tier 2, size 4:
//
//	hnA -> aidle0..5 (Empty) + a1 (Partial): idle 6 + busy 1, so once a1 is freed
//	       nothing is left to drain — hnA holds one block and cannot fill a second.
//	hnB -> bidle0, bidle1 (Empty) + hnBPartials (Partial): {"b1", "b2"} leaves it one
//	       drained node short of its first block; {"b1"} puts it at its block ceiling.
func hnAAtBlockCeilingSnapshot(hnBPartials []string) topologySnapshot {
	nodes := []*schedapi.NodeInfo{}
	for _, name := range []string{"aidle0", "aidle1", "aidle2", "aidle3", "aidle4", "aidle5", "bidle0", "bidle1"} {
		nodes = append(nodes, topologyNode(name, 8, 0)) // Empty
	}
	for _, name := range []string{"a1", "b1", "b2"} {
		nodes = append(nodes, topologyNode(name, 8, 4)) // Partial
	}
	hnA := sets.New[string]("aidle0", "aidle1", "aidle2", "aidle3", "aidle4", "aidle5", "a1")
	hnB := sets.New[string]("bidle0", "bidle1")
	hnB.Insert(hnBPartials...)
	return topologySnapshot{
		nodes:            nodes,
		hyperNodesByTier: map[int]sets.Set[string]{2: sets.New[string]("hnA", "hnB")},
		realNodesSet:     map[string]sets.Set[string]{"hnA": hnA, "hnB": hnB},
	}
}

// hnA holds one block, which used to earn it the top binpack distribution raw (+1
// against hnB's 0) and decide the choice as soon as nodeBlockProgressWeight dropped
// below the distribution weight. Its block count must not outrank one that can
// complete a block.
func TestHyperNodeAtBlockCeilingDoesNotOutrankOneThatCanCompleteABlock(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mode           repackv1alpha1.RepackBlockMode
		progressWeight int64
	}{
		{"binpack default weight", repackv1alpha1.RepackBlockModeBinpack, weightNodeBlockProgress},
		{"spread default weight", repackv1alpha1.RepackBlockModeSpread, weightNodeBlockProgress},
		{"binpack with progress weight tuned down", repackv1alpha1.RepackBlockModeBinpack, 0},
		{"spread with progress weight tuned down", repackv1alpha1.RepackBlockModeSpread, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := topologyRun(tc.mode, intPtr(2), nil, 4, 0)
			ssn := openSession(hnAAtBlockCeilingSnapshot([]string{"b1", "b2"}), run, []framework.PluginOption{
				{Name: Name, Arguments: framework.Arguments{"nodeBlockProgressWeight": tc.progressWeight}},
			})
			defer framework.CloseSession(ssn)

			scores := scoreFor(ssn, []*api.CandidatePlan{candidate("a1"), candidate("b1")})
			atCeiling, ok := findTerm(scores[0], "nodeBlockDistribution")
			if !ok {
				t.Fatal("hnA block-ceiling candidate distribution term missing")
			}
			live, ok := findTerm(scores[1], "nodeBlockDistribution")
			if !ok {
				t.Fatal("live HyperNode candidate distribution term missing")
			}
			if atCeiling.Raw >= live.Raw {
				t.Errorf("hnA at its block ceiling (1 block, nothing left to drain) distribution raw=%d must sit below hnB (one node short of a block) raw=%d",
					atCeiling.Raw, live.Raw)
			}
			if scores[1].Total <= scores[0].Total {
				t.Errorf("hnB candidate total=%d must beat the hnA block-ceiling candidate total=%d",
					scores[1].Total, scores[0].Total)
			}
		})
	}
}

// With every HyperNode at its block ceiling the block terms have nothing to say, so
// the candidates tie and the cost term decides.
func TestHyperNodesAtBlockCeilingTieOnBothBlockTerms(t *testing.T) {
	for _, mode := range []repackv1alpha1.RepackBlockMode{
		repackv1alpha1.RepackBlockModeBinpack, repackv1alpha1.RepackBlockModeSpread,
	} {
		t.Run(string(mode), func(t *testing.T) {
			run := topologyRun(mode, intPtr(2), nil, 4, 0)
			ssn := openSession(hnAAtBlockCeilingSnapshot([]string{"b1"}), run, framework.PluginOptions(Name))
			defer framework.CloseSession(ssn)

			scores := scoreFor(ssn, []*api.CandidatePlan{candidate("a1"), candidate("b1")})
			first, ok := findTerm(scores[0], "nodeBlockDistribution")
			if !ok {
				t.Fatal("hnA distribution term missing")
			}
			second, ok := findTerm(scores[1], "nodeBlockDistribution")
			if !ok {
				t.Fatal("hnB distribution term missing")
			}
			if first.Raw != second.Raw {
				t.Errorf("hnA raw=%d and hnB raw=%d must match: neither HyperNode can host a block",
					first.Raw, second.Raw)
			}
			if scores[0].Total != scores[1].Total {
				t.Errorf("hnA total=%d and hnB total=%d must tie on the block terms",
					scores[0].Total, scores[1].Total)
			}
		})
	}
}

func TestNodeToHyperNodeOverlapCountsOnce(t *testing.T) {
	snapshot := topologySnapshot{
		nodes: []*schedapi.NodeInfo{
			topologyNode("shared", 8, 4), topologyNode("a2", 8, 4), topologyNode("b1", 8, 4),
		},
		// Both HyperNodes claim "shared"; hnA sorts first so it must win.
		hyperNodesByTier: map[int]sets.Set[string]{1: sets.New[string]("hnA", "hnB")},
		realNodesSet: map[string]sets.Set[string]{
			"hnA": sets.New[string]("shared", "a2"),
			"hnB": sets.New[string]("shared", "b1"),
		},
	}
	ssn := openSession(snapshot, nil, nil)
	defer framework.CloseSession(ssn)

	blockSession, ok := buildNodeBlockSession(ssn, topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(1), nil, 4, 0).Spec.NetworkTopology)
	if !ok {
		t.Fatal("buildNodeBlockSession failed")
	}
	if blockSession.nodeToHyperNode["shared"] != "hnA" {
		t.Errorf("nodeToHyperNode[shared]=%q, want hnA (first hit wins)", blockSession.nodeToHyperNode["shared"])
	}
	if blockSession.busyInHyperNode["hnA"] != 2 || blockSession.busyInHyperNode["hnB"] != 1 {
		// shared counted once (under hnA) + a2 under hnA; b1 under hnB.
		t.Errorf("busyInHyperNode=%v, want hnA:2, hnB:1 (shared must not double count)", blockSession.busyInHyperNode)
	}
	if blockSession.busyInHyperNode["hnA"]+blockSession.busyInHyperNode["hnB"] != 3 {
		t.Errorf("total classified nodes=%d, want 3 (three distinct nodes)", blockSession.busyInHyperNode["hnA"]+blockSession.busyInHyperNode["hnB"])
	}
}

func TestBlockCountConstraintAdmission(t *testing.T) {
	// tier 5: hnA -> a1..a4 (Partial), hnB -> b1..b4 (Partial). size=4.
	nodes := []*schedapi.NodeInfo{}
	for _, name := range []string{"a1", "a2", "a3", "a4", "b1", "b2", "b3", "b4"} {
		nodes = append(nodes, topologyNode(name, 8, 4))
	}
	snapshot := topologySnapshot{
		nodes:            nodes,
		hyperNodesByTier: map[int]sets.Set[string]{5: sets.New[string]("hnA", "hnB")},
		realNodesSet: map[string]sets.Set[string]{
			"hnA": sets.New[string]("a1", "a2", "a3", "a4"),
			"hnB": sets.New[string]("b1", "b2", "b3", "b4"),
		},
	}
	// 2 required blocks of 4 nodes: only a plan emptying two whole HyperNodes passes.
	run := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(5), nil, 4, 2)
	ssn := openSession(snapshot, run, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	cases := []struct {
		name  string
		freed []string
		want  bool
	}{
		{"one block short", []string{"a1", "a2", "a3", "a4"}, false},
		{"meets two blocks", []string{"a1", "a2", "a3", "a4", "b1", "b2", "b3", "b4"}, true},
	}
	for _, tc := range cases {
		if got := ssn.PlanAdmissible(&api.RepackPlan{FreedNodes: tc.freed}); got != tc.want {
			t.Errorf("%s: admissible=%v, want %v", tc.name, got, tc.want)
		}
		wantReason := ""
		if !tc.want {
			wantReason = state.ReasonRequiredNodeBlocksNotMet
		}
		if got := ssn.ConstraintRejection(); got != wantReason {
			t.Errorf("%s: constraintRejection=%q, want %q", tc.name, got, wantReason)
		}
	}

	// requiredBlocks=0 always admits, even a single freed node.
	noFloor := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(5), nil, 4, 0)
	lenient := openSession(snapshot, noFloor, framework.PluginOptions(Name))
	defer framework.CloseSession(lenient)
	if !lenient.PlanAdmissible(&api.RepackPlan{FreedNodes: []string{"a1"}}) {
		t.Error("requiredBlocks=0 must always pass the block-count constraint")
	}
}

// Topology for dominance tests. Both anchors must stay block-capable, or the
// block-ceiling demotion would make the two terms agree instead of opposing.
//
//	tier 3: hnA -> 3 Empty + 4 Partial; hnB -> 9 Empty + 3 Partial.
func dominanceSnapshot() topologySnapshot {
	nodes := []*schedapi.NodeInfo{}
	for _, name := range []string{"a1", "a2", "a3"} {
		nodes = append(nodes, topologyNode(name, 8, 0)) // Empty
	}
	for _, name := range []string{"a4", "a5", "a6", "a7"} {
		nodes = append(nodes, topologyNode(name, 8, 4)) // Partial
	}
	for _, name := range []string{"b1", "b2", "b3", "b4", "b5", "b6", "b7", "b8", "b9"} {
		nodes = append(nodes, topologyNode(name, 8, 0)) // Empty
	}
	for _, name := range []string{"b10", "b11", "b12"} {
		nodes = append(nodes, topologyNode(name, 8, 4)) // Partial
	}
	return topologySnapshot{
		nodes:            nodes,
		hyperNodesByTier: map[int]sets.Set[string]{3: sets.New[string]("hnA", "hnB")},
		realNodesSet: map[string]sets.Set[string]{
			"hnA": sets.New[string]("a1", "a2", "a3", "a4", "a5", "a6", "a7"),
			"hnB": sets.New[string]("b1", "b2", "b3", "b4", "b5", "b6", "b7", "b8", "b9", "b10", "b11", "b12"),
		},
	}
}

func TestProgressScoreDominatesDistribution(t *testing.T) {
	snapshot := dominanceSnapshot()
	run := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(3), nil, 4, 0)
	ssn := openSession(snapshot, run, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	// P_A frees an hnA Partial: freeInHyperNode 3+1=4 -> progress 4 (full block), blocks 1.
	// P_B frees an hnB Partial: freeInHyperNode 9+1=10 -> progress 2 (r=2, 2 drainable),
	// blocks 2. Distribution OPPOSES progress, yet the progress weight must dominate.
	candidates := []*api.CandidatePlan{candidate("a4"), candidate("b10")}
	scores := scoreFor(ssn, candidates)

	progressA, _ := findTerm(scores[0], "nodeBlockProgress")
	progressB, _ := findTerm(scores[1], "nodeBlockProgress")
	distA, _ := findTerm(scores[0], "nodeBlockDistribution")
	distB, _ := findTerm(scores[1], "nodeBlockDistribution")

	if !(progressA.Raw > progressB.Raw) {
		t.Fatalf("precondition: progressA=%d must exceed progressB=%d", progressA.Raw, progressB.Raw)
	}
	if !(distB.Raw > distA.Raw) {
		t.Fatalf("precondition: distributionB=%d must exceed distributionA=%d", distB.Raw, distA.Raw)
	}
	if scores[0].Total <= scores[1].Total {
		t.Errorf("higher progress must win regardless of distribution: totalA=%d totalB=%d",
			scores[0].Total, scores[1].Total)
	}
}

// Topology for the cost test — tier 4: hnA -> 2 Empty + 4 Partial; hnB -> 4 Partial, size 2.
func costSnapshot() topologySnapshot {
	nodes := []*schedapi.NodeInfo{topologyNode("a1", 8, 0), topologyNode("a2", 8, 0)}
	for _, name := range []string{"a3", "a4", "a5", "a6", "b1", "b2", "b3", "b4"} {
		nodes = append(nodes, topologyNode(name, 8, 4))
	}
	return topologySnapshot{
		nodes:            nodes,
		hyperNodesByTier: map[int]sets.Set[string]{4: sets.New[string]("hnA", "hnB")},
		realNodesSet: map[string]sets.Set[string]{
			"hnA": sets.New[string]("a1", "a2", "a3", "a4", "a5", "a6"),
			"hnB": sets.New[string]("b1", "b2", "b3", "b4"),
		},
	}
}

func TestDistributionScoreDominatesDisruptionCost(t *testing.T) {
	snapshot := costSnapshot()
	run := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(4), nil, 2, 0)
	ssn := openSession(snapshot, run, framework.PluginOptions(Name, "workloaddisruption"))
	defer framework.CloseSession(ssn)

	// Same progress tier (both raw 1): hnA freeInHyperNode 2+1=3, hnB 1, both r=1 with
	// enough freeable. Distribution differs — hnA blocks 3/2=1, hnB 1/2=0 — so binpack
	// prefers P_A. Cost opposes: P_A disrupts pgA (1 pod, 4000 cards), P_B has no task
	// and costs 0. With w_dist=100 vs w_cost=10+3+1=14, distribution must win.
	pA := api.NewCandidatePlan(nil, []*api.Move{{
		From: "a3", To: "b1",
		Task: &schedapi.TaskInfo{
			Name: "t", Job: schedapi.JobID("pgA"),
			InitResreq: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{testResource: 4000}},
		},
	}})
	pB := api.NewCandidatePlan(nil, []*api.Move{{From: "b1", To: "a1"}})
	candidates := []*api.CandidatePlan{pA, pB}
	scores := scoreFor(ssn, candidates)

	progressA, _ := findTerm(scores[0], "nodeBlockProgress")
	progressB, _ := findTerm(scores[1], "nodeBlockProgress")
	if progressA.Raw != progressB.Raw {
		t.Fatalf("precondition: same progress tier, got %d vs %d", progressA.Raw, progressB.Raw)
	}
	distA, _ := findTerm(scores[0], "nodeBlockDistribution")
	distB, _ := findTerm(scores[1], "nodeBlockDistribution")
	if !(distA.Raw > distB.Raw) {
		t.Fatalf("precondition: distributionA=%d must exceed distributionB=%d", distA.Raw, distB.Raw)
	}
	if scores[0].Total <= scores[1].Total {
		t.Errorf("distribution must dominate cost within the same progress tier: totalA=%d totalB=%d",
			scores[0].Total, scores[1].Total)
	}
}

func TestNodeBlockScoreNoOverflowAtMaxWeights(t *testing.T) {
	// costSnapshot with size 2: hnA idle 2 / busy 4, hnB idle 0 / busy 4.
	// P_A (a3): freeInHyperNode 3 -> progress raw 1, blocks 1 (tier max).
	// P_B (b1): freeInHyperNode 1 -> progress raw 1 (same tier), blocks 0.
	// Progress raws tie (span 0 -> both 100), so P_A's contributions are exactly
	// weight*100 on both terms; distribution is what separates them.
	snapshot := costSnapshot()
	run := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(4), nil, 2, 0)
	ssn := openSession(snapshot, run, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	candidates := []*api.CandidatePlan{candidate("a3"), candidate("b1")}
	scores := scoreFor(ssn, candidates)

	progress, ok := findTerm(scores[0], "nodeBlockProgress")
	if !ok {
		t.Fatal("nodeBlockProgress term missing")
	}
	distribution, ok := findTerm(scores[0], "nodeBlockDistribution")
	if !ok {
		t.Fatal("nodeBlockDistribution term missing")
	}
	if want := weightNodeBlockProgress * 100; progress.Contribution != want {
		t.Errorf("progress contribution=%d, want %d (weight*100 fits int64)", progress.Contribution, want)
	}
	if want := weightNodeBlockDistribution * 100; distribution.Contribution != want {
		t.Errorf("distribution contribution=%d, want %d", distribution.Contribution, want)
	}
	wantTotal := weightNodeBlockProgress*100 + weightNodeBlockDistribution*100
	if scores[0].Total != wantTotal {
		t.Errorf("total=%d, want %d", scores[0].Total, wantTotal)
	}
}

func TestRequiresDomainCapabilityAndInDefaultPluginList(t *testing.T) {
	requires := framework.PluginRequires(Name)
	if len(requires) != 1 || requires[0] != framework.CapabilityDomain {
		t.Errorf("PluginRequires(%q)=%v, want [domain]", Name, requires)
	}
	found := false
	for _, option := range conf.DefaultPluginOptions() {
		if option.Name == Name {
			found = true
		}
	}
	if !found {
		t.Errorf("default plugin options %v must include %q", conf.DefaultPluginOptions(), Name)
	}
}

func TestValidateArgumentsRejectsNegativeAndUnknownWeights(t *testing.T) {
	valid := framework.Arguments{
		argNodeBlockProgressWeight:     int64(500000),
		argNodeBlockDistributionWeight: int64(50),
	}
	if err := validateArguments(valid); err != nil {
		t.Errorf("valid arguments rejected: %v", err)
	}

	// zero disables a term but is a valid configuration.
	zero := framework.Arguments{argNodeBlockProgressWeight: int64(0), argNodeBlockDistributionWeight: int64(0)}
	if err := validateArguments(zero); err != nil {
		t.Errorf("zero weights must be valid: %v", err)
	}

	for _, tc := range []struct {
		name string
		args framework.Arguments
	}{
		{"negative progress weight", framework.Arguments{argNodeBlockProgressWeight: int64(-1)}},
		{"negative distribution weight", framework.Arguments{argNodeBlockDistributionWeight: int64(-1)}},
		{"unknown key", framework.Arguments{"nodeBlockProgressWeigth": int64(1)}},
		{"fractional weight", framework.Arguments{argNodeBlockProgressWeight: float64(1.5)}},
	} {
		if err := validateArguments(tc.args); err == nil {
			t.Errorf("%s: expected rejection, got nil", tc.name)
		}
		if err := framework.ValidatePluginArguments(Name, tc.args); err == nil {
			t.Errorf("%s: registry validator must reject too", tc.name)
		}
	}
}

// A zero weight disables the term through the real OpenSession path.
func TestZeroWeightsDisableScoreTerms(t *testing.T) {
	snapshot := topologySnapshot{
		nodes:            []*schedapi.NodeInfo{topologyNode("a1", 8, 4)},
		hyperNodesByTier: map[int]sets.Set[string]{2: sets.New[string]("hnA")},
		realNodesSet:     map[string]sets.Set[string]{"hnA": sets.New[string]("a1")},
	}
	run := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(2), nil, 4, 0)
	ssn := openSession(snapshot, run, []framework.PluginOption{{
		Name: Name,
		Arguments: framework.Arguments{
			argNodeBlockProgressWeight:     int64(0),
			argNodeBlockDistributionWeight: int64(0),
		},
	}})
	defer framework.CloseSession(ssn)

	scores := scoreFor(ssn, []*api.CandidatePlan{candidate("a1")})
	if len(scores[0].Terms) != 0 {
		t.Errorf("all weights zero -> terms=%v, want none", scores[0].Terms)
	}
}

// planningCandidate wraps a plan into the read-only candidate view plugins receive.
func planningCandidate(plan *api.CandidatePlan) *framework.PlanningCandidate {
	return &framework.PlanningCandidate{Plan: plan}
}

// receiver builds a receiver candidate over an anchorSnapshot node. The block
// preference reads only HyperNode membership; StaysOccupied is for key-order tests.
func receiver(name string, staysOccupied bool) *framework.ReceiverCandidate {
	return &framework.ReceiverCandidate{
		Node:              topologyNode(name, 8, 4),
		StaysOccupied:     staysOccupied,
		AvailableResource: 4,
	}
}

// preserveTerm extracts the nodeBlockPreserve term, failing when unregistered.
func preserveTerm(t *testing.T, ordered framework.OrderedReceiver) framework.ReceiverPreference {
	t.Helper()
	for _, term := range ordered.Terms {
		if term.Name == "nodeBlockPreserve" {
			return term.Values
		}
	}
	t.Fatalf("receiver %s has no nodeBlockPreserve term (terms=%v)", ordered.Receiver.Node.Name, ordered.Terms)
	return framework.ReceiverPreference{}
}

func orderedNames(ordered []framework.OrderedReceiver) []string {
	names := make([]string, len(ordered))
	for i, r := range ordered {
		names[i] = r.Receiver.Node.Name
	}
	return names
}

// The three preference classes order no-HyperNode > other HyperNode > own
// HyperNode, keyed off the candidate's anchor (a1 -> hnA).
func TestNodeBlockReceiverPreferenceOrdersByHyperNode(t *testing.T) {
	snapshot := anchorSnapshot()
	run := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(2), nil, 4, 0)
	ssn := openSession(snapshot, run, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	ordered := ssn.OrderReceivers(
		planningCandidate(candidate("a1")),
		[]*framework.ReceiverCandidate{receiver("a2", false), receiver("b1", false), receiver("outside", false)},
	)
	wantOrder := []string{"outside", "b1", "a2"}
	wantValues := []framework.ReceiverPreference{{3}, {2}, {1}}
	if len(ordered) != len(wantOrder) {
		t.Fatalf("ordered=%d receivers, want %d", len(ordered), len(wantOrder))
	}
	for i, want := range wantOrder {
		if ordered[i].Receiver.Node.Name != want {
			t.Errorf("position %d receiver=%s, want %s (order=%v)", i, ordered[i].Receiver.Node.Name, want, orderedNames(ordered))
		}
		if got := preserveTerm(t, ordered[i]); got != wantValues[i] {
			t.Errorf("receiver %s preference=%v, want %v", want, got, wantValues[i])
		}
	}
}

// An empty incremental move set makes every receiver abstain ({}), so the sort is
// stable and the input order is preserved — never a mis-ordering.
func TestNodeBlockReceiverPreferenceAbstainsWithoutAnchor(t *testing.T) {
	snapshot := anchorSnapshot()
	run := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(2), nil, 4, 0)
	ssn := openSession(snapshot, run, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	receivers := []*framework.ReceiverCandidate{receiver("a2", false), receiver("outside", false)}
	ordered := ssn.OrderReceivers(planningCandidate(api.NewCandidatePlan(nil, nil)), receivers)
	if len(ordered) != len(receivers) {
		t.Fatalf("ordered=%d receivers, want %d", len(ordered), len(receivers))
	}
	for i, r := range ordered {
		if r.Receiver.Node.Name != receivers[i].Node.Name {
			t.Errorf("position %d receiver=%s, want stable input %s", i, r.Receiver.Node.Name, receivers[i].Node.Name)
		}
		if got := preserveTerm(t, r); got != (framework.ReceiverPreference{}) {
			t.Errorf("receiver %s preference=%v, want abstain {}", r.Receiver.Node.Name, got)
		}
	}
}

// An anchor outside the tier has no "own HyperNode" to protect, so every in-tier
// receiver is "another HyperNode" ({2}); a no-H receiver outranks them ({3}).
func TestNodeBlockReceiverPreferenceAnchorOutsideTier(t *testing.T) {
	snapshot := anchorSnapshot()
	run := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(2), nil, 4, 0)
	ssn := openSession(snapshot, run, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	ordered := ssn.OrderReceivers(
		planningCandidate(candidate("outside")),
		[]*framework.ReceiverCandidate{receiver("a2", false), receiver("outside", false)},
	)
	if len(ordered) != 2 {
		t.Fatalf("ordered=%d receivers, want 2", len(ordered))
	}
	if ordered[0].Receiver.Node.Name != "outside" {
		t.Errorf("first receiver=%s, want outside", ordered[0].Receiver.Node.Name)
	}
	if got := preserveTerm(t, ordered[0]); got != (framework.ReceiverPreference{3}) {
		t.Errorf("outside preference=%v, want {3}", got)
	}
	if got := preserveTerm(t, ordered[1]); got != (framework.ReceiverPreference{2}) {
		t.Errorf("in-tier receiver preference=%v, want {2} (anchor outside tier)", got)
	}
}

// Dormancy: without networkTopology no receiver preference is registered.
func TestNodeBlockReceiverPreferenceRegistersOnlyWithTopology(t *testing.T) {
	snapshot := anchorSnapshot()
	ssn := openSession(snapshot, &repackv1alpha1.RepackRun{}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	ordered := ssn.OrderReceivers(
		planningCandidate(candidate("a1")),
		[]*framework.ReceiverCandidate{receiver("outside", false)},
	)
	if len(ordered) != 1 {
		t.Fatalf("ordered=%d receivers, want 1", len(ordered))
	}
	if len(ordered[0].Terms) != 0 {
		t.Errorf("no topology -> terms=%v, want none", ordered[0].Terms)
	}
}

// staysOccupied (Stability) sorts before nodeBlockPreserve (Topology): a stays-
// occupied own-H receiver wins over a drainable no-H one, pinning the design's
// "only loses to staysOccupied" invariant.
func TestNodeBlockReceiverPreferenceSitsAfterStaysOccupied(t *testing.T) {
	snapshot := anchorSnapshot()
	run := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(2), nil, 4, 0)
	ssn := openSession(snapshot, run, framework.PluginOptions(Name, "binpack"))
	defer framework.CloseSession(ssn)

	ownStays := receiver("a2", true)      // hnA own-H, stays-occupied
	noHFree := receiver("outside", false) // no-H, drainable
	ordered := ssn.OrderReceivers(
		planningCandidate(candidate("a1")),
		[]*framework.ReceiverCandidate{ownStays, noHFree},
	)
	if len(ordered) != 2 {
		t.Fatalf("ordered=%d receivers, want 2", len(ordered))
	}
	if ordered[0].Receiver.Node.Name != "a2" {
		t.Errorf("first receiver=%s, want stays-occupied a2: staysOccupied must precede the block preference", ordered[0].Receiver.Node.Name)
	}
	// The block preference alone would pick the no-H node, so prove both values: the
	// order above comes from the Stability key, not a tie.
	if got := preserveTerm(t, ordered[0]); got != (framework.ReceiverPreference{1}) {
		t.Errorf("a2 block preference=%v, want {1}", got)
	}
	if got := preserveTerm(t, ordered[1]); got != (framework.ReceiverPreference{3}) {
		t.Errorf("outside block preference=%v, want {3}", got)
	}
}

// A co-placement group's victims can span several HyperNodes: the anchor-H set
// treats every member's HyperNode as "own", so a receiver in hnB is {1}, not {2}.
func TestNodeBlockReceiverPreferenceMultiAnchorSet(t *testing.T) {
	snapshot := anchorSnapshot()
	run := topologyRun(repackv1alpha1.RepackBlockModeBinpack, intPtr(2), nil, 4, 0)
	ssn := openSession(snapshot, run, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	plan := api.NewCandidatePlan(nil, []*api.Move{{From: "a1"}, {From: "b1"}})
	ordered := ssn.OrderReceivers(
		planningCandidate(plan),
		[]*framework.ReceiverCandidate{receiver("b1", false), receiver("outside", false)},
	)
	if len(ordered) != 2 {
		t.Fatalf("ordered=%d receivers, want 2", len(ordered))
	}
	var b1Pref framework.ReceiverPreference
	for _, r := range ordered {
		switch r.Receiver.Node.Name {
		case "b1":
			b1Pref = preserveTerm(t, r)
		case "outside":
			if got := preserveTerm(t, r); got != (framework.ReceiverPreference{3}) {
				t.Errorf("outside preference=%v, want {3}", got)
			}
		}
	}
	if b1Pref != (framework.ReceiverPreference{1}) {
		t.Errorf("hnB receiver (group member) preference=%v, want {1}: the anchor-H set must protect every member H, not anchor[0] only", b1Pref)
	}
}
