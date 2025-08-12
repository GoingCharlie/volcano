package validate

import (
	"fmt"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"volcano.sh/apis/pkg/apis/batch/v1alpha1"
	busv1alpha1 "volcano.sh/apis/pkg/apis/bus/v1alpha1"
	schedulingv1beta2 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	fakeclient "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	informers "volcano.sh/apis/pkg/client/informers/externalversions"
)

func TestValidateJobSpec(t *testing.T) {
	var invTTL int32 = -1
	var policyExitCode int32 = -1
	var invMinAvailable int32 = -1
	namespace := "test"
	privileged := true

	testCases := []struct {
		Name           string
		Job            v1alpha1.Job
		ExpectErr      bool
		reviewResponse admissionv1.AdmissionResponse
		ret            string
	}{
		{
			Name: "validate valid-job",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "valid-job",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Policies: []v1alpha1.LifecyclePolicy{
						{
							Event:  busv1alpha1.PodEvictedEvent,
							Action: busv1alpha1.RestartTaskAction,
						},
					},
				},
			},
			ret:       "",
			ExpectErr: false,
		},
		// duplicate task name
		{
			Name: "duplicate-task-job",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "duplicate-task-job",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "duplicated-task-1",
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
							Name:     "duplicated-task-1",
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
			ret:       "duplicated task name duplicated-task-1",
			ExpectErr: true,
		},
		// Duplicated Policy Event
		{
			Name: "job-policy-duplicated",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-policy-duplicated",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Policies: []v1alpha1.LifecyclePolicy{
						{
							Event:  busv1alpha1.PodFailedEvent,
							Action: busv1alpha1.AbortJobAction,
						},
						{
							Event:  busv1alpha1.PodFailedEvent,
							Action: busv1alpha1.RestartJobAction,
						},
					},
				},
			},
			ret:       "duplicate",
			ExpectErr: true,
		},
		// Min Available illegal
		{
			Name: "Min Available illegal",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-min-illegal",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 2,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
			ret:       "'minAvailable' should not be greater than total replicas in tasks",
			ExpectErr: true,
		},
		// Job Plugin illegal
		{
			Name: "Job Plugin illegal",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-plugin-illegal",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Plugins: map[string][]string{
						"big_plugin": {},
					},
				},
			},
			ret:       "unable to find job plugin: big_plugin",
			ExpectErr: true,
		},
		// ttl-illegal
		{
			Name: "job-ttl-illegal",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-ttl-illegal",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					TTLSecondsAfterFinished: &invTTL,
				},
			},
			ret:       "'ttlSecondsAfterFinished' cannot be less than zero",
			ExpectErr: true,
		},
		// min-MinAvailable less than zero
		{
			Name: "minAvailable-lessThanZero",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "minAvailable-lessThanZero",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: -1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
			ret:       "'minAvailable' must be >= 0",
			ExpectErr: true,
		},
		// maxretry less than zero
		{
			Name: "maxretry-lessThanZero",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "maxretry-lessThanZero",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					MaxRetry:     -1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
			ret:       "'maxRetry' cannot be less than zero.",
			ExpectErr: true,
		},
		// no task specified in the job
		{
			Name: "no-task",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-task",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks:        []v1alpha1.TaskSpec{},
				},
			},
			ret:       "No task specified in job spec",
			ExpectErr: true,
		},
		// replica set less than zero
		{
			Name: "replica-lessThanZero",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "replica-lessThanZero",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
							Replicas: -1,
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
			ret:       "'replicas' < 0 in task: task-1; job 'minAvailable' should not be greater than total replicas in tasks;",
			ExpectErr: true,
		},
		// task minAvailable set less than zero
		{
			Name: "replica-lessThanZero",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "taskMinAvailable-lessThanZero",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:         "task-1",
							Replicas:     1,
							MinAvailable: &invMinAvailable,
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
			ret:       "'minAvailable' < 0 in task: task-1;",
			ExpectErr: true,
		},
		// task name error
		{
			Name: "nonDNS-task",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "replica-lessThanZero",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "Task-1",
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
			ret:       "[a lowercase RFC 1123 label must consist of lower case alphanumeric characters or '-', and must start and end with an alphanumeric character (e.g. 'my-name',  or '123-abc', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?')];",
			ExpectErr: true,
		},
		// Policy Event with exit code
		{
			Name: "job-policy-withExitCode",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-policy-withExitCode",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Policies: []v1alpha1.LifecyclePolicy{
						{
							Event:    busv1alpha1.PodFailedEvent,
							Action:   busv1alpha1.AbortJobAction,
							ExitCode: &policyExitCode,
						},
					},
				},
			},
			ret:       "must not specify event and exitCode simultaneously",
			ExpectErr: true,
		},
		// Both policy event and exit code are nil
		{
			Name: "policy-noEvent-noExCode",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "policy-noEvent-noExCode",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Policies: []v1alpha1.LifecyclePolicy{
						{
							Action: busv1alpha1.AbortJobAction,
						},
					},
				},
			},
			ret:       "either event and exitCode should be specified",
			ExpectErr: true,
		},
		// invalid policy event
		{
			Name: "invalid-policy-event",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-policy-event",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Policies: []v1alpha1.LifecyclePolicy{
						{
							Event:  busv1alpha1.Event("someFakeEvent"),
							Action: busv1alpha1.AbortJobAction,
						},
					},
				},
			},
			ret:       "invalid policy event",
			ExpectErr: true,
		},
		// invalid policy action
		{
			Name: "invalid-policy-action",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-policy-action",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Policies: []v1alpha1.LifecyclePolicy{
						{
							Event:  busv1alpha1.PodEvictedEvent,
							Action: busv1alpha1.Action("someFakeAction"),
						},
					},
				},
			},
			ret:       "invalid policy action",
			ExpectErr: true,
		},
		// policy exit-code zero
		{
			Name: "policy-extcode-zero",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "policy-extcode-zero",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Policies: []v1alpha1.LifecyclePolicy{
						{
							Action: busv1alpha1.AbortJobAction,
							ExitCode: func(i int32) *int32 {
								return &i
							}(int32(0)),
						},
					},
				},
			},
			ret:       "0 is not a valid error code",
			ExpectErr: true,
		},
		// duplicate policy exit-code
		{
			Name: "duplicate-exitcode",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "duplicate-exitcode",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Policies: []v1alpha1.LifecyclePolicy{
						{
							ExitCode: func(i int32) *int32 {
								return &i
							}(int32(1)),
						},
						{
							ExitCode: func(i int32) *int32 {
								return &i
							}(int32(1)),
						},
					},
				},
			},
			ret:       "duplicate exitCode 1",
			ExpectErr: true,
		},
		// Policy with any event and other events
		{
			Name: "job-policy-withExitCode",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-policy-withExitCode",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Policies: []v1alpha1.LifecyclePolicy{
						{
							Event:  busv1alpha1.AnyEvent,
							Action: busv1alpha1.AbortJobAction,
						},
						{
							Event:  busv1alpha1.PodFailedEvent,
							Action: busv1alpha1.RestartJobAction,
						},
					},
				},
			},
			ret:       "if there's * here, no other policy should be here",
			ExpectErr: true,
		},
		// invalid mount volume
		{
			Name: "invalid-mount-volume",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-mount-volume",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Policies: []v1alpha1.LifecyclePolicy{
						{
							Event:  busv1alpha1.AnyEvent,
							Action: busv1alpha1.AbortJobAction,
						},
					},
					Volumes: []v1alpha1.VolumeSpec{
						{
							MountPath: "",
						},
					},
				},
			},
			ret:       " mountPath is required;",
			ExpectErr: true,
		},
		// duplicate mount volume
		{
			Name: "duplicate-mount-volume",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "duplicate-mount-volume",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Policies: []v1alpha1.LifecyclePolicy{
						{
							Event:  busv1alpha1.AnyEvent,
							Action: busv1alpha1.AbortJobAction,
						},
					},
					Volumes: []v1alpha1.VolumeSpec{
						{
							MountPath:       "/var",
							VolumeClaimName: "pvc1",
						},
						{
							MountPath:       "/var",
							VolumeClaimName: "pvc2",
						},
					},
				},
			},
			ret:       " duplicated mountPath: /var;",
			ExpectErr: true,
		},
		{
			Name: "volume without VolumeClaimName and VolumeClaim",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-volume",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
					Policies: []v1alpha1.LifecyclePolicy{
						{
							Event:  busv1alpha1.AnyEvent,
							Action: busv1alpha1.AbortJobAction,
						},
					},
					Volumes: []v1alpha1.VolumeSpec{
						{
							MountPath: "/var",
						},
						{
							MountPath: "/var",
						},
					},
				},
			},
			ret:       " either VolumeClaim or VolumeClaimName must be specified;",
			ExpectErr: true,
		},
		// task Policy with any event and other events
		{
			Name: "taskpolicy-withAnyandOthrEvent",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "taskpolicy-withAnyandOthrEvent",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
							Policies: []v1alpha1.LifecyclePolicy{
								{
									Event:  busv1alpha1.AnyEvent,
									Action: busv1alpha1.AbortJobAction,
								},
								{
									Event:  busv1alpha1.PodFailedEvent,
									Action: busv1alpha1.RestartJobAction,
								},
							},
						},
					},
				},
			},
			ret:       "if there's * here, no other policy should be here",
			ExpectErr: true,
		},
		// job with no queue created
		{
			Name: "job-with-noQueue",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-with-noQueue",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "jobQueue",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
			ret:       "unable to find job queue",
			ExpectErr: true,
		},
		{
			Name: "job with privileged init container",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "valid-job",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
							Replicas: 1,
							Template: v1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: map[string]string{"name": "test"},
								},
								Spec: v1.PodSpec{
									InitContainers: []v1.Container{
										{
											Name:  "init-fake-name",
											Image: "busybox:1.24",
											SecurityContext: &v1.SecurityContext{
												Privileged: &privileged,
											},
										},
									},
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
			ret:       "",
			ExpectErr: false,
		},
		{
			Name: "job with privileged container",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "valid-job",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
											SecurityContext: &v1.SecurityContext{
												Privileged: &privileged,
											},
										},
									},
								},
							},
						},
					},
				},
			},
			ret:       "",
			ExpectErr: false,
		},
		{
			Name: "job with valid task depends on",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-with-valid-task-depends-on",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "t1",
							Replicas: 1,
							DependsOn: &v1alpha1.DependsOn{
								Name: []string{"t2"},
							},
							Template: v1.PodTemplateSpec{
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
							Name:      "t2",
							Replicas:  1,
							DependsOn: nil,
							Template: v1.PodTemplateSpec{
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
			ret:       "",
			ExpectErr: false,
		},
		{
			Name: "job with invalid task depends on",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-with-invalid-task-depends-on",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "default",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "t1",
							Replicas: 1,
							DependsOn: &v1alpha1.DependsOn{
								Name: []string{"t3"},
							},
							Template: v1.PodTemplateSpec{
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
							Name:      "t2",
							Replicas:  1,
							DependsOn: nil,
							Template: v1.PodTemplateSpec{
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
			ret:       "job has dependencies between tasks, but doesn't form a directed acyclic graph(DAG)",
			ExpectErr: true,
		},
	}

	defaultqueue := &schedulingv1beta2.Queue{
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
		},
		Spec: schedulingv1beta2.QueueSpec{
			Weight: 1,
		},
		Status: schedulingv1beta2.QueueStatus{
			State: schedulingv1beta2.QueueStateOpen,
		},
	}

	// create fake volcano clientset
	config.VolcanoClient = fakeclient.NewSimpleClientset(defaultqueue)
	informerFactory := informers.NewSharedInformerFactory(config.VolcanoClient, 0)
	queueInformer := informerFactory.Scheduling().V1beta1().Queues()
	config.QueueLister = queueInformer.Lister()

	stopCh := make(chan struct{})
	informerFactory.Start(stopCh)
	for informerType, ok := range informerFactory.WaitForCacheSync(stopCh) {
		if !ok {
			panic(fmt.Errorf("failed to sync cache: %v", informerType))
		}
	}
	defer close(stopCh)

	for _, testCase := range testCases {
		t.Run(testCase.Name, func(t *testing.T) {
			ret := validateJobSpec(&testCase.Job.Spec, namespace)
			//fmt.Printf("test-case name:%s, ret:%v  testCase.reviewResponse:%v \n", testCase.Name, ret,testCase.reviewResponse)
			if testCase.ExpectErr == true && ret == "" {
				t.Errorf("Expect error msg :%s, but got nil.", testCase.ret)
			}
			// if testCase.ExpectErr == true && testCase.reviewResponse.Allowed != false {
			// 	t.Errorf("Expect Allowed as false but got true.")
			// }
			if testCase.ExpectErr == true && !strings.Contains(ret, testCase.ret) {
				t.Errorf("Expect error msg :%s, but got diff error %v", testCase.ret, ret)
			}

			if testCase.ExpectErr == false && ret != "" {
				t.Errorf("Expect no error, but got error %v", ret)
			}
			// if testCase.ExpectErr == false && testCase.reviewResponse.Allowed != true {
			// 	t.Errorf("Expect Allowed as true but got false. %v", testCase.reviewResponse)
			// }
		})
	}
}

func TestValidateHierarchyCreate(t *testing.T) {
	namespace := "test"

	testCases := []struct {
		Name           string
		Job            v1alpha1.Job
		ExpectErr      bool
		reviewResponse admissionv1.AdmissionResponse
		ret            string
	}{
		// job with root queue created
		{
			Name: "job-with-rootQueue",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-with-noQueue",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "root",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
			ret:       "can not submit job to root queue",
			ExpectErr: true,
		},
		// job with non leaf queue created
		{
			Name: "job-with-parentQueue",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-with-parentQueue",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "parentQueue",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
			ret:       "can only submit job to leaf queue",
			ExpectErr: true,
		},
		// job with leaf queue created
		{
			Name: "job-with-leafQueue",
			Job: v1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "job-with-leafQueue",
					Namespace: namespace,
				},
				Spec: v1alpha1.JobSpec{
					MinAvailable: 1,
					Queue:        "childQueue",
					Tasks: []v1alpha1.TaskSpec{
						{
							Name:     "task-1",
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
			ret:       "",
			ExpectErr: false,
		},
	}

	rootQueue := &schedulingv1beta2.Queue{
		ObjectMeta: metav1.ObjectMeta{
			Name: "root",
		},
		Spec: schedulingv1beta2.QueueSpec{},
		Status: schedulingv1beta2.QueueStatus{
			State: schedulingv1beta2.QueueStateOpen,
		},
	}
	parentQueue := &schedulingv1beta2.Queue{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parentQueue",
		},
		Spec: schedulingv1beta2.QueueSpec{
			Parent: "root",
		},
		Status: schedulingv1beta2.QueueStatus{
			State: schedulingv1beta2.QueueStateOpen,
		},
	}

	childQueue := &schedulingv1beta2.Queue{
		ObjectMeta: metav1.ObjectMeta{
			Name: "childQueue",
		},
		Spec: schedulingv1beta2.QueueSpec{
			Parent: "parentQueue",
		},
		Status: schedulingv1beta2.QueueStatus{
			State: schedulingv1beta2.QueueStateOpen,
		},
	}

	// create fake volcano clientset
	config.VolcanoClient = fakeclient.NewSimpleClientset(rootQueue, parentQueue, childQueue)
	informerFactory := informers.NewSharedInformerFactory(config.VolcanoClient, 0)
	queueInformer := informerFactory.Scheduling().V1beta1().Queues()
	config.QueueLister = queueInformer.Lister()

	stopCh := make(chan struct{})
	defer close(stopCh)
	informerFactory.Start(stopCh)
	for informerType, ok := range informerFactory.WaitForCacheSync(stopCh) {
		if !ok {
			panic(fmt.Errorf("failed to sync cache: %v", informerType))
		}
	}

	for _, testCase := range testCases {
		t.Run(testCase.Name, func(t *testing.T) {

			ret := validateJobSpec(&testCase.Job.Spec, namespace)

			if testCase.ExpectErr == true && ret == "" {
				t.Errorf("Expect error msg :%s, but got nil.", testCase.ret)
			}
			// if testCase.ExpectErr == true && testCase.reviewResponse.Allowed != false {
			// 	t.Errorf("Expect Allowed as false but got true.")
			// }
			if testCase.ExpectErr == true && !strings.Contains(ret, testCase.ret) {
				t.Errorf("Expect error msg :%s, but got diff error %v", testCase.ret, ret)
			}

			if testCase.ExpectErr == false && ret != "" {
				t.Errorf("Expect no error, but got error %v", ret)
			}
			// if testCase.ExpectErr == false && testCase.reviewResponse.Allowed != true {
			// 	t.Errorf("Expect Allowed as true but got false. %v", testCase.reviewResponse)
			// }
		})
	}
}
func TestValidateCronjobSpec(t *testing.T) {
	testCases := []struct {
		Name      string
		CronJob   v1alpha1.CronJob
		ExpectErr bool
		ret       string
	}{
		{
			Name: "validate valid-cronjob",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("1a2b3c"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "* * * * *",
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: false,
			ret:       "",
		},
		{
			Name: "validate non-standard scheduled",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "@every 1m",
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: false,
			ret:       "",
		},
		{
			Name: "validate correct timeZone",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "*/5 * * * *",
					TimeZone:          ptr.To("Pacific/Honolulu"),
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: false,
			ret:       "",
		},
		{
			Name: "validate contain TZ schedule",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "TZ=UTC 0 20 * * *",
					TimeZone:          ptr.To("Pacific/Honolulu"),
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " schedule should not contain TZ or CRON_TZ, TZ should only be set in the timeZone field",
		},
		{
			Name: "validate error schedule",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "error schedule",
					TimeZone:          ptr.To("Pacific/Honolulu"),
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       "schedule is not a valid cron expression",
		},
		{
			Name: "validate empty schedule",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "",
					TimeZone:          ptr.To("Pacific/Honolulu"),
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " schedule is required, but got empty",
		},
		{
			Name: "validate unknown . timezone",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "*/5 * * * *",
					TimeZone:          ptr.To("Asia/./Shanghai"),
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " unknown timeZone",
		},
		{
			Name: "validate unknown _ timezone",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "*/5 * * * *",
					TimeZone:          ptr.To("America/-NewYork"),
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " unknown timeZone",
		},
		{
			Name: "validate unknown space timezone",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "*/5 * * * *",
					TimeZone:          ptr.To("New York"),
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " unknown timeZone",
		},
		{
			Name: "validate unknown ! timezone",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "*/5 * * * *",
					TimeZone:          ptr.To("UTC!"),
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " unknown timeZone",
		},
		{
			Name: "validate Local timezone",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "*/5 * * * *",
					TimeZone:          ptr.To("Local"),
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " timeZone can't be Local, and must be defined in https://www.iana.org/time-zones",
		},
		{
			Name: "validate empty timezone",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "",
					TimeZone:          ptr.To(""),
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " timeZone must be nil or non-empty string, got empty string",
		},
		{
			Name: "validate error timezone",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "",
					TimeZone:          ptr.To("Amerrica/New_York"),
					ConcurrencyPolicy: v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " invalid timeZone",
		},

		{
			Name: "validate negative StartingDeadlineSeconds",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:                "*/5 * * * *",
					StartingDeadlineSeconds: ptr.To(int64(-5)),
					ConcurrencyPolicy:       v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " startingDeadlineSeconds can not be negative",
		},
		{
			Name: "validate negative successfulJobsHistoryLimit",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:                   "*/5 * * * *",
					SuccessfulJobsHistoryLimit: ptr.To(int32(-5)),
					ConcurrencyPolicy:          v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " successfulJobsHistoryLimit can not be negative",
		},
		{
			Name: "validate negative FailedJobsHistoryLimit",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:               "*/5 * * * *",
					FailedJobsHistoryLimit: ptr.To(int32(-5)),
					ConcurrencyPolicy:      v1alpha1.AllowConcurrent,
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " failedJobsHistoryLimit can not be negative",
		},
		{
			Name: "validate empty ConcurrencyPolicy",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "*/5 * * * *",
					ConcurrencyPolicy: "",
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " ConcurrencyPolicy must be nil or Allow/Forbid/Replace, got empty string",
		},
		{
			Name: "validate error ConcurrencyPolicy",
			CronJob: v1alpha1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cronvcjob",
					Namespace: metav1.NamespaceDefault,
					UID:       types.UID("123456"),
				},
				Spec: v1alpha1.CronJobSpec{
					Schedule:          "*/5 * * * *",
					ConcurrencyPolicy: "error ConcurrencyPolicy",
					JobTemplate: v1alpha1.JobTemplateSpec{
						Spec: getJobTemplate(),
					},
				},
			},
			ExpectErr: true,
			ret:       " ConcurrencyPolicy must be nil or Allow/Forbid/Replace",
		},
	}
	defaultqueue := &schedulingv1beta2.Queue{
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
		},
		Spec: schedulingv1beta2.QueueSpec{
			Weight: 1,
		},
		Status: schedulingv1beta2.QueueStatus{
			State: schedulingv1beta2.QueueStateOpen,
		},
	}

	// create fake volcano clientset
	config.VolcanoClient = fakeclient.NewSimpleClientset(defaultqueue)
	informerFactory := informers.NewSharedInformerFactory(config.VolcanoClient, 0)
	queueInformer := informerFactory.Scheduling().V1beta1().Queues()
	config.QueueLister = queueInformer.Lister()

	stopCh := make(chan struct{})
	informerFactory.Start(stopCh)
	for informerType, ok := range informerFactory.WaitForCacheSync(stopCh) {
		if !ok {
			panic(fmt.Errorf("failed to sync cache: %v", informerType))
		}
	}
	defer close(stopCh)

	for _, testCase := range testCases {
		t.Run(testCase.Name, func(t *testing.T) {
			ret := validateCronjobSpec(&testCase.CronJob.Spec, testCase.CronJob.Namespace)
			//fmt.Printf("test-case name:%s, ret:%v  testCase.reviewResponse:%v \n", testCase.Name, ret,testCase.reviewResponse)
			if testCase.ExpectErr == true && ret == "" {
				t.Errorf("Expect error msg :%s, but got nil.", testCase.ret)
			}
			if testCase.ExpectErr == true && !strings.Contains(ret, testCase.ret) {
				t.Errorf("Expect error msg :%s, but got diff error %v", testCase.ret, ret)
			}
			if testCase.ExpectErr == false && ret != "" {
				t.Errorf("Expect no error, but got error %v", ret)
			}
		})
	}
}
func getJobTemplate() v1alpha1.JobSpec {
	return v1alpha1.JobSpec{
		MinAvailable: 1,
		Queue:        "default",
		Tasks: []v1alpha1.TaskSpec{
			{
				Name:     "task-1",
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
		Policies: []v1alpha1.LifecyclePolicy{
			{
				Event:  busv1alpha1.PodEvictedEvent,
				Action: busv1alpha1.RestartTaskAction,
			},
		},
	}
}
