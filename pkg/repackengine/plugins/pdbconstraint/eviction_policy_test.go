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

package pdbconstraint

import (
	"errors"
	"testing"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/framework"
)

type policySnapshot struct {
	*pdbSnapshot
	annotations map[schedapi.JobID]map[string]string
	calls       map[schedapi.JobID]int
}

func (s *policySnapshot) PodGroupAnnotations(id schedapi.JobID) (map[string]string, bool) {
	if s.calls == nil {
		s.calls = make(map[schedapi.JobID]int)
	}
	s.calls[id]++
	annotations, found := s.annotations[id]
	if !found {
		return nil, false
	}
	copy := make(map[string]string, len(annotations))
	for key, value := range annotations {
		copy[key] = value
	}
	return copy, true
}

func TestCompileEvictionPolicy(t *testing.T) {
	tests := []struct {
		name                string
		raw                 string
		wantErr             bool
		wantPodGroupBlocked bool
		wantSubgroups       map[string]bool
	}{
		{
			name: "pod group and role gates",
			raw: `{
				"apiVersion":"scheduling.volcano.sh/v1alpha1",
				"podGroup":{"disruptionsAllowed":1},
				"subgroups":{
					"prefill":{"disruptionsAllowed":0},
					"decode":{"disruptionsAllowed":2}
				}
			}`,
			wantSubgroups: map[string]bool{"prefill": true, "decode": false},
		},
		{
			name:                "pod group blocked",
			raw:                 `{"apiVersion":"scheduling.volcano.sh/v1alpha1","podGroup":{"disruptionsAllowed":0}}`,
			wantPodGroupBlocked: true,
			wantSubgroups:       map[string]bool{},
		},
		{
			name:    "malformed JSON",
			raw:     `{`,
			wantErr: true,
		},
		{
			name:    "unsupported version",
			raw:     `{"apiVersion":"scheduling.volcano.sh/v1beta1"}`,
			wantErr: true,
		},
		{
			name:    "missing allowance",
			raw:     `{"apiVersion":"scheduling.volcano.sh/v1alpha1","podGroup":{}}`,
			wantErr: true,
		},
		{
			name:    "negative allowance",
			raw:     `{"apiVersion":"scheduling.volcano.sh/v1alpha1","subgroups":{"prefill":{"disruptionsAllowed":-1}}}`,
			wantErr: true,
		},
		{
			name:    "empty subgroup",
			raw:     `{"apiVersion":"scheduling.volcano.sh/v1alpha1","subgroups":{"":{"disruptionsAllowed":0}}}`,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled, err := compileEvictionPolicy(test.raw)
			if (err != nil) != test.wantErr {
				t.Fatalf("compileEvictionPolicy() error=%v, wantErr=%t", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			if compiled.podGroupBlocked != test.wantPodGroupBlocked {
				t.Errorf("podGroupBlocked=%t, want %t", compiled.podGroupBlocked, test.wantPodGroupBlocked)
			}
			if len(compiled.subgroups) != len(test.wantSubgroups) {
				t.Fatalf("subgroups=%v, want %v", compiled.subgroups, test.wantSubgroups)
			}
			for subgroup, wantBlocked := range test.wantSubgroups {
				if got, found := compiled.subgroups[subgroup]; !found || got != wantBlocked {
					t.Errorf("subgroup %q blocked=%t found=%t, want blocked=%t", subgroup, got, found, wantBlocked)
				}
			}
		})
	}
}

func TestBlockingEvictionPolicy(t *testing.T) {
	policy, err := compileEvictionPolicy(`{
		"apiVersion":"scheduling.volcano.sh/v1alpha1",
		"podGroup":{"disruptionsAllowed":1},
		"subgroups":{
			"prefill":{"disruptionsAllowed":0},
			"decode":{"disruptionsAllowed":1}
		}
	}`)
	if err != nil {
		t.Fatalf("compile policy: %v", err)
	}
	blockedPodGroup, err := compileEvictionPolicy(`{
		"apiVersion":"scheduling.volcano.sh/v1alpha1",
		"podGroup":{"disruptionsAllowed":0},
		"subgroups":{"decode":{"disruptionsAllowed":1}}
	}`)
	if err != nil {
		t.Fatalf("compile blocked PodGroup policy: %v", err)
	}

	tests := []struct {
		name       string
		task       *schedapi.TaskInfo
		policy     *compiledEvictionPolicy
		wantBlock  bool
		wantReason string
	}{
		{name: "nil policy", task: modelServingTask("a", "prefill"), policy: nil},
		{name: "pod group zero blocks every role", task: modelServingTask("a", "decode"), policy: blockedPodGroup, wantBlock: true, wantReason: podGroupZeroDisruptionReason},
		{name: "blocked ModelServing role", task: modelServingTask("a", "prefill"), policy: policy, wantBlock: true, wantReason: subgroupZeroDisruptionReason},
		{name: "allowed ModelServing role", task: modelServingTask("a", "decode"), policy: policy},
		{name: "undeclared ModelServing role has no subgroup constraint", task: modelServingTask("a", "other"), policy: policy},
		{name: "missing role has no subgroup constraint", task: testTask("a", "ns", nil, true, true), policy: policy},
		{name: "Volcano task role fallback", task: taskWithRole("a", "decode"), policy: policy},
		{name: "invalid policy", task: modelServingTask("a", "decode"), policy: &compiledEvictionPolicy{validationError: errors.New("invalid")}, wantBlock: true, wantReason: invalidEvictionPolicyReason},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info, blocked := blockingEvictionPolicy(test.task, test.policy)
			if blocked != test.wantBlock {
				t.Fatalf("blocked=%t, want %t", blocked, test.wantBlock)
			}
			if info.reason != test.wantReason {
				t.Errorf("reason=%q, want %q", info.reason, test.wantReason)
			}
		})
	}
}

