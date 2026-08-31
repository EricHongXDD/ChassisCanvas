package trayapp

import (
	"context"
	"fmt"
	"time"

	appassets "casescreen/assets"
	"casescreen/internal/service"

	"github.com/gogpu/systray"
)

type Options struct {
	URL          string
	Service      *service.Service
	SetTelemetry func(bool) error
	Cancel       context.CancelFunc
}

func Run(ctx context.Context, options Options) error {
	if options.Service == nil || options.SetTelemetry == nil || options.Cancel == nil {
		return fmt.Errorf("托盘参数不完整")
	}
	icon, err := makeIcon()
	if err != nil {
		return err
	}
	tray := systray.New()
	menu := systray.NewMenu()
	menu.Add("打开控制台", func() { _ = openURL(options.URL) })
	var telemetryItem *systray.MenuItem
	telemetryItem = menu.AddCheckbox("发送监控数据", options.Service.Status().TelemetryEnabled, func() {
		enabled := !telemetryItem.IsChecked()
		if err := options.SetTelemetry(enabled); err != nil {
			tray.ShowNotification("CaseScreen", "设置保存失败："+err.Error())
			return
		}
		telemetryItem.SetChecked(enabled)
	})
	menu.AddSeparator()
	menu.Add("退出", func() {
		options.Cancel()
		tray.Remove()
	})
	tray.SetIcon(icon).SetDarkModeIcon(icon).SetTooltip("CaseScreen 机箱副屏").SetMenu(menu)
	tray.OnClick(func() { _ = openURL(options.URL) })
	tray.Show()

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				tray.Remove()
				return
			case <-ticker.C:
				status := options.Service.Status()
				telemetryItem.SetChecked(status.TelemetryEnabled)
				tray.SetTooltip(fmt.Sprintf("CaseScreen · %d 台设备", len(status.Devices)))
			}
		}
	}()
	if err := tray.Run(); err != nil {
		return fmt.Errorf("托盘消息循环失败: %w", err)
	}
	return nil
}

func makeIcon() ([]byte, error) {
	return appassets.IconPNG(), nil
}
