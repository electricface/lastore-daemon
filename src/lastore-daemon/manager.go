package main

import (
	"internal/system"
)

type Manager struct {
	b      system.System
	config *Config

	JobList    []*Job
	jobManager *JobManager

	SystemArchitectures []system.Architecture

	UpgradableApps []string

	SystemOnChanging bool
}

func NewManager(b system.System, c *Config) *Manager {
	m := &Manager{
		config:              c,
		b:                   b,
		SystemArchitectures: b.SystemArchitectures(),
	}
	m.jobManager = NewJobManager(b, m.updateJobList)

	b.AttachIndicator(m.jobManager.handleJobProgressInfo)

	go m.jobManager.Dispatch()

	m.updatableApps()
	m.updateJobList()
	return m
}

func (m *Manager) UpdatePackage(packageId string) (*Job, error) {
	// TODO: Check whether the package can be updated
	return m.jobManager.CreateJob(system.UpdateJobType, packageId)
}

func (m *Manager) InstallPackage(packageId string) (*Job, error) {
	if m.PackageExists(packageId) {
		return nil, system.ResourceExitError
	}
	return m.jobManager.CreateJob(system.InstallJobType, packageId)
}

func (m *Manager) DownloadPackage(packageId string) (*Job, error) {
	if m.PackageExists(packageId) {
		return nil, system.ResourceExitError
	}
	return m.jobManager.CreateJob(system.DownloadJobType, packageId)
}

func (m *Manager) RemovePackage(packageId string) (*Job, error) {
	if !m.PackageExists(packageId) {
		return nil, system.NotFoundError
	}
	return m.jobManager.CreateJob(system.RemoveJobType, packageId)
}

func (m *Manager) UpdateSource() (*Job, error) {
	return m.jobManager.CreateJob(system.UpdateSourceJobType, "")
}

func (m *Manager) DistUpgrade() (*Job, error) {
	var updateJobIds []string
	for _, job := range m.JobList {
		if job.Type == system.DistUpgradeJobType {
			return nil, system.ResourceExitError
		}
		if job.Type == system.UpdateJobType && job.Status != system.RunningStatus {
			updateJobIds = append(updateJobIds, job.Id)
		}
	}
	for _, jobId := range updateJobIds {
		m.CleanJob(jobId)
	}

	return m.jobManager.CreateJob(system.DistUpgradeJobType, "")
}

func (m *Manager) StartJob(jobId string) error {
	return m.jobManager.MarkStart(jobId)
}
func (m *Manager) PauseJob(jobId string) error {
	return m.jobManager.PauseJob(jobId)
}
func (m *Manager) CleanJob(jobId string) error {
	return m.jobManager.CleanJob(jobId)
}

func (m *Manager) PackageExists(packageId string) bool {
	return m.b.CheckInstalled(packageId)
}

func (m *Manager) PackagesDownloadSize(packages []string) int64 {
	if len(packages) == 1 && m.PackageExists(packages[0]) {
		return 0
	}
	return int64(QueryPackageDownloadSize(packages...))
}

func (m *Manager) PackageDesktopPath(packageId string) string {
	r := QueryDesktopPath(packageId)
	if r != "" {
		return r
	}
	return QueryDesktopPath(QueryPackageSameNameDepends(packageId)...)
}

func (m *Manager) SetRegion(region string) error {
	return m.config.SetAppstoreRegion(region)
}
