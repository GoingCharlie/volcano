package cronjob

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	batchv1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

func TestSyncCronJobtemp(t *testing.T) {
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
			name:                   "not first run, pre all done, is time, Allow",
			concurrency:            "Allow",
			doneJobNumSucc:         1,
			jobProcessTime:         60 * time.Second,
			now:                    justTen().Add(5 * time.Minute),
			expectRequeueAfter:     durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastSuccessTime:  &metav1.Time{Time: justTen().Add(60 * time.Second)},
			expectLastScheduleTime: &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        1,
		},
		{
			name:                   "not first run, pre all done, is time, Forbid",
			concurrency:            "Forbid",
			doneJobNumSucc:         1,
			jobProcessTime:         60 * time.Second,
			now:                    justTen().Add(5 * time.Minute),
			expectRequeueAfter:     durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastSuccessTime:  &metav1.Time{Time: justTen().Add(60 * time.Second)},
			expectLastScheduleTime: &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        1,
		},
		{
			name:                   "not first run, pre all done, is time, Replace",
			concurrency:            "Replace",
			doneJobNumSucc:         1,
			jobProcessTime:         60 * time.Second,
			now:                    justTen().Add(5 * time.Minute),
			expectRequeueAfter:     durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastSuccessTime:  &metav1.Time{Time: justTen().Add(60 * time.Second)},
			expectLastScheduleTime: &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        1,
		},
		{
			name:                   "not first run, pre all done fail, is time, Allow",
			concurrency:            "Allow",
			doneJobNumFail:         1,
			jobProcessTime:         60 * time.Second,
			now:                    justTen().Add(5 * time.Minute),
			expectRequeueAfter:     durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastScheduleTime: &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        1,
		},
		{
			name:                   "not first run, pre all done, one suc and one fail, is time, Allow",
			concurrency:            "Allow",
			doneJobNumSucc:         1,
			doneJobNumFail:         1,
			jobProcessTime:         60 * time.Second,
			now:                    justTen().Add(15 * time.Minute),
			expectRequeueAfter:     durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastSuccessTime:  &metav1.Time{Time: justTen().Add(60 * time.Second)},
			expectLastScheduleTime: &metav1.Time{Time: justTen().Add(15 * time.Minute)},
			expectInActiveNum:      1,
			expectEventsNum:        1,
		},
		{
			name:                   "not first run, pre all done but in active, is time, Allow",
			concurrency:            "Allow",
			doneJobInActiveNum:     1,
			jobProcessTime:         60 * time.Second,
			now:                    justTen().Add(5 * time.Minute),
			expectRequeueAfter:     durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastSuccessTime:  &metav1.Time{Time: justTen().Add(60 * time.Second)},
			expectLastScheduleTime: &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        2,
		},
		{
			name:                   "not first run, pre all done but in active, is time, Forbid",
			concurrency:            "Forbid",
			doneJobInActiveNum:     1,
			jobProcessTime:         60 * time.Second,
			now:                    justTen().Add(5 * time.Minute),
			expectRequeueAfter:     durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastSuccessTime:  &metav1.Time{Time: justTen().Add(60 * time.Second)},
			expectLastScheduleTime: &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        2,
		},
		{
			name:                   "not first run, pre all done but in active, is time, Replace",
			concurrency:            "Replace",
			doneJobInActiveNum:     1,
			jobProcessTime:         60 * time.Second,
			now:                    justTen().Add(5 * time.Minute),
			expectRequeueAfter:     durationPtr(5*time.Minute + 100*time.Millisecond),
			expectUpdatestatus:     true,
			expectError:            false,
			expectLastSuccessTime:  &metav1.Time{Time: justTen().Add(60 * time.Second)},
			expectLastScheduleTime: &metav1.Time{Time: fiveMinutesAfterTen()},
			expectInActiveNum:      1,
			expectEventsNum:        2,
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
