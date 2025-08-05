# 单元测试计划
## handler 测试
### 待测函数
- addJob  
- updateJob
- deleteJob
- addCronjobcontrollerQueue
- updateCronJob
- getJobsByCronJob
- processFinishedJobs*
- processCtljobAndActiveJob*
- isJobFinished
- removeOldestJobs
- deleteJobByApi
- validateTZandSchedule*
- processConcurrencyPolicy*
- createJob*
### 测试用例
1. addJob
- 正常待调谐的job
- job为空
- 非job
- 正在删除的job
- 非cronjob控制的job
- Job的OwnerReference指向不存在的CronJob
2. processFinishedJobs
## controller测试
1. SyncCronJob  

第一次运行：schedule、TZ、没到时间、时区没到时间、挂起、删除 | 到时间，时区到时间，TZ设置到schedule、超过or未超过deadline  
非第一次运行且之前完成了：没到时间、到时间，创建job失败、job已存在、挂起、删除、过dead  
非第一次运行且之前没有完成：没到时间、同上需要不同的并发策略 | 

每次运行前cronjob的状态  
active：有，无  
joblister：全完成、部分完成、全部未完成  
两者联系：active与joblister一一对应、在active但不在lister（）、在active但lister已完成、在lister但不在active **--舍弃**  

synccronjob的功能
同步状态：active、lastfinishedtime  
active：将已完成的清除、不是acive但在active的清除  **--舍弃**，变成syncjob调用的其他函数的单元测试了  

！！！ 四个场景进行排列组合
首次运行场景：从未运行过的 CronJob 在不同时间点和配置下的行为  
包括：没到时间、到时间、超过deadline、多次miss
之前运行过：未完成、已完成、部分完成
调度时间场景：在调度时间到达时的行为：创建成功、创建失败、已经存在、已经处理、未到达调度时间、错过调度时间（有无deadline）
并发策略场景：不同并发策略（Allow/Forbid/Replace）下的行为，有运行和没有运行
异常情况：无效调度表达式、时区错误、暂停状态

# 疑惑
如果控制器重启，q的job不存在如何处理