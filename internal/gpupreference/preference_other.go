//go:build !windows

package gpupreference

import "errors"

func platformStatus(targets []ApplicationStatus) (Status, error) {
	applications := append([]ApplicationStatus(nil), targets...)
	for index := range applications {
		applications[index].Preference = SystemDefault
	}
	return statusFromApplications(false, applications), nil
}

func platformApply(targets []ApplicationStatus, _ Preference) (ApplyResult, error) {
	status, _ := platformStatus(targets)
	return ApplyResult{Status: status}, errors.New("按进程 GPU 首选项仅受 Windows 支持")
}
