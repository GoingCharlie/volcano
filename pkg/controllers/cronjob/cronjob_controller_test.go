package cronjob

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubeclient "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	batchv1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	volcanoclient "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	volcanoscheme "volcano.sh/apis/pkg/client/clientset/versioned/scheme"
	informerfactory "volcano.sh/apis/pkg/client/informers/externalversions"
	"volcano.sh/volcano/pkg/controllers/framework"
)

var (
	sixtySecondsDead int64 = 60
	threeMinutesDead int64 = 180
	longDead         int64 = 60060
)

func init() {
	_ = volcanoscheme.AddToScheme(scheme.Scheme)
}

func newFakeController() *cronjobcontroller {

	baseClient := volcanoclient.NewSimpleClientset()

	wrappedClient := &testFakeClient{
		Clientset: baseClient,
	}
	kubeClientSet := kubeclient.NewSimpleClientset()
	vcSharedInformers := informerfactory.NewSharedInformerFactory(wrappedClient, 0)

	controller := &cronjobcontroller{}
	opt := &framework.ControllerOption{
		VolcanoClient:           wrappedClient,
		KubeClient:              kubeClientSet,
		VCSharedInformerFactory: vcSharedInformers,
		WorkerNum:               3,
	}
	controller.Initialize(opt)
	return controller
}

func newCronJob() batchv1.CronJob {
	return batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "vccronjob",
			Namespace:         "volcano",
			UID:               types.UID("test-uid-123"),
			CreationTimestamp: metav1.Time{Time: justTen()},
		},
		Spec: batchv1.CronJobSpec{
			Schedule:          "*/5 * * * *",
			ConcurrencyPolicy: "Allow",
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"volcano": "yes"},
					Annotations: map[string]string{"cronjob": "yes"},
				},
			},
		},
	}
}
func justTen() time.Time {
	T, err := time.Parse(time.RFC3339, "2025-03-11T10:00:00Z")
	if err != nil {
		panic("test setup error")
	}
	return T
}
func tomorrowTen() time.Time {
	T, err := time.Parse(time.RFC3339, "2025-03-12T10:00:00Z")
	if err != nil {
		panic("test setup error")
	}
	return T
}
func fiveMinutesAfterTen() time.Time {
	return justTen().Add(5 * time.Minute)
}

func threeMinutesAfterTen() time.Time {
	return justTen().Add(3 * time.Minute)
}

func sevenMinutesAfterTen() time.Time {
	return justTen().Add(7 * time.Minute)
}

