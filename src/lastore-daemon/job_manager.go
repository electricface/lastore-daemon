package main

import (
	"fmt"
	log "github.com/cihub/seelog"
	"internal/system"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DownloadQueue        = "download"
	DownloadQueueCap     = 3
	SystemChangeQueue    = "system change"
	SystemChangeQueueCap = 1

	// LockQueue is special. All other queue must wait for LockQueue be emptied.
	LockQueue = "lock"
)

// JobManager
// 1. maintain DownloadQueue and SystemchangeQueue
// 2. Create, Delete and Pause Jobs and schedule they.
type JobManager struct {
	queues map[string]*JobQueue

	system system.System

	dispatchLock sync.Mutex

	notify  func()
	changed bool
}

func NewJobManager(api system.System, notifyFn func()) *JobManager {
	if api == nil || notifyFn == nil {
		panic("NewJobManager with api=nil, notifyFn=nil")
	}
	m := &JobManager{
		queues: make(map[string]*JobQueue),
		notify: notifyFn,
		system: api,
	}
	m.createJobList(DownloadQueue, DownloadQueueCap)
	m.createJobList(SystemChangeQueue, SystemChangeQueueCap)
	m.createJobList(LockQueue, 1)

	api.AttachIndicator(m.handleJobProgressInfo)
	return m
}

func (m *JobManager) List() JobList {
	var r JobList
	for _, queue := range m.queues {
		for _, job := range queue.Jobs {
			r = append(r, job)
		}
	}
	sort.Sort(r)
	return r
}

func (m *JobManager) guest(jobType string, packages []string) string {
	pList := strings.Join(packages, "")
	for _, job := range m.List() {
		if job.Type == jobType && strings.Join(job.Packages, "") == pList {
			return job.Id
		}
		if job.next == nil {
			continue
		}
		if job.next.Type == jobType && strings.Join(job.next.Packages, "") == pList {
			// Don't return the job.next.
			// It's not a workable Job before the Job finished.
			return job.Id
		}
	}
	return ""
}

// CreateJob create the job and try starting it
func (m *JobManager) CreateJob(jobName string, jobType string, packages []string) (*Job, error) {
	if job := m.find(m.guest(jobType, packages)); job != nil {
		return job, m.MarkStart(job.Id)
	}

	var job *Job
	switch jobType {
	case system.DownloadJobType:
		job = NewJob(jobName, packages, system.DownloadJobType, DownloadQueue)
	case system.InstallJobType:
		job = NewJob(jobName, packages, system.DownloadJobType, DownloadQueue)
		job.next = NewJob(jobName, packages, system.InstallJobType, SystemChangeQueue)
		job.Id = job.next.Id
	case system.RemoveJobType:
		job = NewJob(jobName, packages, system.RemoveJobType, SystemChangeQueue)
	case system.UpdateSourceJobType:
		job = NewJob(jobName, nil, system.UpdateSourceJobType, LockQueue)
	case system.DistUpgradeJobType:
		job = NewJob(jobName, packages, system.DistUpgradeJobType, LockQueue)
	case system.UpdateJobType:
		job = NewJob(jobName, packages, system.UpdateJobType, SystemChangeQueue)
	default:
		return nil, system.NotSupportError
	}
	m.addJob(job)
	return job, m.MarkStart(job.Id)
}

// MarkStart transition the Job status to ReadyStatus
// and move the it to the head of queue.
func (m *JobManager) MarkStart(jobId string) error {
	job := m.find(jobId)
	if job == nil {
		return system.NotFoundError
	}

	if job.Status != system.ReadyStatus {
		err := TransitionJobState(job, system.ReadyStatus)
		if err != nil {
			return err
		}
	}

	queue, ok := m.queues[job.queueName]
	if !ok {
		return system.NotFoundError
	}
	return queue.Raise(jobId)
}

// CleanJob transition the Job status to EndStatus,
// so the job will be auto clean in next dispatch run.
func (m *JobManager) CleanJob(jobId string) error {
	job := m.find(jobId)
	if job == nil {
		return system.NotFoundError
	}

	if job.Status == system.RunningStatus && job.Cancelable {
		m.PauseJob(jobId)
	}

	if ValidTransitionJobState(job.Status, system.EndStatus) {
		job.next = nil
	}
	return TransitionJobState(job, system.EndStatus)
}

// PauseJob try aborting the job and transition the status to PauseStatus
func (m *JobManager) PauseJob(jobId string) error {
	job := m.find(jobId)
	if job == nil {
		return system.NotFoundError
	}

	if !ValidTransitionJobState(job.Status, system.PausedStatus) {
		return system.NotSupportError
	}

	err := m.system.Abort(job.Id)
	if err != nil {
		return err
	}

	return TransitionJobState(job, system.PausedStatus)
}

func (m *JobManager) find(jobId string) *Job {
	for _, queue := range m.queues {
		job := queue.Find(jobId)
		if job != nil {
			return job
		}
	}
	return nil
}