func TestPluginUsesEvictionPolicyWhenPDBReaderFails(t *testing.T) {
	prefill := modelServingTask("prefill", "prefill")
	decode := modelServingTask("decode", "decode")
	snapshot := &policySnapshot{
		pdbSnapshot: &pdbSnapshot{
			snapshotView: &snapshotView{nodes: []*schedapi.NodeInfo{{Tasks: schedapi.TasksMap{
				prefill.UID: prefill,
				decode.UID:  decode,
			}}}},
			err: errors.New("PDB lister unavailable"),
		},
		annotations: map[schedapi.JobID]map[string]string{
			"ns/pg": {
				evictionPolicyAnnotationKey: `{
					"apiVersion":"scheduling.volcano.sh/v1alpha1",
					"podGroup":{"disruptionsAllowed":1},
					"subgroups":{
						"prefill":{"disruptionsAllowed":0}
					}
				}`,
			},
		},
	}

	ssn := framework.OpenSession(framework.SessionConfig{Snapshot: snapshot, Resource: testResource}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	if ssn.Movable()(prefill) {
		t.Fatal("prefill task must be blocked by the ModelServing subgroup policy")
	}
	if !ssn.Movable()(decode) {
		t.Fatal("decode task must remain movable when its subgroup allowance is positive")
	}
	if got := snapshot.calls["ns/pg"]; got != 1 {
		t.Fatalf("PodGroupAnnotations calls=%d, want one lookup per PodGroup", got)
	}
}

func TestPluginInvalidEvictionPolicyFailsClosed(t *testing.T) {
	task := modelServingTask("a", "prefill")
	snapshot := &policySnapshot{
		pdbSnapshot: &pdbSnapshot{
			snapshotView: &snapshotView{nodes: []*schedapi.NodeInfo{{Tasks: schedapi.TasksMap{task.UID: task}}}},
		},
		annotations: map[schedapi.JobID]map[string]string{
			"ns/pg": {evictionPolicyAnnotationKey: `{"apiVersion":"unsupported"}`},
		},
	}

	ssn := framework.OpenSession(framework.SessionConfig{Snapshot: snapshot, Resource: testResource}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	if ssn.Movable()(task) {
		t.Fatal("a task with a present but invalid eviction policy must fail closed")
	}
}

func modelServingTask(name, role string) *schedapi.TaskInfo {
	return testTask(name, "ns", map[string]string{
		modelServingNameLabel:      "sample",
		modelServingGroupNameLabel: "sample-0",
		modelServingRoleLabel:      role,
	}, true, true)
}

func taskWithRole(name, role string) *schedapi.TaskInfo {
	task := testTask(name, "ns", nil, true, true)
	task.TaskRole = role
	return task
}
