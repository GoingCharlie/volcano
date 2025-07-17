- volcano/pkg/controllers/cronjob/utils.go 在删除多余limit的job时，排序依据为CreationTimestamp
- Job的完成条件判断：Job.Status.State.Phase = Completed || Failed
- 在状态为completed时，使用job.Status.State.LastTransitionTime为job完成的时间
- 勘误：ResourceVesion改为ResourceVersion
- 是否需要删除api定义中的JobReference，改为corev1.ObjectReference，与K8S保持一致（待尝试）
- 修改API
    在labels.go中增加CronJobScheduledTimestampAnnotation = "volcano.sh/cronjob-schedules-timestamp"
- volcano/pkg/features/volcano_features.go 增加VolcanoCronJobSupport
- 是否新增CronJobsScheduledAnnotation，还是直接使用VolcanoCronJobSupport
