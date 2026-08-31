package workbench

import (
	"fmt"
	"image/color"
	"reflect"
	"strconv"
	"strings"
	"time"

	"casescreen/internal/config"
	"casescreen/internal/device"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// waveshareLibraryState 只在微雪目标下使用，不改变其他设备的界面或主题流程。
type waveshareLibraryState struct {
	list                  widget.List
	settingsList          widget.List
	loadedTarget          string
	loading               bool
	operationRunning      bool
	templates             []config.WaveshareTemplate
	playlist              config.WavesharePlaylist
	activeTemplateID      string
	editingTemplateID     string
	renamingTemplateID    string
	storage               *device.StorageInfo
	storageFound          bool
	lastRefresh           time.Time
	statusRefreshing      bool
	statusRefreshVersion  uint64
	activeRefreshVersion  uint64
	activeRefreshPending  bool
	templateName          textField
	renameName            textField
	defaultDuration       textField
	durationFields        map[string]*textField
	playlistRuntime       WavesharePlaylistRuntime
	playlistBaseline      wavesharePlaylistDraft
	playlistBaselineReady bool
	playlistDrafts        map[string]wavesharePlaylistDraft
	playlistStateVersion  uint64
}

// wavesharePlaylistDraft 同时保存结构化设置和编辑框原文，确保无效或未完成的输入
// 在切换目标屏幕后也不会被静默丢弃。
type wavesharePlaylistDraft struct {
	Playlist        config.WavesharePlaylist
	DefaultDuration string
	Durations       map[string]string
}

func (state *waveshareLibraryState) init() {
	state.list.Axis = layout.Vertical
	state.settingsList.Axis = layout.Vertical
	if state.durationFields == nil {
		state.durationFields = make(map[string]*textField)
		state.defaultDuration.set(strconv.Itoa(config.DefaultWavesharePlaylistDurationSeconds))
	}
	if state.playlistDrafts == nil {
		state.playlistDrafts = make(map[string]wavesharePlaylistDraft)
	}
	state.templateName.editor.SingleLine = true
	state.renameName.editor.SingleLine = true
	state.defaultDuration.editor.SingleLine = true
	if state.playlist.Mode == "" {
		state.playlist.Mode = "sequence"
	}
}

func (state *waveshareLibraryState) reset(target string) {
	state.init()
	state.loadedTarget = target
	state.loading = false
	state.operationRunning = false
	state.templates = nil
	state.playlist = config.WavesharePlaylist{Mode: "sequence", DefaultDurationSeconds: config.DefaultWavesharePlaylistDurationSeconds}
	state.defaultDuration.set(strconv.Itoa(config.DefaultWavesharePlaylistDurationSeconds))
	state.playlistRuntime = WavesharePlaylistRuntime{}
	state.activeTemplateID = ""
	state.editingTemplateID = ""
	state.renamingTemplateID = ""
	state.renameName.set("")
	state.storage = nil
	state.storageFound = false
	state.lastRefresh = time.Time{}
	state.statusRefreshing = false
	state.statusRefreshVersion++
	state.activeRefreshVersion++
	state.activeRefreshPending = false
	state.durationFields = make(map[string]*textField)
	state.playlistBaseline = wavesharePlaylistDraft{}
	state.playlistBaselineReady = false
	state.playlistStateVersion++
}

func (model *gioEditor) layoutWaveshareLibrary(gtx layout.Context) layout.Dimensions {
	state := &model.waveshareLibrary
	state.init()
	model.ensureWaveshareLibraryLoaded()
	return state.list.Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return model.layoutWavesharePageIntro(gtx, "模板制作", "先在左侧画布完成设计，再命名保存；模板内容与屏幕轮播设置彼此独立。")
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(14)}.Layout(gtx, model.layoutWaveshareTemplateSave)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(18), Bottom: unit.Dp(12)}.Layout(gtx, model.layoutWaveshareTemplates)
			}),
		)
	})
}

// layoutWaveshareSettings 集中承载屏幕运行期设置，避免把轮播选项混入模板创建流程。
func (model *gioEditor) layoutWaveshareSettings(gtx layout.Context) layout.Dimensions {
	state := &model.waveshareLibrary
	state.init()
	model.ensureWaveshareLibraryLoaded()
	if model.periodicUIWorkEnabled() && model.api != nil && !state.loading && !state.statusRefreshing && !state.lastRefresh.IsZero() && time.Since(state.lastRefresh) >= 10*time.Second {
		model.refreshWaveshareSettingsStatus()
	}
	// 仅打开微雪设置时定时刷新，不为模板制作或其他设备界面增加后台请求。
	if model.periodicUIWorkEnabled() {
		gtx.Execute(op.InvalidateCmd{At: time.Now().Add(5 * time.Second)})
	}
	return state.settingsList.Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return model.layoutWavesharePageIntro(gtx, "微雪屏端设置", "当前目标："+model.waveshareTargetContext()+"。这里管理存储与轮播，不会修改模板内容。")
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(18)}.Layout(gtx, model.layoutWaveshareStorage)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(18), Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					if !state.playlistBaselineReady {
						return model.layoutWavesharePlaylistLoading(gtx)
					}
					return model.layoutWavesharePlaylist(gtx)
				})
			}),
		)
	})
}

func (model *gioEditor) layoutWavesharePlaylistLoading(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return sectionTitle(model, gtx, "模板轮播")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(model.theme, "正在读取该屏幕的轮播设置，完成后即可编辑。")
				label.Color = color.NRGBA{R: 151, G: 178, B: 188, A: 255}
				return label.Layout(gtx)
			})
		}),
	)
}