func tenHoursAfterTen() time.Time {
	return justTen().Add(10 * time.Hour)
}
func comparePtr(a, b *time.Duration) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func durationPtr(d time.Duration) *time.Duration {
	return &d
}
func metaTimeEqual(a, b *metav1.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Time.Equal(b.Time)
}
func getEventsFromRecorder(recorder *record.FakeRecorder) []string {
	close(recorder.Events)
	var events []string
	for event := range recorder.Events {
		events = append(events, event)
	}
	return events
}
func TestSyncCronJob(t *testing.T) {
	testCases := []struct {
		name                    string
		schedule                string
		timeZone                string
		suspend                 bool
		now                     time.Time
		concurrency             batchv1.ConcurrencyPolicy
		lastScheduleTime        *metav1.Time
		startingDeadlineSeconds *int64
		doneJobNumSucc          int
		doneJobNumFail          int
		activeJobNum            int
		doneJobInActiveNum      int
		jobProcessTime          time.Duration

		expectLastScheduleTime *metav1.Time
		expectRequeueAfter     *time.Duration
		expectUpdatestatus     bool
		expectEventsNum        int
		expectError            bool
		expectInActiveNum      int
		expectLastSuccessTime  *metav1.Time
	}{
		{
			name:               "Abnormal situation, error schedule",
			schedule:           "wrong shcedlue",
			expectRequeueAfter: nil,
			expectUpdatestatus: false,
			expectError:        true,
		},
		{
			name:               "Abnormal situation, error TimeZone",
			timeZone:           "error TZ",
			expectRequeueAfter: nil,
			expectUpdatestatus: false,
			expectError:        true,
			expectEventsNum:    1,
		},
		{
			name:               "Abnormal situation, suspend",
			suspend:            true,
			expectRequeueAfter: nil,
			expectUpdatestatus: false,
			expectError:        false,
		},
		{
			name:                   "First run, is not time, Allow",
			concurrency:            "Allow",
			now:                    threeMinutesAfterTen(),
			expectRequeueAfter:     durationPtr(2*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     false,
			expectError:            false,
			expectLastScheduleTime: nil,
		},
		{
			name:                   "First run, is not time, Forbid",
			concurrency:            "Forbid",
			now:                    threeMinutesAfterTen(),
			expectRequeueAfter:     durationPtr(2*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     false,
			expectError:            false,
			expectLastScheduleTime: nil,
		},
		{
			name:                   "First run, is not time, Replace",
			concurrency:            "Replace",
			now:                    threeMinutesAfterTen(),
			expectRequeueAfter:     durationPtr(2*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     false,
			expectError:            false,
			expectLastScheduleTime: nil,
		},
		{
			name:                   "First run, is not time in TZ",
			concurrency:            "Allow",
			now:                    justTen(),
			schedule:               "0 10 * * *",
			timeZone:               "Pacific/Honolulu",
			expectRequeueAfter:     durationPtr(10*time.Hour + 100*time.Millisecond),
			expectUpdatestatus:     false,
			expectError:            false,
			expectLastScheduleTime: nil,
		},
		{
			name:                   "First run, is time, Allow",
			concurrency:            "Allow",
			now:                    fiveMinutesAfterTen(),
			expectRequeueAfter:     durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastScheduleTime: &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        1,
		},
		{
			name:                   "First run, is time, Forbid",
			concurrency:            "Forbid",
			now:                    fiveMinutesAfterTen(),
			expectRequeueAfter:     durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastScheduleTime: &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        1,
		},
		{
			name:                   "First run, is time, Replace",
			concurrency:            "Forbid",
			now:                    fiveMinutesAfterTen(),
			expectRequeueAfter:     durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastScheduleTime: &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        1,
		},
		{
			name:                    "First run, is time, has startingDeadlineSeconds, Allow",
			concurrency:             "Allow",
			now:                     fiveMinutesAfterTen(),
			startingDeadlineSeconds: &sixtySecondsDead,
			expectRequeueAfter:      durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:      true,
			expectError:             false,
			expectLastScheduleTime:  &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:       1,
			expectEventsNum:         1,
		},
		{
			name:                   "First run, is time in TZ, Allow",
			concurrency:            "Allow",
			now:                    tenHoursAfterTen(),
			schedule:               "0 10 * * *",
			timeZone:               "Pacific/Honolulu",
			expectRequeueAfter:     durationPtr(24*time.Hour + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastScheduleTime: &metav1.Time{Time: tenHoursAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        1,
		},
		{
			name:                   "First run, is time, uses schedule's TZ despite conflict with TimeZone setting",
			schedule:               "TZ=UTC 0 20 * * *",
			timeZone:               "Pacific/Honolulu",
			now:                    tenHoursAfterTen(),
			expectRequeueAfter:     durationPtr(24*time.Hour + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastScheduleTime: &metav1.Time{Time: tenHoursAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        2,
		},
		{
			name:                   "First run, miss Time, no startingDeadlineSeconds, Allow",
			concurrency:            "Allow",
			now:                    sevenMinutesAfterTen(),
			expectRequeueAfter:     durationPtr(3*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastScheduleTime: &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        1,
		},
		{
			name:                    "First run, miss Time, no miss deadline, Allow",
			concurrency:             "Allow",
			now:                     sevenMinutesAfterTen(),
			startingDeadlineSeconds: &threeMinutesDead,
			expectRequeueAfter:      durationPtr(3*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:      true,
			expectError:             false,
			expectLastScheduleTime:  &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:       1,
			expectEventsNum:         1,
		},
		{
			name:                    "First run, miss deadline, Allow",
			concurrency:             "Allow",
			now:                     sevenMinutesAfterTen(),
			startingDeadlineSeconds: &sixtySecondsDead,
			expectRequeueAfter:      durationPtr(3*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:      false,
			expectError:             false,
			expectLastScheduleTime:  nil,
		},
		{
			name:                   "First run, manymiss, no startingDeadlineSeconds, Allow",
			concurrency:            "Allow",
			now:                    tenHoursAfterTen(),
			expectRequeueAfter:     durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastScheduleTime: &metav1.Time{Time: tenHoursAfterTen()},
			expectEventsNum:        2,
			expectInActiveNum:      1,
		},
		{
			name:                    "First run, manymiss, has startingDeadlineSeconds, Allow",
			concurrency:             "Allow",
			now:                     tomorrowTen(),
			schedule:                "* * * * *",
			startingDeadlineSeconds: &longDead,
			expectRequeueAfter:      durationPtr(1*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:      true,
			expectError:             false,
			expectLastScheduleTime:  &metav1.Time{Time: tomorrowTen()},
			expectEventsNum:         2,
			expectInActiveNum:       1,
		},
		{
			name:                  "not first run, pre all done, is not time, Allow",
			concurrency:           "Allow",
			doneJobNumSucc:        1,
			jobProcessTime:        60 * time.Second,
			now:                   threeMinutesAfterTen(),
			expectRequeueAfter:    durationPtr(2*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:    true,
			expectError:           false,
			expectLastSuccessTime: &metav1.Time{Time: justTen().Add(60 * time.Second)},
		},
		{
			name:                  "not first run, pre all done, is not time, Forbid",
			concurrency:           "Forbid",
			doneJobNumSucc:        1,
			jobProcessTime:        60 * time.Second,
			now:                   threeMinutesAfterTen(),
			expectRequeueAfter:    durationPtr(2*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:    true,
			expectError:           false,
			expectLastSuccessTime: &metav1.Time{Time: justTen().Add(60 * time.Second)},
		},
		{
			name:                  "not first run, pre all done, is not time, Replace",
			concurrency:           "Replace",
			doneJobNumSucc:        1,
			jobProcessTime:        60 * time.Second,
			now:                   threeMinutesAfterTen(),
			expectRequeueAfter:    durationPtr(2*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:    true,
			expectError:           false,
			expectLastSuccessTime: &metav1.Time{Time: justTen().Add(60 * time.Second)},
		},
		{
			name:                  "not first run, pre all done fail, is not time, Allow",
			concurrency:           "Allow",
			doneJobNumFail:        1,
			jobProcessTime:        60 * time.Second,
			now:                   threeMinutesAfterTen(),
			expectRequeueAfter:    durationPtr(2*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:    false,
			expectError:           false,
			expectLastSuccessTime: nil,
		},
		{
			name:                   "not first run, pre all done, one suc and one fail, is not time, Allow",
			concurrency:            "Allow",
			doneJobNumSucc:         1,
			doneJobNumFail:         1,
			jobProcessTime:         60 * time.Second,
			now:                    justTen().Add(17 * time.Minute),
			lastScheduleTime:       &metav1.Time{Time: justTen().Add(15 * time.Minute)},
			expectLastScheduleTime: &metav1.Time{Time: justTen().Add(15 * time.Minute)},
			expectRequeueAfter:     durationPtr(3*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastSuccessTime:  &metav1.Time{Time: justTen().Add(60 * time.Second)},
		},
		{
			name:                  "not first run, pre all done but in active, is not time, Allow",
			concurrency:           "Allow",
			doneJobInActiveNum:    1,
			jobProcessTime:        60 * time.Second,
			now:                   threeMinutesAfterTen(),
			expectRequeueAfter:    durationPtr(2*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:    true,
			expectError:           false,
			expectInActiveNum:     0,
			expectLastSuccessTime: &metav1.Time{Time: justTen().Add(60 * time.Second)},
			expectEventsNum:       1,
		},
		{
			name:                  "not first run, pre all done but in active, is not time, Forbid",
			concurrency:           "Forbid",
			doneJobInActiveNum:    1,
			jobProcessTime:        60 * time.Second,
			now:                   threeMinutesAfterTen(),
			expectRequeueAfter:    durationPtr(2*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:    true,
			expectError:           false,
			expectInActiveNum:     0,
			expectLastSuccessTime: &metav1.Time{Time: justTen().Add(60 * time.Second)},
			expectEventsNum:       1,
		},
		{
			name:                  "not first run, pre all done but in active, is not time, Replace",
			concurrency:           "Replace",
			doneJobInActiveNum:    1,
			jobProcessTime:        60 * time.Second,
			now:                   threeMinutesAfterTen(),
			expectRequeueAfter:    durationPtr(2*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:    true,
			expectError:           false,
			expectInActiveNum:     0,
			expectLastSuccessTime: &metav1.Time{Time: justTen().Add(60 * time.Second)},
			expectEventsNum:       1,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cronJob := newCronJob()
			controller := newFakeController()
			recorder := record.NewFakeRecorder(10)
			controller.recorder = recorder
			if tc.schedule != "" {
				cronJob.Spec.Schedule = tc.schedule
			}
			if tc.timeZone != "" {
				cronJob.Spec.TimeZone = &tc.timeZone
			}
			cronJob.Spec.Suspend = &tc.suspend
			if tc.concurrency != "" {
				cronJob.Spec.ConcurrencyPolicy = tc.concurrency
			}
			if !tc.now.IsZero() {
				controller.now = func() time.Time { return tc.now }
			}
			if tc.startingDeadlineSeconds != nil {
				cronJob.Spec.StartingDeadlineSeconds = tc.startingDeadlineSeconds
			}
			if tc.lastScheduleTime != nil {
				cronJob.Status.LastScheduleTime = tc.lastScheduleTime
			}
			jobsByCronJob := make([]*batchv1.Job, 0, tc.doneJobNumSucc+tc.doneJobNumFail+tc.activeJobNum+tc.doneJobInActiveNum)
			createTime := cronJob.ObjectMeta.CreationTimestamp.Time

			generateJobs := func(count int, status batchv1.JobPhase, updateActive bool) {
				for i := 0; i < count; i++ {
					job, err := getJobFromTemplate(&cronJob, createTime)
					if err != nil {
						t.Fatalf("unexpected error getting job from template: %v", err)
					}

					job.UID = "test-uid-123"
					job.Namespace = cronJob.Namespace
					job.Status.State.Phase = status
					job.Status.State.LastTransitionTime = metav1.Time{Time: createTime.Add(tc.jobProcessTime)}
					job.TypeMeta = metav1.TypeMeta{
						Kind:       "Job",
						APIVersion: "batch.volcano.sh/v1alpha1",
					}
					jobsByCronJob = append(jobsByCronJob, job)

					if updateActive {
						ref, err := getRef(job)
						if err != nil {
							t.Fatalf("unexpected error getting job reference: %v", err)
						}
						cronJob.Status.Active = append(cronJob.Status.Active, convertToVolcanoJobRef(ref))
					}

					createTime = createTime.Add(5 * time.Minute)
				}
			}
			generateJobs(tc.doneJobNumSucc, batchv1.Completed, false)
			generateJobs(tc.doneJobNumFail, batchv1.Failed, false)
			generateJobs(tc.activeJobNum, "", true)
			generateJobs(tc.doneJobInActiveNum, batchv1.Completed, true)

			requeueAfter, updateStatus, syncErr := controller.syncCronJob(&cronJob, jobsByCronJob)
			if tc.expectError {
				if syncErr == nil {
					t.Errorf("Expected error but got nil")
				}
			} else {
				if syncErr != nil {
					t.Errorf("Expected no error but got: %v", syncErr)
				}
			}
			if !comparePtr(tc.expectRequeueAfter, requeueAfter) {
				t.Errorf("Expected %v, got %v", tc.expectRequeueAfter, requeueAfter)
			}
			if updateStatus != tc.expectUpdatestatus {
				t.Errorf("Expected updateStatus to be %v, got %v", tc.expectUpdatestatus, updateStatus)
			}
			if !metaTimeEqual(tc.expectLastScheduleTime, cronJob.Status.LastScheduleTime) {
				actual := "nil"
				if cronJob.Status.LastScheduleTime != nil {
					actual = cronJob.Status.LastScheduleTime.String()
				}
				expected := "nil"
				if tc.expectLastScheduleTime != nil {
					expected = tc.expectLastScheduleTime.String()
				}
				t.Errorf("Expected LastScheduleTime to be %s, got %s", expected, actual)
			}
			if !metaTimeEqual(tc.expectLastSuccessTime, cronJob.Status.LastSuccessfulTime) {
				actual := "nil"
				if cronJob.Status.LastSuccessfulTime != nil {
					actual = cronJob.Status.LastSuccessfulTime.String()
				}
				expected := "nil"
				if tc.expectLastSuccessTime != nil {
					expected = tc.expectLastSuccessTime.String()
				}
				t.Errorf("Expected LastSuccessfulTime to be %s, got %s", expected, actual)
			}
			if len(cronJob.Status.Active) != tc.expectInActiveNum {
				t.Errorf("Expected InActiveNum to be %d, got %d", tc.expectInActiveNum, len(cronJob.Status.Active))
			}

			actualEvents := getEventsFromRecorder(recorder)
			if len(actualEvents) != tc.expectEventsNum {
				t.Errorf("Expected %d events, got %d. Events: %v",
					tc.expectEventsNum, len(actualEvents), actualEvents)
			}
		})
	}
}