// Dispatch transition Job status in Job Queues
// 1. Clean Jobs whose status is system.EndStatus
// 2. Run all Pending Jobs.
func (m *JobManager) dispatch() {
	m.dispatchLock.Lock()
	defer m.dispatchLock.Unlock()

	var pendingDeleteJobs []*Job
	for _, queue := range m.queues {
		// 1. Clean Jobs with EndStatus
		for _, job := range queue.Jobs {
			switch {
			case job.Status == system.EndStatus:
				pendingDeleteJobs = append(pendingDeleteJobs, job)
			}
		}
	}
	for _, job := range pendingDeleteJobs {
		m.removeJob(job.Id, job.queueName)
		if job.next != nil {
			job = job.next
			m.addJob(job)
			m.MarkStart(job.Id)
		}
	}

	for name, queue := range m.queues {
		// wait for LockQueue be idled
		if name != LockQueue && len(m.queues[LockQueue].RunningJobs()) != 0 {
			continue
		}

		// 2. Try starting jobs with ReadyStatus
		jobs := queue.PendingJobs()
		for _, job := range jobs {
			if job.Status == system.FailedStatus {
				m.MarkStart(job.Id)
				log.Infof("Retry failed Job %v\n", job)
			}
			err := StartSystemJob(m.system, job)
			if err != nil {
				log.Errorf("StartSystemJob failed %v :%v\n", job, err)
			}
		}
	}

	if m.changed && m.notify != nil {
		m.changed = false
		m.notify()
	}
}

func (m *JobManager) Dispatch() {
	for {
		<-time.After(time.Millisecond * 500)
		m.dispatch()
	}
}

func (m *JobManager) createJobList(name string, cap int) {
	list := NewJobQueue(name, cap)
	m.queues[name] = list
}

func (m *JobManager) addJob(j *Job) error {
	if j == nil {
		log.Trace("adJob with nil")
		return system.NotFoundError
	}
	queueName := j.queueName
	queue, ok := m.queues[queueName]
	if !ok {
		return system.NotFoundError
	}

	err := queue.Add(j)
	if err != nil {
		return err
	}
	m.changed = true
	return nil
}
func (m *JobManager) removeJob(jobId string, queueName string) error {
	queue, ok := m.queues[queueName]
	if !ok {
		return system.NotFoundError
	}

	err := queue.Remove(jobId)
	if err != nil {
		return err
	}
	m.changed = true
	return nil
}

type JobList []*Job

func (l JobList) Len() int {
	return len(l)
}
func (l JobList) Less(i, j int) bool {
	if l[i].Type == system.UpdateSourceJobType {
		return true
	}
	return l[i].CreateTime < l[j].CreateTime
}
func (l JobList) Swap(i, j int) {
	l[i], l[j] = l[j], l[i]
}

type JobQueue struct {
	Name string
	Jobs JobList
	Cap  int
}

func NewJobQueue(name string, cap int) *JobQueue {
	return &JobQueue{
		Name: name,
		Cap:  cap,
	}
}

// PendingJob get the workable ready Jobs and recoverable failed Jobs
func (l *JobQueue) PendingJobs() JobList {
	var numRunning int
	var readyJobs []*Job
	for _, job := range l.Jobs {
		switch job.Status {
		case system.FailedStatus:
			if job.retry > 0 {
				job.retry--
				readyJobs = append(readyJobs, job)
			}
		case system.RunningStatus:
			numRunning = numRunning + 1
		case system.ReadyStatus:
			readyJobs = append(readyJobs, job)
		}
	}
	space := l.Cap - numRunning
	numPending := len(readyJobs)

	var n int
	for space > 0 && numPending > 0 {
		space--
		numPending--
		n++
	}
	if n+1 < numPending {
		log.Info("These jobs are waiting for running...", readyJobs[n+1:])
	}
	r := JobList(readyJobs[:n])
	sort.Sort(r)
	return r
}

func (l *JobQueue) RunningJobs() JobList {
	var r JobList
	for _, job := range l.Jobs {
		switch job.Status {
		case system.RunningStatus:
			r = append(r, job)
		}
	}
	return r
}

func (l *JobQueue) Add(j *Job) error {
	for _, job := range l.Jobs {
		if job.Type == j.Type && strings.Join(job.Packages, "") == strings.Join(j.Packages, "") {
			return fmt.Errorf("exists job %q:%q", job.Type, job.Packages)
		}
	}
	l.Jobs = append(l.Jobs, j)
	sort.Sort(l.Jobs)
	return nil
}

func (l *JobQueue) Remove(id string) error {
	index := -1
	for i, job := range l.Jobs {
		if job.Id == id {
			index = i
			break
		}
	}
	if index == -1 {
		return system.NotFoundError
	}

	job := l.Jobs[index]
	DestroyJob(job)

	l.Jobs = append(l.Jobs[0:index], l.Jobs[index+1:]...)
	sort.Sort(l.Jobs)
	return nil
}

// Raise raise the specify Job to head of JobList
// return system.NotFoundError if can't find the specify Job
func (l *JobQueue) Raise(jobId string) error {
	var p int = -1
	for i, job := range l.Jobs {
		if job.Id == jobId {
			p = i
			break
		}
	}
	if p == -1 {
		return system.NotFoundError
	}
	l.Jobs.Swap(0, p)
	return nil
}

func (l *JobQueue) Find(id string) *Job {
	for _, job := range l.Jobs {
		if job.Id == id {
			return job
		}
	}
	return nil
}

func (m *JobManager) handleJobProgressInfo(info system.JobProgressInfo) {
	j := m.find(info.JobId)
	if j == nil {
		log.Warnf("Can't find Job %q when update info %v\n", info.JobId, info)
		return
	}

	if j._UpdateInfo(info) {
		m.changed = true
	}
}