func (model *gioEditor) layoutWavesharePageIntro(gtx layout.Context, title, description string) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := material.H6(model.theme, title)
			label.Color = color.NRGBA{R: 113, G: 224, B: 220, A: 255}
			return label.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(model.theme, description)
				label.Color = color.NRGBA{R: 151, G: 178, B: 188, A: 255}
				return label.Layout(gtx)
			})
		}),
	)
}

func (model *gioEditor) layoutWaveshareStorage(gtx layout.Context) layout.Dimensions {
	state := &model.waveshareLibrary
	status := "正在读取 TF 卡容量…"
	if !state.loading {
		status = waveshareStorageStatus(state.storage, state.storageFound)
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return material.Body1(model.theme, "TF 卡容量").Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return model.buttonSizedState(gtx, model.optionButton("waveshare-storage-refresh"), "刷新", model.refreshWaveshareStorage, 72, state.loading)
				}),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(model.theme, status)
				label.Color = color.NRGBA{R: 151, G: 178, B: 188, A: 255}
				return label.Layout(gtx)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if state.lastRefresh.IsZero() {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: unit.Dp(3)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				text := "最后同步 " + state.lastRefresh.Format("15:04:05")
				if state.storage != nil && !state.storage.UpdatedAt.IsZero() {
					text += " · 设备数据 " + state.storage.UpdatedAt.Local().Format("15:04:05")
				}
				label := material.Caption(model.theme, text)
				label.Color = color.NRGBA{R: 112, G: 137, B: 147, A: 255}
				return label.Layout(gtx)
			})
		}),
	)
}

// waveshareStorageStatus 优先保留最近一次有效容量。后台轮询偶发超时时，
// 用户仍能看到可用空间，同时明确这些数字来自上次成功查询。
func waveshareStorageStatus(storage *device.StorageInfo, found bool) string {
	switch {
	case !found:
		return "服务尚未返回 TF 卡信息"
	case storage == nil:
		return "TF 卡信息不可用"
	case storage.Supported && storage.TotalBytes > 0:
		status := fmt.Sprintf("总容量 %s  已用 %s  可用 %s", formatStorageBytes(storage.TotalBytes), formatStorageBytes(storage.UsedBytes), formatStorageBytes(storage.AvailableBytes))
		if storage.Error != "" {
			status += "（最近刷新失败，显示上次有效数据）"
		}
		return status
	case storage.Error != "":
		return "容量刷新失败：" + storage.Error
	case !storage.Supported:
		return "当前微雪固件未报告 TF 卡容量"
	default:
		return "TF 卡信息不可用"
	}
}

func (model *gioEditor) layoutWaveshareTemplateSave(gtx layout.Context) layout.Dimensions {
	state := &model.waveshareLibrary
	contextText := "当前目标：" + model.waveshareTargetContext()
	editingRecord, editing := model.waveshareTemplateByID(state.editingTemplateID)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return sectionTitle(model, gtx, "1 · 创建模板")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := material.Caption(model.theme, contextText)
			label.Color = color.NRGBA{R: 151, G: 178, B: 188, A: 255}
			return label.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				if !editing {
					label := material.Body2(model.theme, "新建模式：保存后会生成一个新模板")
					label.Color = color.NRGBA{R: 112, G: 160, B: 151, A: 255}
					return label.Layout(gtx)
				}
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						label := material.Body2(model.theme, "更新模式："+editingRecord.Name)
						label.Color = color.NRGBA{R: 255, G: 205, B: 90, A: 255}
						return label.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return model.buttonSized(gtx, model.optionButton("waveshare-template-new-mode"), "改为新建", model.clearWaveshareTemplateUpdate, 86)
					}),
				)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.layoutTextField(gtx, &state.templateName, "输入模板名称")
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := "创建新模板"
				if editing {
					label = "用当前画布更新此模板"
				}
				if state.operationRunning {
					label = "正在处理…"
				}
				return model.buttonState(gtx, model.optionButton("waveshare-template-save"), label, model.saveCurrentWaveshareTemplate, state.operationRunning)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Caption(model.theme, "新建时不会静默覆盖同名模板；首次应用会上传新增资源，之后切换不会重复上传相同资源。")
				label.Color = color.NRGBA{R: 112, G: 137, B: 147, A: 255}
				return label.Layout(gtx)
			})
		}),
	)
}

func (model *gioEditor) waveshareTargetContext() string {
	targetLabel := model.target
	if option := model.targetOption(model.target); option != nil && option.label != "" {
		targetLabel = option.label
	}
	return fmt.Sprintf("%s  ·  %d × %d  ·  %d 个图层", targetLabel, model.manifest.Width, model.manifest.Height, len(model.manifest.Layers))
}

