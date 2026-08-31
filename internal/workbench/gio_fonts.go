package workbench

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/font/opentype"
	"gioui.org/text"
	"gioui.org/widget/material"
)

var (
	workbenchFontsOnce sync.Once
	workbenchFonts     []font.FontFace
)

const (
	workbenchTypefacePangMen        font.Typeface = "CaseScreen PangMen"
	workbenchTypefaceSourceHan      font.Typeface = "CaseScreen Source Han"
	workbenchTypefaceMicrosoftYaHei font.Typeface = "CaseScreen Microsoft YaHei"
	workbenchTypefaceSimHei         font.Typeface = "CaseScreen SimHei"
	workbenchTypefaceDigital        font.Typeface = "CaseScreen Digital"
	workbenchTypefaceMonospace      font.Typeface = "CaseScreen Monospace"
	workbenchTypefaceRounded        font.Typeface = "CaseScreen Rounded"
	workbenchTypefaceSerif          font.Typeface = "CaseScreen Serif"
)

// newWorkbenchTheme 使用有界字体白名单，避免 Gio 在首帧同步扫描整个
// Windows 字体目录。系统字体较多时，全量索引会让窗口长时间停在白屏。
func newWorkbenchTheme() *material.Theme {
	theme := material.NewTheme()
	theme.Shaper = text.NewShaper(
		text.NoSystemFonts(),
		text.WithCollection(workbenchFontCollection()),
	)
	return theme
}

// workbenchFontCollection 为每个逻辑字体族显式注册四档字重，并保留内置
// Go 字体作为后备。逻辑字体族使用 CaseScreen 独立别名，避免 TTC 元数据
// 不同导致所有按钮回退到同一张字脸。相同文件只解析一次，不扫描整个目录。
func workbenchFontCollection() []font.FontFace {
	workbenchFontsOnce.Do(func() {
		fontDirectory := ""
		if runtime.GOOS == "windows" {
			fontDirectory = filepath.Join(os.Getenv("WINDIR"), "Fonts")
		}
		workbenchFonts = buildWorkbenchFontCollection(fontDirectory)
	})
	return workbenchFonts
}

func buildWorkbenchFontCollection(fontDirectory string) []font.FontFace {
	collection := make([]font.FontFace, 0, len(gofont.Collection())+40)
	if fontDirectory != "" {
		cache := make(map[string]font.Face)
		add := func(typeface font.Typeface, weight font.Weight, names ...string) {
			collection = appendWorkbenchFontWeight(collection, fontDirectory, cache, typeface, weight, names...)
		}
		add(workbenchTypefacePangMen, font.Light, "Dengl.ttf", "Deng.ttf", "msyhl.ttc", "msyh.ttc")
		add(workbenchTypefacePangMen, font.Normal, "Deng.ttf", "msyh.ttc")
		add(workbenchTypefacePangMen, font.Medium, "Dengb.ttf", "Deng.ttf", "msyhbd.ttc", "msyh.ttc")
		add(workbenchTypefacePangMen, font.Bold, "Dengb.ttf", "msyhbd.ttc")
		// Source Han 逻辑族不得回退到微软雅黑，否则切换字体族时
		// Light/Regular 会与 Microsoft YaHei 完全相同。Windows 若没有
		// Noto Sans SC，使用系统自带的黑体/等线作为形态明确不同的预览后备。
		add(workbenchTypefaceSourceHan, font.Light, "NotoSansSC-VF.ttf", "simhei.ttf", "Deng.ttf", "simsun.ttc")
		add(workbenchTypefaceSourceHan, font.Normal, "NotoSansSC-VF.ttf", "simhei.ttf", "Deng.ttf", "simsun.ttc")
		add(workbenchTypefaceSourceHan, font.Medium, "NotoSansSC-VF.ttf", "simhei.ttf", "Deng.ttf", "simsunb.ttf", "simsun.ttc")
		add(workbenchTypefaceSourceHan, font.Bold, "NotoSansSC-VF.ttf", "simhei.ttf", "Deng.ttf", "simsunb.ttf", "simsun.ttc")
		add(workbenchTypefaceMicrosoftYaHei, font.Light, "msyhl.ttc", "msyh.ttc")
		add(workbenchTypefaceMicrosoftYaHei, font.Normal, "msyh.ttc")
		add(workbenchTypefaceMicrosoftYaHei, font.Medium, "msyhbd.ttc", "msyh.ttc")
		add(workbenchTypefaceMicrosoftYaHei, font.Bold, "msyhbd.ttc")
		add(workbenchTypefaceSimHei, font.Light, "simhei.ttf", "msyhl.ttc")
		add(workbenchTypefaceSimHei, font.Normal, "simhei.ttf", "msyh.ttc")
		add(workbenchTypefaceSimHei, font.Medium, "simhei.ttf", "msyhbd.ttc")
		add(workbenchTypefaceSimHei, font.Bold, "simhei.ttf", "msyhbd.ttc")
		add(workbenchTypefaceDigital, font.Light, "LiquidCrystal-Light.otf", "consola.ttf")
		add(workbenchTypefaceDigital, font.Normal, "LiquidCrystal-Normal.otf", "consola.ttf")
		add(workbenchTypefaceDigital, font.Medium, "LiquidCrystal-Bold.otf", "consolab.ttf")
		add(workbenchTypefaceDigital, font.Bold, "LiquidCrystal-ExBold.otf", "consolab.ttf")
		add(workbenchTypefaceMonospace, font.Light, "CascadiaMono.ttf", "consola.ttf")
		add(workbenchTypefaceMonospace, font.Normal, "consola.ttf")
		add(workbenchTypefaceMonospace, font.Medium, "consolab.ttf", "consola.ttf")
		add(workbenchTypefaceMonospace, font.Bold, "consolab.ttf")
		add(workbenchTypefaceRounded, font.Light, "YuGothL.ttc", "msyhl.ttc")
		add(workbenchTypefaceRounded, font.Normal, "YuGothR.ttc", "msyh.ttc")
		add(workbenchTypefaceRounded, font.Medium, "YuGothM.ttc", "msyhbd.ttc")
		add(workbenchTypefaceRounded, font.Bold, "YuGothB.ttc", "msyhbd.ttc")
		add(workbenchTypefaceSerif, font.Light, "NotoSerifSC-VF.ttf", "simsun.ttc")
		add(workbenchTypefaceSerif, font.Normal, "simsun.ttc", "NotoSerifSC-VF.ttf")
		add(workbenchTypefaceSerif, font.Medium, "simsunb.ttf", "NotoSerifSC-VF.ttf")
		add(workbenchTypefaceSerif, font.Bold, "simsunb.ttf")
	}
	collection = append(collection, gofont.Collection()...)
	return collection[:len(collection):len(collection)]
}

func appendWorkbenchFontWeight(collection []font.FontFace, directory string, cache map[string]font.Face, typeface font.Typeface, weight font.Weight, names ...string) []font.FontFace {
	if directory == "" {
		return collection
	}
	for _, name := range names {
		path := filepath.Join(directory, name)
		face := cache[path]
		if face == nil {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			faces, err := opentype.ParseCollection(data)
			if err != nil || len(faces) == 0 {
				continue
			}
			face = faces[0].Face
			cache[path] = face
		}
		return append(collection, font.FontFace{Font: font.Font{Typeface: typeface, Weight: weight}, Face: face})
	}
	return collection
}
