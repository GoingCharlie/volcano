package cronjob

import (
	"context"
	"sync"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	batchv1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
)

type cronjobClientInterface interface {
	getCronJobClient(vcClient vcclientset.Interface, namespace string, name string) (*batchv1.CronJob, error)
	UpdateStatus(vcClient vcclientset.Interface, cronjob *batchv1.CronJob) (*batchv1.CronJob, error)
}
type realCronjobClient struct{}

func (r *realCronjobClient) getCronJobClient(vcClient vcclientset.Interface, namespace string, name string) (*batchv1.CronJob, error) {
	return vcClient.BatchV1alpha1().CronJobs(namespace).Get(context.TODO(),
		name,
		metav1.GetOptions{})
}
func (r *realCronjobClient) UpdateStatus(vcClient vcclientset.Interface, cronjob *batchv1.CronJob) (*batchv1.CronJob, error) {
	return vcClient.BatchV1alpha1().CronJobs(cronjob.Namespace).UpdateStatus(context.TODO(),
		cronjob,
		metav1.UpdateOptions{})
}

type fakeCronjobClient struct {
	CronJob *batchv1.CronJob
	Updates []batchv1.CronJob
}

func (f *fakeCronjobClient) getCronJobClient(vcClient vcclientset.Interface, namespace string, name string) (*batchv1.CronJob, error) {
	if name == f.CronJob.Name && namespace == f.CronJob.Namespace {
		return f.CronJob, nil
	}
	return nil, errors.NewNotFound(schema.GroupResource{
		Group:    "batch.volcano.sh",
		Resource: "cronjobs",
	}, name)
}
func (f *fakeCronjobClient) UpdateStatus(vcClient vcclientset.Interface, cj *batchv1.CronJob) (*batchv1.CronJob, error) {
	f.Updates = append(f.Updates, *cj)
	return cj, nil
}

type jobClientInterface interface {
	getJobClient(vcClient vcclientset.Interface, namespace string, name string) (*batchv1.Job, error)
	deleteJobClient(vcClient vcclientset.Interface, namespace string, name string) error
	createJobClient(vcClient vcclientset.Interface, namespace string, job *batchv1.Job) (*batchv1.Job, error)
}
type realJobClient struct{}

func (r *realJobClient) getJobClient(vcClient vcclientset.Interface, namespace string, name string) (*batchv1.Job, error) {
	job, err := vcClient.BatchV1alpha1().Jobs(namespace).Get(context.TODO(),
		name,
		metav1.GetOptions{})
	return job, err
}
func (r *realJobClient) deleteJobClient(vcClient vcclientset.Interface, namespace string, name string) error {
	err := vcClient.BatchV1alpha1().Jobs(namespace).Delete(context.TODO(),
		name,
		metav1.DeleteOptions{})
	return err
}
func (r *realJobClient) createJobClient(vcClient vcclientset.Interface, namespace string, job *batchv1.Job) (*batchv1.Job, error) {
	newJob, err := vcClient.BatchV1alpha1().Jobs(namespace).Create(context.TODO(),
		job,
		metav1.CreateOptions{})
	return newJob, err
}

type fakeJobClient struct {
	sync.Mutex
	Job           *batchv1.Job
	Jobs          []batchv1.Job
	DeleteJobName []string
	Err           error
	CreateErr     error
}

func (f *fakeJobClient) createJobClient(vcClient vcclientset.Interface, namespace string, job *batchv1.Job) (*batchv1.Job, error) {
	f.Lock()
	defer f.Unlock()
	if f.CreateErr != nil {
		return nil, f.CreateErr
	}
	job.UID = "test-uid-123"
	job.TypeMeta = metav1.TypeMeta{
		Kind:       "Job",
		APIVersion: "batch.volcano.sh/v1alpha1",
	}
	f.Jobs = append(f.Jobs, *job)
	return job, nil
}

func (f *fakeJobClient) getJobClient(vcClient vcclientset.Interface, namespace string, name string) (*batchv1.Job, error) {
	f.Lock()
	defer f.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Job, nil
}

func (f *fakeJobClient) deleteJobClient(vcClient vcclientset.Interface, namespace string, name string) error {
	f.Lock()
	defer f.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.DeleteJobName = append(f.DeleteJobName, name)
	return nil
}
