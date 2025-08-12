package cronjob

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	batchv1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
)

// VolcanoOperator defines the interface for interacting with Volcano's Job and CronJob APIs.
type VolcanoOperator interface {
	// Job operations
	CreateJob(ctx context.Context, namespace string, job *batchv1.Job) (*batchv1.Job, error)
	DeleteJob(ctx context.Context, namespace, name string) error
	UpdateJob(ctx context.Context, namespace string, job *batchv1.Job) (*batchv1.Job, error)
	GetJob(ctx context.Context, namespace, name string) (*batchv1.Job, error)

	// CronJob operations
	GetCronJob(ctx context.Context, namespace, name string) (*batchv1.CronJob, error)
	CreateCronJob(ctx context.Context, namespace string, cronJob *batchv1.CronJob) (*batchv1.CronJob, error)
	DeleteCronJob(ctx context.Context, namespace, name string) error
	UpdateCronJob(ctx context.Context, namespace string, cronJob *batchv1.CronJob) (*batchv1.CronJob, error)
}

// RealVolcanoOperator is the concrete implementation of the VolcanoOperator interface,
// which uses the actual Volcano client to perform operations.
type RealVolcanoOperator struct {
	client vcclientset.Interface
}

func NewRealVolcanoOperator(client vcclientset.Interface) VolcanoOperator {
	return &RealVolcanoOperator{client: client}
}

// Job operations implementation
func (r *RealVolcanoOperator) CreateJob(ctx context.Context, namespace string, job *batchv1.Job) (*batchv1.Job, error) {
	return r.client.BatchV1alpha1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
}

func (r *RealVolcanoOperator) DeleteJob(ctx context.Context, namespace, name string) error {
	return r.client.BatchV1alpha1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (r *RealVolcanoOperator) UpdateJob(ctx context.Context, namespace string, job *batchv1.Job) (*batchv1.Job, error) {
	return r.client.BatchV1alpha1().Jobs(namespace).Update(ctx, job, metav1.UpdateOptions{})
}

func (r *RealVolcanoOperator) GetJob(ctx context.Context, namespace, name string) (*batchv1.Job, error) {
	return r.client.BatchV1alpha1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
}

// CronJob] operations implementation
func (r *RealVolcanoOperator) GetCronJob(ctx context.Context, namespace, name string) (*batchv1.CronJob, error) {
	return r.client.BatchV1alpha1().CronJobs(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *RealVolcanoOperator) CreateCronJob(ctx context.Context, namespace string, cronJob *batchv1.CronJob) (*batchv1.CronJob, error) {
	return r.client.BatchV1alpha1().CronJobs(namespace).Create(ctx, cronJob, metav1.CreateOptions{})
}

func (r *RealVolcanoOperator) DeleteCronJob(ctx context.Context, namespace, name string) error {
	return r.client.BatchV1alpha1().CronJobs(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (r *RealVolcanoOperator) UpdateCronJob(ctx context.Context, namespace string, cronJob *batchv1.CronJob) (*batchv1.CronJob, error) {
	return r.client.BatchV1alpha1().CronJobs(namespace).Update(ctx, cronJob, metav1.UpdateOptions{})
}

// MockVolcanoOperator is a mock implementation of the VolcanoOperator interface
type MockVolcanoOperator struct {
	// Job operations mock functions
	CreateJobFunc func(ctx context.Context, namespace string, job *batchv1.Job) (*batchv1.Job, error)
	DeleteJobFunc func(ctx context.Context, namespace, name string) error
	UpdateJobFunc func(ctx context.Context, namespace string, job *batchv1.Job) (*batchv1.Job, error)
	GetJobFunc    func(ctx context.Context, namespace, name string) (*batchv1.Job, error)

	// CronJob operations mock functions
	GetCronJobFunc    func(ctx context.Context, namespace, name string) (*batchv1.CronJob, error)
	CreateCronJobFunc func(ctx context.Context, namespace string, cronJob *batchv1.CronJob) (*batchv1.CronJob, error)
	DeleteCronJobFunc func(ctx context.Context, namespace, name string) error
	UpdateCronJobFunc func(ctx context.Context, namespace string, cronJob *batchv1.CronJob) (*batchv1.CronJob, error)

	// Call counters
	JobCounters     map[string]int
	CronJobCounters map[string]int
}

func NewMockVolcanoOperator() *MockVolcanoOperator {
	return &MockVolcanoOperator{
		JobCounters:     make(map[string]int),
		CronJobCounters: make(map[string]int),
	}
}

// Job operations mock implementation
func (m *MockVolcanoOperator) CreateJob(ctx context.Context, namespace string, job *batchv1.Job) (*batchv1.Job, error) {
	m.JobCounters["Create"]++
	if m.CreateJobFunc != nil {
		return m.CreateJobFunc(ctx, namespace, job)
	}
	return job, nil
}

func (m *MockVolcanoOperator) DeleteJob(ctx context.Context, namespace, name string) error {
	m.JobCounters["Delete"]++
	if m.DeleteJobFunc != nil {
		return m.DeleteJobFunc(ctx, namespace, name)
	}
	return nil
}

func (m *MockVolcanoOperator) UpdateJob(ctx context.Context, namespace string, job *batchv1.Job) (*batchv1.Job, error) {
	m.JobCounters["Update"]++
	if m.UpdateJobFunc != nil {
		return m.UpdateJobFunc(ctx, namespace, job)
	}
	return job, nil
}

func (m *MockVolcanoOperator) GetJob(ctx context.Context, namespace, name string) (*batchv1.Job, error) {
	m.JobCounters["Get"]++
	if m.GetJobFunc != nil {
		return m.GetJobFunc(ctx, namespace, name)
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}, nil
}

// CronJob operations mock implementation
func (m *MockVolcanoOperator) GetCronJob(ctx context.Context, namespace, name string) (*batchv1.CronJob, error) {
	m.CronJobCounters["Get"]++
	if m.GetCronJobFunc != nil {
		return m.GetCronJobFunc(ctx, namespace, name)
	}
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}, nil
}

func (m *MockVolcanoOperator) CreateCronJob(ctx context.Context, namespace string, cronJob *batchv1.CronJob) (*batchv1.CronJob, error) {
	m.CronJobCounters["Create"]++
	if m.CreateCronJobFunc != nil {
		return m.CreateCronJobFunc(ctx, namespace, cronJob)
	}
	return cronJob, nil
}

func (m *MockVolcanoOperator) DeleteCronJob(ctx context.Context, namespace, name string) error {
	m.CronJobCounters["Delete"]++
	if m.DeleteCronJobFunc != nil {
		return m.DeleteCronJobFunc(ctx, namespace, name)
	}
	return nil
}

func (m *MockVolcanoOperator) UpdateCronJob(ctx context.Context, namespace string, cronJob *batchv1.CronJob) (*batchv1.CronJob, error) {
	m.CronJobCounters["Update"]++
	if m.UpdateCronJobFunc != nil {
		return m.UpdateCronJobFunc(ctx, namespace, cronJob)
	}
	return cronJob, nil
}
