package coolermaster

import (
	"errors"
	"testing"
)

func TestOEMControlConflictRequiresEveryControllerToBeStopped(t *testing.T) {
	for _, test := range []struct {
		service bool
		agent   bool
		wantErr bool
	}{
		{wantErr: false},
		{service: true, wantErr: true},
		{agent: true, wantErr: true},
		{service: true, agent: true, wantErr: true},
	} {
		err := oemControlConflict(test.service, test.agent)
		if errors.Is(err, ErrOEMControlConflict) != test.wantErr {
			t.Fatalf("service=%t agent=%t err=%v，wantErr=%t", test.service, test.agent, err, test.wantErr)
		}
	}
}

func TestOEMOwnershipConflictBlocksDelayedAutomaticStartup(t *testing.T) {
	for _, test := range []struct {
		name                string
		serviceActive       bool
		serviceMayAutoStart bool
		agentActive         bool
		wantErr             bool
	}{
		{name: "全部停止", wantErr: false},
		{name: "服务正在运行", serviceActive: true, wantErr: true},
		{name: "代理正在运行", agentActive: true, wantErr: true},
		{name: "尚未运行但稍后会自动启动", serviceMayAutoStart: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := oemControlOwnershipConflict(
				test.serviceActive,
				test.serviceMayAutoStart,
				test.agentActive,
			)
			if errors.Is(err, ErrOEMControlConflict) != test.wantErr {
				t.Fatalf("err=%v，wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestOEMServiceAutomaticStartupAccountsForTrayAndDisabledState(t *testing.T) {
	for _, test := range []struct {
		name             string
		serviceAutomatic bool
		serviceDisabled  bool
		trayAutoStart    bool
		want             bool
	}{
		{name: "服务自启", serviceAutomatic: true, want: true},
		{name: "手动服务但托盘登录启动", trayAutoStart: true, want: true},
		{name: "已禁用服务可保留无效托盘项", serviceDisabled: true, trayAutoStart: true, want: false},
		{name: "完全手动", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := oemServiceMayAutoStart(
				test.serviceAutomatic,
				test.serviceDisabled,
				test.trayAutoStart,
			)
			if got != test.want {
				t.Fatalf("got=%t want=%t", got, test.want)
			}
		})
	}
}
