package cronjob

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	batchv1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

// func newFakeController() *cronjobcontroller {
// 	scheme := runtime.NewScheme()
// 	_ = batchv1.AddToScheme(scheme)
// 	volcanoClientSet := volcanoclient.NewSimpleClientset()
// 	kubeClientSet := kubeclient.NewSimpleClientset()
// 	vcSharedInformers := informerfactory.NewSharedInformerFactory(volcanoClientSet, 0)

// 	controller := &cronjobcontroller{}
// 	opt := &framework.ControllerOption{
// 		VolcanoClient:           volcanoClientSet,
// 		KubeClient:              kubeClientSet,
// 		VCSharedInformerFactory: vcSharedInformers,
// 		WorkerNum:               3,
// 	}

// 	controller.Initialize(opt)

//		return controller
//	}
func TestGetJobsByCronJob(t *testing.T) {
	cronJobUID := types.UID("test-uid-123")
	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cronjob",
			Namespace: "default",
			UID:       cronJobUID,
		},
	}

	testCases := []struct {
		name        string
		jobsToAdd   []*batchv1.Job
		expectedLen int
	}{
		{
			name: "matching controller",
			jobsToAdd: []*batchv1.Job{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "job1",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "batch/v1",
								Kind:       "CronJob",
								Name:       "test-cronjob",
								UID:        cronJobUID,
								Controller: ptr.To(true),
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "job2",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "batch/v1",
								Kind:       "CronJob",
								Name:       "test-cronjob",
								UID:        cronJobUID,
								Controller: ptr.To(true),
							},
						},
					},
				},
			},
			expectedLen: 2,
		},
		{
			name: "non-matching controller kind",
			jobsToAdd: []*batchv1.Job{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "job3",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "batch/v1",
								Kind:       "Deployment",
								Name:       "test-cronjob",
								UID:        cronJobUID,
								Controller: ptr.To(true),
							},
						},
					},
				},
			},
			expectedLen: 0,
		},
		{
			name: "different namespace job",
			jobsToAdd: []*batchv1.Job{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "job4",
						Namespace: "other-namespace",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "batch/v1",
								Kind:       "CronJob",
								Name:       "test-cronjob",
								UID:        cronJobUID,
								Controller: ptr.To(true),
							},
						},
					},
				},
			},
			expectedLen: 0,
		},
		{
			name: "job without controller reference",
			jobsToAdd: []*batchv1.Job{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "job5",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "batch/v1",
								Kind:       "CronJob",
								Name:       "test-cronjob",
								UID:        cronJobUID,
							},
						},
					},
				},
			},
			expectedLen: 0,
		},
		{
			name: "jobs with mismatched UIDs",
			jobsToAdd: []*batchv1.Job{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "job6",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "batch/v1",
								Kind:       "CronJob",
								Name:       "test-cronjob",
								UID:        "different-uid",
								Controller: ptr.To(true),
							},
						},
					},
				},
			},
			expectedLen: 0,
		},
		{
			name:        "empty job list",
			jobsToAdd:   []*batchv1.Job{},
			expectedLen: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			controller := newFakeController()
			for _, job := range tc.jobsToAdd {
				jobCopy := job.DeepCopy()
				if err := controller.jobInformer.Informer().GetIndexer().Add(jobCopy); err != nil {
					t.Fatalf("failed to add job to informer: %v", err)
				}
			}
			jobs, err := controller.getJobsByCronJob(cronJob)
			if err != nil {
				t.Fatalf("getJobsByCronJob returned error: %v", err)
			}
			if len(jobs) != tc.expectedLen {
				t.Errorf("expected %d jobs, got %d", tc.expectedLen, len(jobs))
				for i, job := range jobs {
					t.Logf("job[%d]: %s/%s, OwnerRef: %v", i, job.Namespace, job.Name, job.OwnerReferences)
				}
			}
		})
	}
}