func (model *gioEditor) layoutWaveshareTemplates(gtx layout.Context) layout.Dimensions {
	state := &model.waveshareLibrary
	children := []layout.FlexChild{layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return sectionTitle(model, gtx, fmt.Sprintf("2 · 已保存模板（%d）", len(state.templates)))
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return model.buttonSizedState(gtx, model.optionButton("waveshare-library-refresh"), "刷新", model.reloadWaveshareLibrary, 72, state.loading)
			}),
		)
	})}
	if len(state.templates) == 0 {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(model.theme, "尚无模板，可把当前画布保存为第一个模板。")
				label.Color = color.NRGBA{R: 151, G: 178, B: 188, A: 255}
				return label.Layout(gtx)
			})
		}))
	}
	for _, record := range state.templates {
		record := record
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.layoutWaveshareTemplateRow(gtx, record)
			})
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (model *gioEditor) layoutWaveshareTemplateRow(gtx layout.Context, record config.WaveshareTemplate) layout.Dimensions {
	state := &model.waveshareLibrary
	title := record.Name
	if record.ID == state.activeTemplateID {
		title = "● " + title + "（已激活）"
	}
	detail := "ID " + record.ID
	if record.SizeBytes > 0 {
		detail += "  ·  " + formatStorageBytes(uint64(record.SizeBytes))
	}
	if !record.UpdatedAt.IsZero() {
		detail += "  ·  更新 " + record.UpdatedAt.Local().Format("01-02 15:04")
	}
	if state.renamingTemplateID == record.ID {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(model.theme, "重命名模板 · "+record.Name)
				label.Color = color.NRGBA{R: 255, G: 205, B: 90, A: 255}
				return label.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return model.layoutTextField(gtx, &state.renameName, "输入新的模板名称")
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Right: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return model.buttonState(gtx, model.optionButton("waveshare-rename-save-"+record.ID), "保存新名称", model.renameWaveshareTemplate, state.operationRunning)
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return model.buttonSized(gtx, model.optionButton("waveshare-rename-cancel-"+record.ID), "取消", model.cancelWaveshareTemplateRename, 72)
						}),
					)
				})
			}),
		)
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return material.Body2(model.theme, title).Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := material.Caption(model.theme, detail)
			label.Color = color.NRGBA{R: 116, G: 144, B: 154, A: 255}
			return label.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Right: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return model.buttonState(gtx, model.optionButton("waveshare-activate-"+record.ID), "显示并载入编辑", func() { model.activateWaveshareTemplate(record.ID) }, record.ID == state.editingTemplateID)
						})
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Right: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return model.buttonSized(gtx, model.optionButton("waveshare-rename-"+record.ID), "重命名", func() { model.beginWaveshareTemplateRename(record.ID) }, 76)
						})
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return model.buttonSizedState(gtx, model.optionButton("waveshare-delete-"+record.ID), "删除", func() { model.deleteWaveshareTemplate(record.ID) }, 64, false)
					}),
				)
			})
		}),
	)
}

func (model *gioEditor) layoutWavesharePlaylist(gtx layout.Context) layout.Dimensions {
	state := &model.waveshareLibrary
	mode := state.playlist.Mode
	if mode == "" {
		mode = "sequence"
	}
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return sectionTitle(model, gtx, "模板轮播")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := material.Caption(model.theme, "选择多个已保存模板，并设置屏幕自动切换方式。")
			label.Color = color.NRGBA{R: 151, G: 178, B: 188, A: 255}
			return label.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := "○ 轮播已停止"
				if state.playlist.Enabled {
					label = "● 轮播已启用"
				}
				return model.buttonState(gtx, model.optionButton("waveshare-playlist-enabled"), label, func() {
					state.playlist.Enabled = !state.playlist.Enabled
				}, state.playlist.Enabled)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.layoutChoiceButtons(gtx, "waveshare-playlist-mode", []optionChoice{{"sequence", "顺序"}, {"random", "随机"}}, mode, func(value string) {
					state.playlist.Mode = value
				})
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				dims := layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X, gtx.Constraints.Max.X = gtx.Metric.Dp(104), gtx.Metric.Dp(104)
						return material.Caption(model.theme, "统一时长（秒）").Layout(gtx)
					}),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return model.layoutTextField(gtx, &state.defaultDuration, strconv.Itoa(config.DefaultWavesharePlaylistDurationSeconds))
					}),
				)
				if duration, err := strconv.Atoi(strings.TrimSpace(state.defaultDuration.editor.Text())); err == nil && duration >= 1 && duration <= 86400 {
					state.playlist.DefaultDurationSeconds = duration
				}
				return dims
			})
		}),
	}
	if status := wavesharePlaylistRuntimeText(state.playlistRuntime); status != "" {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Caption(model.theme, status)
				if state.playlistRuntime.LastError != "" {
					label.Color = color.NRGBA{R: 238, G: 126, B: 126, A: 255}
				} else {
					label.Color = color.NRGBA{R: 112, G: 160, B: 151, A: 255}
				}
				return label.Layout(gtx)
			})
		}))
	}
	children = append(children,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return sectionTitle(model, gtx, fmt.Sprintf("参与轮播的模板（已选 %d / %d）", len(state.playlist.Entries), len(state.templates)))
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Right: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return model.buttonState(gtx, model.optionButton("waveshare-playlist-select-all"), "全选", model.selectAllWavesharePlaylistEntries, len(state.templates) > 0 && len(state.playlist.Entries) == len(state.templates))
						})
					}),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return model.buttonState(gtx, model.optionButton("waveshare-playlist-clear"), "清空", model.clearWavesharePlaylistEntries, len(state.playlist.Entries) == 0)
					}),
				)
			})
		}),
	)
	if len(state.templates) == 0 {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(model.theme, "还没有可参与轮播的模板，请先到“模板”页创建。")
				label.Color = color.NRGBA{R: 151, G: 178, B: 188, A: 255}
				return label.Layout(gtx)
			})
		}))
	}
	for _, record := range state.templates {
		record := record
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return model.layoutWavesharePlaylistEntry(gtx, record)
			})
		}))
	}
	children = append(children,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := "保存轮播设置"
				if state.playlistDirty() {
					label = "保存轮播设置（有未保存修改）"
				}
				if state.operationRunning {
					label = "正在处理…"
				}
				return model.buttonState(gtx, model.optionButton("waveshare-playlist-save"), label, model.saveWavesharePlaylist, state.operationRunning)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Caption(model.theme, "统一时长为 1–86400 秒；每个模板可留空继承，也可单独覆盖。顺序模式可用箭头调整播放次序。")
				label.Color = color.NRGBA{R: 112, G: 137, B: 147, A: 255}
				return label.Layout(gtx)
			})
		}),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (model *gioEditor) layoutWavesharePlaylistEntry(gtx layout.Context, record config.WaveshareTemplate) layout.Dimensions {
	state := &model.waveshareLibrary
	entryIndex := state.playlistEntryIndex(record.ID)
	included := entryIndex >= 0
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := "＋ " + record.Name
			if included {
				if state.playlist.Mode == "random" {
					label = "✓ " + record.Name
				} else {
					label = fmt.Sprintf("%02d  %s", entryIndex+1, record.Name)
				}
			}
			return model.buttonState(gtx, model.optionButton("waveshare-playlist-toggle-"+record.ID), label, func() {
				model.toggleWavesharePlaylistEntry(record.ID)
			}, included)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !included {
				return layout.Dimensions{}
			}
			field := state.durationField(record.ID, state.playlist.Entries[entryIndex].DurationSeconds)
			return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				children := []layout.FlexChild{
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X, gtx.Constraints.Max.X = gtx.Metric.Dp(104), gtx.Metric.Dp(104)
						return material.Caption(model.theme, "单独时长（秒）").Layout(gtx)
					}),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return model.layoutTextField(gtx, field, "继承统一时长")
					}),
				}
				if state.playlist.Mode != "random" {
					children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return model.buttonSized(gtx, model.optionButton("waveshare-playlist-up-"+record.ID), "↑", func() { model.moveWavesharePlaylistEntry(record.ID, -1) }, 34)
						})
					}), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(3)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return model.buttonSized(gtx, model.optionButton("waveshare-playlist-down-"+record.ID), "↓", func() { model.moveWavesharePlaylistEntry(record.ID, 1) }, 34)
						})
					}))
				}
				dims := layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx, children...)
				value := strings.TrimSpace(field.editor.Text())
				if value == "" {
					state.playlist.Entries[entryIndex].DurationSeconds = 0
				} else if duration, err := strconv.Atoi(value); err == nil && duration >= 1 && duration <= 86400 {
					state.playlist.Entries[entryIndex].DurationSeconds = duration
				}
				return dims
			})
		}),
	)
}

