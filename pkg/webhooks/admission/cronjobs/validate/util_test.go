/*
Copyright 2021 The Volcano Authors.

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

package validate

import (
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

func TestTopoSort(t *testing.T) {
	testCases := []struct {
		name        string
		Spec        *v1alpha1.JobSpec
		sortedTasks []string
		isDag       bool
	}{
		{
			name: "test-1",
			Spec: &v1alpha1.JobSpec{
				Tasks: []v1alpha1.TaskSpec{
					{
						Name: "t1",
						DependsOn: &v1alpha1.DependsOn{
							Name: []string{"t2", "t3"},
						},
					},
					{
						Name: "t2",
						DependsOn: &v1alpha1.DependsOn{
							Name: []string{"t3"},
						},
					},
					{
						Name: "t3",
						DependsOn: &v1alpha1.DependsOn{
							Name: []string{},
						},
					},
				},
			},
			sortedTasks: []string{"t3", "t2", "t1"},
			isDag:       true,
		},
		{
			name: "test-2",
			Spec: &v1alpha1.JobSpec{
				Tasks: []v1alpha1.TaskSpec{
					{
						Name: "t1",
						DependsOn: &v1alpha1.DependsOn{
							Name: []string{"t2"},
						},
					},
					{
						Name: "t2",
						DependsOn: &v1alpha1.DependsOn{
							Name: []string{"t1"},
						},
					},
					{
						Name:      "t3",
						DependsOn: nil,
					},
				},
			},
			sortedTasks: nil,
			isDag:       false,
		},
		{
			name: "test-3",
			Spec: &v1alpha1.JobSpec{
				Tasks: []v1alpha1.TaskSpec{
					{
						Name: "t1",
						DependsOn: &v1alpha1.DependsOn{
							Name: []string{"t2", "t3"},
						},
					},
					{
						Name: "t2",
						DependsOn: &v1alpha1.DependsOn{
							Name: []string{"t2"},
						},
					},
					{
						Name:      "t3",
						DependsOn: nil,
					},
				},
			},
			sortedTasks: nil,
			isDag:       false,
		},
		{
			name: "test-4",
			Spec: &v1alpha1.JobSpec{
				Tasks: []v1alpha1.TaskSpec{
					{
						Name: "t1",
						DependsOn: &v1alpha1.DependsOn{
							Name: []string{"t2", "t3"},
						},
					},
					{
						Name:      "t2",
						DependsOn: nil,
					},
					{
						Name: "t3",
						DependsOn: &v1alpha1.DependsOn{
							Name: []string{"t2"},
						},
					},
				},
			},
			sortedTasks: []string{"t2", "t3", "t1"},
			isDag:       true,
		},
	}

	for _, testcase := range testCases {
		tasks, isDag := topoSort(testcase.Spec)
		if isDag != testcase.isDag || !equality.Semantic.DeepEqual(tasks, testcase.sortedTasks) {
			t.Errorf("%s failed, expect sortedTasks: %v, got: %v, expected isDag: %v, got: %v",
				testcase.name, testcase.sortedTasks, tasks, testcase.isDag, isDag)
		}
	}
}
func TestValidateTaskTopoPolicy(t *testing.T) {
	testCases := []struct {
		name     string
		taskSpec v1alpha1.TaskSpec
		expect   string
	}{
		{
			name: "test-1",
			taskSpec: v1alpha1.TaskSpec{
				Name:           "task-1",
				Replicas:       5,
				TopologyPolicy: v1alpha1.Restricted,
				Template: v1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"name": "test"},
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Resources: v1.ResourceRequirements{
									Limits: v1.ResourceList{
										v1.ResourceCPU:    *resource.NewQuantity(1, ""),
										v1.ResourceMemory: *resource.NewQuantity(2000, resource.BinarySI),
									},
								},
							},
						},
					},
				},
			},
			expect: "",
		},
		{
			name: "test-2",
			taskSpec: v1alpha1.TaskSpec{
				Name:           "task-2",
				TopologyPolicy: v1alpha1.Restricted,
				Template: v1.PodTemplateSpec{
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Resources: v1.ResourceRequirements{
									Limits: v1.ResourceList{
										v1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
										v1.ResourceMemory: *resource.NewQuantity(2000, resource.BinarySI),
									},
								},
							},
						},
					},
				},
			},
			expect: "the cpu request isn't  an integer",
		},
	}

	for _, testcase := range testCases {
		msg := validateTaskTopoPolicy(testcase.taskSpec, 0)
		if !strings.Contains(msg, testcase.expect) {
			t.Errorf("%s failed.", testcase.name)
		}
	}
}
func TestValidateCronJobName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "valid name",
			input:    "my-cronjob",
			expected: "",
		},
		{
			name:     "exactly max length",
			input:    strings.Repeat("a", 52),
			expected: "",
		},
		{
			name:     "too long name",
			input:    strings.Repeat("a", 65),
			expected: "must be no more than 52 characters",
		},
		{
			name:     "invalid chars",
			input:    "-UPPERCASE",
			expected: "invalid cronJob name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := validateCronJobName(tt.input)
			if tt.expected == "" && msg != "" {
				t.Errorf("Unexpected error: %q", msg)
			} else if tt.expected != "" && !strings.Contains(msg, tt.expected) {
				t.Errorf("Expected %q in error, got %q", tt.expected, msg)
			}
		})
	}
}
