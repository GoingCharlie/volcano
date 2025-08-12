package mutate

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

// TODO wcx patchDefaultScheduler can not run in windows, try Linux
func TestCreatePatchExecution(t *testing.T) {

	namespace := "test"

	testCase := struct {
		Name      string
		Job       v1alpha1.Job
		operation patchOperation
	}{
		Name: "patch default task name",
		Job: v1alpha1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "path-task-name",
				Namespace: namespace,
			},
			Spec: v1alpha1.JobSpec{
				MinAvailable: 1,
				Tasks: []v1alpha1.TaskSpec{
					{
						Replicas: 1,
						Template: v1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"name": "test"},
							},
							Spec: v1.PodSpec{
								Containers: []v1.Container{
									{
										Name:  "fake-name",
										Image: "busybox:1.24",
									},
								},
							},
						},
					},
					{
						Replicas: 1,
						Template: v1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"name": "test"},
							},
							Spec: v1.PodSpec{
								Containers: []v1.Container{
									{
										Name:  "fake-name",
										Image: "busybox:1.24",
									},
								},
							},
						},
					},
				},
			},
		},
		operation: patchOperation{
			Op:   "replace",
			Path: "/spec/tasks",
			Value: []v1alpha1.TaskSpec{
				{
					Name:     v1alpha1.DefaultTaskSpec + "0",
					Replicas: 1,
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"name": "test"},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "fake-name",
									Image: "busybox:1.24",
								},
							},
						},
					},
				},
				{
					Name:     v1alpha1.DefaultTaskSpec + "1",
					Replicas: 1,
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"name": "test"},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "fake-name",
									Image: "busybox:1.24",
								},
							},
						},
					},
				},
			},
		},
	}

	ret := mutateSpec(testCase.Job.Spec.Tasks, "/spec/tasks")
	if ret.Path != testCase.operation.Path || ret.Op != testCase.operation.Op {
		t.Errorf("testCase %s's expected patch operation %v, but got %v",
			testCase.Name, testCase.operation, *ret)
	}

	actualTasks, ok := ret.Value.([]v1alpha1.TaskSpec)
	if !ok {
		t.Errorf("testCase '%s' path value expected to be '[]v1alpha1.TaskSpec', but negative",
			testCase.Name)
	}
	expectedTasks, _ := testCase.operation.Value.([]v1alpha1.TaskSpec)
	for index, task := range expectedTasks {
		aTask := actualTasks[index]
		if aTask.Name != task.Name {
			t.Errorf("testCase '%s's expected patch operation with value %v, but got %v",
				testCase.Name, testCase.operation.Value, ret.Value)
		}
		if aTask.MaxRetry != defaultMaxRetry {
			t.Errorf("testCase '%s's expected patch 'task.MaxRetry' with value %v, but got %v",
				testCase.Name, defaultMaxRetry, aTask.MaxRetry)
		}
	}

}
func TestPatchConcurrencyPolicy(t *testing.T) {
	tests := []struct {
		name     string
		input    v1alpha1.ConcurrencyPolicy
		expected interface{}
	}{
		{"zero value", "", v1alpha1.AllowConcurrent},
		{"custom value", "Allow", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cj := &v1alpha1.CronJob{
				Spec: v1alpha1.CronJobSpec{
					ConcurrencyPolicy: tt.input,
				},
			}
			patch := patchConcurrencyPolicy(cj)
			if tt.expected == nil {
				require.Nil(t, patch)
			} else {
				require.Equal(t, tt.expected, patch.Value)
			}
		})
	}
}
func TestPatchFailedJobsHistoryLimit(t *testing.T) {
	tests := []struct {
		name     string
		input    *int32
		expected interface{}
	}{
		{"nil pointer", nil, ptr.To(int32(1))},
		{"zero value", ptr.To(int32(0)), nil},
		{"custom value", ptr.To(int32(5)), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cj := &v1alpha1.CronJob{
				Spec: v1alpha1.CronJobSpec{
					FailedJobsHistoryLimit: tt.input,
				},
			}
			patch := patchFailedJobsHistoryLimit(cj)
			if tt.expected == nil {
				require.Nil(t, patch)
			} else {
				require.Equal(t, tt.expected, patch.Value)
			}
		})
	}
}
func TestPatchSuccessfulJobsHistoryLimit(t *testing.T) {
	tests := []struct {
		name     string
		input    *int32
		expected interface{}
	}{
		{"nil pointer", nil, ptr.To(int32(3))},
		{"zero value", ptr.To(int32(0)), nil},
		{"custom value", ptr.To(int32(5)), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cj := &v1alpha1.CronJob{
				Spec: v1alpha1.CronJobSpec{
					SuccessfulJobsHistoryLimit: tt.input,
				},
			}
			patch := patchSuccessfulJobsHistoryLimit(cj)
			if tt.expected == nil {
				require.Nil(t, patch)
			} else {
				require.Equal(t, tt.expected, patch.Value)
			}
		})
	}
}
func TestPatchSuspend(t *testing.T) {
	tests := []struct {
		name     string
		input    *bool
		expected interface{}
	}{
		{"nil pointer", nil, ptr.To(false)},
		{"custom value", ptr.To(true), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cj := &v1alpha1.CronJob{
				Spec: v1alpha1.CronJobSpec{
					Suspend: tt.input,
				},
			}
			patch := patchSuspend(cj)
			if tt.expected == nil {
				require.Nil(t, patch)
			} else {
				require.Equal(t, tt.expected, patch.Value)
			}
		})
	}
}
func TestCreatePatch_DefaultValues(t *testing.T) {
	cronjob := &v1alpha1.CronJob{
		Spec: v1alpha1.CronJobSpec{},
	}

	patchBytes, err := createPatch(cronjob)
	require.NoError(t, err)

	var patches []patchOperation
	require.NoError(t, json.Unmarshal(patchBytes, &patches))

	expectedPatches := []patchOperation{
		{Op: "add", Path: "/spec/concurrencyPolicy", Value: "Allow"},
		{Op: "add", Path: "/spec/successfulJobsHistoryLimit", Value: float64(3)},
		{Op: "add", Path: "/spec/failedJobsHistoryLimit", Value: float64(1)},
		{Op: "add", Path: "/spec/suspend", Value: false},
		{Op: "add", Path: "/spec/jobTemplate/spec/queue", Value: "default"},
		{Op: "add", Path: "/spec/jobTemplate/spec/maxRetry", Value: float64(3)},
		{Op: "add", Path: "/spec/jobTemplate/spec/minAvailable", Value: float64(0)},
	}
	require.ElementsMatch(t, expectedPatches, patches)
}