func (state *waveshareLibraryState) durationField(templateID string, duration int) *textField {
	state.init()
	field := state.durationFields[templateID]
	if field == nil {
		field = &textField{}
		field.editor.SingleLine = true
		if duration > 0 {
			field.set(strconv.Itoa(duration))
		} else {
			field.set("")
		}
		state.durationFields[templateID] = field
	}
	return field
}

func (state *waveshareLibraryState) playlistEntryIndex(templateID string) int {
	for index, entry := range state.playlist.Entries {
		if entry.TemplateID == templateID {
			return index
		}
	}
	return -1
}

func (model *gioEditor) toggleWavesharePlaylistEntry(templateID string) {
	state := &model.waveshareLibrary
	index := state.playlistEntryIndex(templateID)
	if index >= 0 {
		state.playlist.Entries = append(state.playlist.Entries[:index], state.playlist.Entries[index+1:]...)
		delete(state.durationFields, templateID)
		return
	}
	state.playlist.Entries = append(state.playlist.Entries, config.WavesharePlaylistEntry{TemplateID: templateID})
	state.durationField(templateID, 0)
}

func (model *gioEditor) selectAllWavesharePlaylistEntries() {
	state := &model.waveshareLibrary
	for _, record := range state.templates {
		if state.playlistEntryIndex(record.ID) >= 0 {
			continue
		}
		state.playlist.Entries = append(state.playlist.Entries, config.WavesharePlaylistEntry{TemplateID: record.ID})
		state.durationField(record.ID, 0)
	}
}

func (model *gioEditor) clearWavesharePlaylistEntries() {
	state := &model.waveshareLibrary
	state.playlist.Entries = nil
	state.durationFields = make(map[string]*textField)
}

func (model *gioEditor) moveWavesharePlaylistEntry(templateID string, delta int) {
	state := &model.waveshareLibrary
	from := state.playlistEntryIndex(templateID)
	to := from + delta
	if from < 0 || to < 0 || to >= len(state.playlist.Entries) {
		return
	}
	state.playlist.Entries[from], state.playlist.Entries[to] = state.playlist.Entries[to], state.playlist.Entries[from]
}

func (model *gioEditor) ensureWaveshareLibraryLoaded() {
	state := &model.waveshareLibrary
	if !model.isWaveshareTarget() {
		return
	}
	if state.loadedTarget != model.target {
		state.rememberPlaylistDraft()
		state.reset(model.target)
	}
	if model.api == nil || state.loading || !state.lastRefresh.IsZero() {
		return
	}
	model.refreshWaveshareLibrary()
}

func (model *gioEditor) reloadWaveshareLibrary() {
	if model.api == nil || !model.isWaveshareTarget() || model.waveshareLibrary.loading {
		return
	}
	target := model.target
	model.waveshareLibrary.loading = true
	model.ensureUIUpdates()
	go func() {
		library, err := model.api.WaveshareTemplates(target)
		model.uiUpdates <- func() {
			state := &model.waveshareLibrary
			if state.loadedTarget != target {
				return
			}
			state.loading = false
			if model.target != target {
				return
			}
			if err != nil {
				model.message = "刷新微雪模板库失败：" + err.Error()
				return
			}
			model.applyWaveshareTemplateLibrary(library)
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) refreshWaveshareLibrary() {
	if model.api == nil || !model.isWaveshareTarget() || model.waveshareLibrary.loading {
		return
	}
	target := model.target
	model.waveshareLibrary.loading = true
	model.ensureUIUpdates()
	go func() {
		library, libraryErr := model.api.WaveshareTemplates(target)
		playlist, runtime, playlistErr := model.api.WavesharePlaylistState(target)
		storage, storageFound, storageErr := model.api.WaveshareStorage(target)
		model.uiUpdates <- func() {
			state := &model.waveshareLibrary
			if state.loadedTarget != target {
				return
			}
			state.loading = false
			defer model.runPendingWaveshareActiveRefresh(target)
			if model.target != target {
				return
			}
			state.lastRefresh = time.Now()
			if libraryErr == nil {
				model.applyWaveshareTemplateLibrary(library)
			}
			if playlistErr == nil {
				state.playlistRuntime = runtime
				state.applyServerPlaylist(target, playlist)
			}
			if storageErr == nil {
				state.storage, state.storageFound = storage, storageFound
			}
			if libraryErr != nil {
				model.message = "读取微雪模板库失败：" + libraryErr.Error()
			} else if playlistErr != nil {
				model.message = "读取微雪轮播设置失败：" + playlistErr.Error()
			} else if storageErr != nil {
				model.message = "读取 TF 卡容量失败：" + storageErr.Error()
			}
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) applyWaveshareTemplateLibrary(library WaveshareTemplateLibrary) {
	state := &model.waveshareLibrary
	state.templates = append([]config.WaveshareTemplate(nil), library.Templates...)
	state.activeTemplateID = library.ActiveTemplateID
	if state.editingTemplateID != "" {
		if _, exists := model.waveshareTemplateByID(state.editingTemplateID); !exists {
			state.editingTemplateID = ""
			state.templateName.set("")
		}
	}
	if state.renamingTemplateID != "" {
		if _, exists := model.waveshareTemplateByID(state.renamingTemplateID); !exists {
			state.renamingTemplateID = ""
			state.renameName.set("")
		}
	}
}

// refreshWaveshareSettingsStatus 只读取服务已缓存的容量和轮播运行态。
// 它不会主动访问 TF 卡，也不会覆盖设置页中尚未保存的轮播草稿。
func (model *gioEditor) refreshWaveshareSettingsStatus() {
	state := &model.waveshareLibrary
	if model.api == nil || !model.isWaveshareTarget() || state.loading || state.statusRefreshing {
		return
	}
	target := model.target
	state.statusRefreshing = true
	state.statusRefreshVersion++
	version := state.statusRefreshVersion
	model.ensureUIUpdates()
	go func() {
		playlist, runtime, playlistErr := model.api.WavesharePlaylistState(target)
		storage, storageFound, storageErr := model.api.WaveshareStorage(target)
		model.uiUpdates <- func() {
			state := &model.waveshareLibrary
			if state.loadedTarget != target || state.statusRefreshVersion != version {
				return
			}
			state.statusRefreshing = false
			if model.target != target {
				return
			}
			state.lastRefresh = time.Now()
			if playlistErr == nil {
				state.playlistRuntime = runtime
				if !state.playlistBaselineReady {
					state.applyServerPlaylist(target, playlist)
				}
			}
			if storageErr == nil {
				state.storage, state.storageFound = storage, storageFound
			}
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) refreshWaveshareStorage() {
	if model.api == nil || !model.isWaveshareTarget() || model.waveshareLibrary.loading {
		return
	}
	target := model.target
	// 用户主动刷新优先于正在进行的轻量缓存同步，避免旧缓存晚到后覆盖新查询结果。
	model.waveshareLibrary.statusRefreshVersion++
	model.waveshareLibrary.statusRefreshing = false
	model.waveshareLibrary.loading = true
	model.ensureUIUpdates()
	go func() {
		playlist, runtime, playlistErr := model.api.WavesharePlaylistState(target)
		storage, err := model.api.RefreshWaveshareStorage(target)
		model.uiUpdates <- func() {
			state := &model.waveshareLibrary
			if state.loadedTarget != target {
				return
			}
			state.loading = false
			defer model.runPendingWaveshareActiveRefresh(target)
			if model.target != target {
				return
			}
			state.lastRefresh = time.Now()
			if playlistErr == nil {
				state.playlistRuntime = runtime
				if !state.playlistBaselineReady {
					state.applyServerPlaylist(target, playlist)
				}
			}
			if err != nil {
				model.message = "读取 TF 卡容量失败：" + err.Error()
				return
			}
			state.storage, state.storageFound = storage, storage != nil
			if playlistErr != nil {
				model.message = "已向设备查询 TF 卡容量，但轮播状态读取失败：" + playlistErr.Error()
			} else {
				model.message = "已向设备查询 TF 卡容量，并刷新轮播状态"
			}
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) saveCurrentWaveshareTemplate() {
	state := &model.waveshareLibrary
	if state.operationRunning || model.api == nil || !model.isWaveshareTarget() {
		return
	}
	name := strings.TrimSpace(state.templateName.editor.Text())
	if name == "" {
		model.message = "请输入模板名称"
		return
	}
	if state.editingTemplateID == "" {
		if record, exists := model.matchingWaveshareTemplate(name); exists {
			model.message = fmt.Sprintf("模板名称 %q 已存在；请在下方对 %q 点击“显示并载入编辑”，或换一个名称", name, record.Name)
			return
		}
	} else if _, exists := model.waveshareTemplateByID(state.editingTemplateID); !exists {
		state.editingTemplateID = ""
		model.message = "要更新的模板已不存在，请重新选择"
		return
	}
	model.adaptManifestToTarget()
	manifest := cloneManifest(model.manifest)
	target := model.target
	templateID := state.editingTemplateID
	state.operationRunning = true
	model.ensureUIUpdates()
	go func() {
		_, err := model.api.SaveWaveshareTemplate(target, templateID, name, manifest)
		model.uiUpdates <- func() {
			if state.loadedTarget != target {
				return
			}
			state.operationRunning = false
			if model.target != target {
				return
			}
			if err != nil {
				model.message = "保存微雪模板失败：" + err.Error()
				return
			}
			state.templateName.set("")
			state.editingTemplateID = ""
			model.message = "模板已编译并保存；首次激活会写入 TF，后续切换免重复上传"
			// 保存模板后只重载模板记录，保留设置页尚未保存的轮播多选、顺序和时长。
			model.reloadWaveshareLibrary()
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) waveshareTemplateByID(templateID string) (config.WaveshareTemplate, bool) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return config.WaveshareTemplate{}, false
	}
	for _, record := range model.waveshareLibrary.templates {
		if record.ID == templateID {
			return record, true
		}
	}
	return config.WaveshareTemplate{}, false
}

func (model *gioEditor) selectWaveshareTemplateForUpdate(templateID string) {
	record, exists := model.waveshareTemplateByID(templateID)
	if !exists {
		model.message = "要更新的模板已不存在，请刷新模板库"
		return
	}
	model.waveshareLibrary.editingTemplateID = record.ID
	model.waveshareLibrary.templateName.set(record.Name)
	model.waveshareLibrary.list.Position = layout.Position{}
	model.message = "已选择更新模板：" + record.Name
}

func (model *gioEditor) beginWaveshareTemplateRename(templateID string) {
	record, exists := model.waveshareTemplateByID(templateID)
	if !exists {
		model.message = "要重命名的模板已不存在，请刷新模板库"
		return
	}
	state := &model.waveshareLibrary
	state.renamingTemplateID = record.ID
	state.renameName.set(record.Name)
	model.message = "请输入新的模板名称；重命名不会重新编译或上传资源"
}

func (model *gioEditor) cancelWaveshareTemplateRename() {
	state := &model.waveshareLibrary
	state.renamingTemplateID = ""
	state.renameName.set("")
	model.message = "已取消模板重命名"
}

func (model *gioEditor) renameWaveshareTemplate() {
	state := &model.waveshareLibrary
	if state.operationRunning || model.api == nil || !model.isWaveshareTarget() || state.renamingTemplateID == "" {
		return
	}
	record, exists := model.waveshareTemplateByID(state.renamingTemplateID)
	if !exists {
		model.message = "要重命名的模板已不存在，请刷新模板库"
		return
	}
	name := strings.TrimSpace(state.renameName.editor.Text())
	if name == "" {
		model.message = "模板名称不能为空"
		return
	}
	if strings.EqualFold(strings.TrimSpace(record.Name), name) {
		model.cancelWaveshareTemplateRename()
		model.message = "模板名称没有变化"
		return
	}
	if matched, duplicate := model.matchingWaveshareTemplate(name); duplicate && matched.ID != record.ID {
		model.message = "同一屏幕已存在同名模板：" + matched.Name
		return
	}
	target, templateID := model.target, record.ID
	state.operationRunning = true
	model.ensureUIUpdates()
	go func() {
		updated, err := model.api.RenameWaveshareTemplate(target, templateID, name)
		model.uiUpdates <- func() {
			state := &model.waveshareLibrary
			if state.loadedTarget != target || model.target != target {
				return
			}
			state.operationRunning = false
			if err != nil {
				model.message = "重命名模板失败：" + err.Error()
				return
			}
			for index := range state.templates {
				if state.templates[index].ID == updated.ID {
					state.templates[index] = updated
					break
				}
			}
			if state.editingTemplateID == updated.ID {
				state.templateName.set(updated.Name)
			}
			state.renamingTemplateID = ""
			state.renameName.set("")
			model.message = "模板已重命名；模板 ID、轮播引用和 TF 资源保持不变"
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) clearWaveshareTemplateUpdate() {
	model.waveshareLibrary.editingTemplateID = ""
	model.waveshareLibrary.templateName.set("")
	model.message = "已切换为新建模板模式"
}

func (model *gioEditor) matchingWaveshareTemplate(name string) (config.WaveshareTemplate, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return config.WaveshareTemplate{}, false
	}
	for _, record := range model.waveshareLibrary.templates {
		if strings.EqualFold(strings.TrimSpace(record.Name), name) {
			return record, true
		}
	}
	return config.WaveshareTemplate{}, false
}

func (model *gioEditor) activateWaveshareTemplate(templateID string) {
	state := &model.waveshareLibrary
	if state.operationRunning || model.api == nil || !model.isWaveshareTarget() {
		return
	}
	record, exists := model.waveshareTemplateByID(templateID)
	if !exists {
		model.message = "要编辑的模板已不存在，请刷新模板库"
		return
	}
	target := model.target
	state.operationRunning = true
	model.ensureUIUpdates()
	go func() {
		err := model.api.ActivateWaveshareTemplate(target, templateID)
		manifest, manifestErr := model.api.Theme(target)
		model.uiUpdates <- func() {
			if state.loadedTarget != target {
				return
			}
			state.operationRunning = false
			if model.target != target {
				return
			}
			if err != nil {
				model.message = "激活微雪模板失败：" + err.Error()
				return
			}
			state.activeTemplateID = templateID
			state.editingTemplateID = templateID
			state.templateName.set(record.Name)
			if manifestErr == nil && manifest.ID != "" {
				model.manifest = manifest
				model.adaptManifestToTarget()
				model.selected = 0
				model.componentLayerID, model.lyricsLayerID = "", ""
			}
			state.list.Position = layout.Position{}
			model.message = "模板已显示并进入更新模式；修改画布后可直接保存，驻留资源不会重复上传"
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) deleteWaveshareTemplate(templateID string) {
	state := &model.waveshareLibrary
	if state.operationRunning || model.api == nil || !model.isWaveshareTarget() {
		return
	}
	target := model.target
	if state.editingTemplateID == templateID {
		state.editingTemplateID = ""
		state.templateName.set("")
	}
	state.operationRunning = true
	model.ensureUIUpdates()
	go func() {
		err := model.api.DeleteWaveshareTemplate(target, templateID)
		model.uiUpdates <- func() {
			if state.loadedTarget != target {
				return
			}
			state.operationRunning = false
			if model.target != target {
				return
			}
			if err != nil {
				model.message = "删除本地模板记录失败：" + err.Error()
				return
			}
			model.removeWavesharePlaylistEntry(templateID)
			model.message = "已删除本地模板记录；屏内共享资源未强制擦除"
			model.reloadWaveshareLibrary()
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) removeWavesharePlaylistEntry(templateID string) {
	state := &model.waveshareLibrary
	index := state.playlistEntryIndex(templateID)
	if index >= 0 {
		state.playlist.Entries = append(state.playlist.Entries[:index], state.playlist.Entries[index+1:]...)
	}
	delete(state.durationFields, templateID)
	if len(state.playlist.Entries) == 0 {
		state.playlist.Enabled = false
	}
}

func (model *gioEditor) saveWavesharePlaylist() {
	state := &model.waveshareLibrary
	if state.operationRunning || model.api == nil || !model.isWaveshareTarget() {
		return
	}
	playlist, err := state.playlistSnapshot()
	if err != nil {
		model.message = err.Error()
		return
	}
	target := model.target
	submittedUI := state.currentPlaylistDraft()
	stateVersion := state.playlistStateVersion
	state.operationRunning = true
	model.ensureUIUpdates()
	go func() {
		err := model.api.SaveWavesharePlaylist(target, playlist)
		model.uiUpdates <- func() {
			if state.loadedTarget != target || state.playlistStateVersion != stateVersion {
				return
			}
			state.operationRunning = false
			if model.target != target {
				return
			}
			if err != nil {
				model.message = "保存微雪轮播设置失败：" + err.Error()
				return
			}
			currentUI := state.currentPlaylistDraft()
			state.playlistBaseline = playlistDraftFromPlaylist(playlist)
			state.playlistBaselineReady = true
			if reflect.DeepEqual(currentUI, submittedUI) {
				state.playlist = cloneWavesharePlaylist(playlist)
				state.syncDurationFields()
				state.playlistBaseline = state.currentPlaylistDraft()
				delete(state.playlistDrafts, target)
				model.message = "微雪轮播设置已保存"
				return
			}
			// 请求期间发生的后续编辑仍是当前草稿，不能被刚保存的旧快照覆盖。
			state.playlistDrafts[target] = currentUI
			model.message = "轮播设置已保存；保存期间的后续修改仍未保存"
		}
		model.invalidateWindow()
	}()
}

func (state *waveshareLibraryState) playlistSnapshot() (config.WavesharePlaylist, error) {
	playlist := cloneWavesharePlaylist(state.playlist)
	if playlist.Mode == "" {
		playlist.Mode = "sequence"
	}
	if playlist.Enabled && len(playlist.Entries) == 0 {
		return config.WavesharePlaylist{}, fmt.Errorf("启用模板轮播前，请至少选择一个参与轮播的模板")
	}
	defaultDuration, err := strconv.Atoi(strings.TrimSpace(state.defaultDuration.editor.Text()))
	if err != nil || defaultDuration < 1 || defaultDuration > 86400 {
		return config.WavesharePlaylist{}, fmt.Errorf("请把统一轮播时长设为 1–86400 秒")
	}
	playlist.DefaultDurationSeconds = defaultDuration
	for index := range playlist.Entries {
		entry := &playlist.Entries[index]
		field := state.durationField(entry.TemplateID, entry.DurationSeconds)
		value := strings.TrimSpace(field.editor.Text())
		if value == "" {
			entry.DurationSeconds = 0
			continue
		}
		duration, parseErr := strconv.Atoi(value)
		if parseErr != nil || duration < 1 || duration > 86400 {
			return config.WavesharePlaylist{}, fmt.Errorf("模板 %q 的单独时长必须留空继承，或设为 1–86400 秒", entry.TemplateID)
		}
		entry.DurationSeconds = duration
	}
	return playlist, nil
}

func cloneWavesharePlaylist(source config.WavesharePlaylist) config.WavesharePlaylist {
	cloned := source
	if len(source.Entries) == 0 {
		cloned.Entries = nil
	} else {
		cloned.Entries = append([]config.WavesharePlaylistEntry(nil), source.Entries...)
	}
	return cloned
}

func playlistDraftFromPlaylist(playlist config.WavesharePlaylist) wavesharePlaylistDraft {
	playlist = cloneWavesharePlaylist(playlist)
	if playlist.Mode == "" {
		playlist.Mode = "sequence"
	}
	defaultDuration := playlist.DefaultDurationSeconds
	if defaultDuration <= 0 {
		defaultDuration = config.DefaultWavesharePlaylistDurationSeconds
		playlist.DefaultDurationSeconds = defaultDuration
	}
	draft := wavesharePlaylistDraft{
		Playlist:        playlist,
		DefaultDuration: strconv.Itoa(defaultDuration),
		Durations:       make(map[string]string, len(playlist.Entries)),
	}
	for _, entry := range playlist.Entries {
		if entry.DurationSeconds > 0 {
			draft.Durations[entry.TemplateID] = strconv.Itoa(entry.DurationSeconds)
		} else {
			draft.Durations[entry.TemplateID] = ""
		}
	}
	return draft
}

func (state *waveshareLibraryState) currentPlaylistDraft() wavesharePlaylistDraft {
	draft := wavesharePlaylistDraft{
		Playlist:        cloneWavesharePlaylist(state.playlist),
		DefaultDuration: state.defaultDuration.editor.Text(),
		Durations:       make(map[string]string, len(state.playlist.Entries)),
	}
	for _, entry := range state.playlist.Entries {
		if field := state.durationFields[entry.TemplateID]; field != nil {
			draft.Durations[entry.TemplateID] = field.editor.Text()
			continue
		}
		if entry.DurationSeconds > 0 {
			draft.Durations[entry.TemplateID] = strconv.Itoa(entry.DurationSeconds)
		} else {
			draft.Durations[entry.TemplateID] = ""
		}
	}
	return draft
}

func (state *waveshareLibraryState) playlistDirty() bool {
	return state.playlistBaselineReady && !reflect.DeepEqual(state.currentPlaylistDraft(), state.playlistBaseline)
}

func (state *waveshareLibraryState) rememberPlaylistDraft() {
	target := strings.TrimSpace(state.loadedTarget)
	if target == "" || !state.playlistBaselineReady {
		return
	}
	if state.playlistDirty() {
		state.playlistDrafts[target] = state.currentPlaylistDraft()
		return
	}
	delete(state.playlistDrafts, target)
}

func (state *waveshareLibraryState) applyServerPlaylist(target string, playlist config.WavesharePlaylist) {
	state.playlist = cloneWavesharePlaylist(playlist)
	if state.playlist.Mode == "" {
		state.playlist.Mode = "sequence"
	}
	state.syncDurationFields()
	state.playlistBaseline = state.currentPlaylistDraft()
	state.playlistBaselineReady = true
	if draft, exists := state.playlistDrafts[target]; exists {
		state.restorePlaylistDraft(draft)
	}
}

func (state *waveshareLibraryState) restorePlaylistDraft(draft wavesharePlaylistDraft) {
	state.playlist = cloneWavesharePlaylist(draft.Playlist)
	if state.playlist.Mode == "" {
		state.playlist.Mode = "sequence"
	}
	state.defaultDuration.set(draft.DefaultDuration)
	state.durationFields = make(map[string]*textField, len(state.playlist.Entries))
	for _, entry := range state.playlist.Entries {
		field := &textField{}
		field.editor.SingleLine = true
		field.set(draft.Durations[entry.TemplateID])
		state.durationFields[entry.TemplateID] = field
	}
}

// refreshWaveshareActiveTemplate 在普通“应用主题”后重新读取模板激活态，不改写用户尚未保存的轮播编辑。
func (model *gioEditor) refreshWaveshareActiveTemplate(target string) {
	state := &model.waveshareLibrary
	if model.api == nil || state.loadedTarget != target {
		return
	}
	state.activeTemplateID = ""
	if state.loading {
		state.activeRefreshPending = true
		return
	}
	state.activeRefreshPending = false
	state.activeRefreshVersion++
	version := state.activeRefreshVersion
	model.ensureUIUpdates()
	go func() {
		library, err := model.api.WaveshareTemplates(target)
		model.uiUpdates <- func() {
			state := &model.waveshareLibrary
			if err != nil || state.loadedTarget != target || state.activeRefreshVersion != version {
				return
			}
			state.templates = append([]config.WaveshareTemplate(nil), library.Templates...)
			state.activeTemplateID = library.ActiveTemplateID
		}
		model.invalidateWindow()
	}()
}

func (model *gioEditor) runPendingWaveshareActiveRefresh(target string) {
	state := &model.waveshareLibrary
	if state.loadedTarget != target || !state.activeRefreshPending {
		return
	}
	state.activeRefreshPending = false
	model.refreshWaveshareActiveTemplate(target)
}

func (state *waveshareLibraryState) syncDurationFields() {
	defaultDuration := state.playlist.DefaultDurationSeconds
	if defaultDuration <= 0 {
		defaultDuration = config.DefaultWavesharePlaylistDurationSeconds
		state.playlist.DefaultDurationSeconds = defaultDuration
	}
	state.defaultDuration.set(strconv.Itoa(defaultDuration))
	state.durationFields = make(map[string]*textField, len(state.playlist.Entries))
	for _, entry := range state.playlist.Entries {
		state.durationField(entry.TemplateID, entry.DurationSeconds)
	}
}

func wavesharePlaylistRuntimeText(runtime WavesharePlaylistRuntime) string {
	if runtime.LastError != "" {
		message := "轮播切换失败：" + runtime.LastError
		if runtime.PendingTemplateID != "" {
			message += "；待切换 " + runtime.PendingTemplateID
		}
		if !runtime.RetryAt.IsZero() {
			message += "；" + runtime.RetryAt.Local().Format("15:04:05") + " 重试"
		}
		return message
	}
	if runtime.Running && runtime.CurrentTemplateID != "" {
		message := "正在显示 " + runtime.CurrentTemplateID
		if !runtime.NextSwitch.IsZero() {
			message += "；下次切换 " + runtime.NextSwitch.Local().Format("15:04:05")
		}
		return message
	}
	return ""
}

func (model *gioEditor) ensureUIUpdates() {
	if model.uiUpdates == nil {
		model.uiUpdates = make(chan func(), 8)
	}
}

func (model *gioEditor) invalidateWindow() {
	if model.window != nil && model.periodicUIWorkEnabled() {
		model.window.Invalidate()
	}
}

func formatStorageBytes(value uint64) string {
	const (
		kib = uint64(1024)
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case value >= gib:
		return fmt.Sprintf("%.2f GiB", float64(value)/float64(gib))
	case value >= mib:
		return fmt.Sprintf("%.1f MiB", float64(value)/float64(mib))
	case value >= kib:
		return fmt.Sprintf("%.1f KiB", float64(value)/float64(kib))
	default:
		return fmt.Sprintf("%d B", value)
	}
}
