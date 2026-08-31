package recognitionrelay

import (
	"context"
	"errors"
	"testing"

	"casescreen/internal/edgerecognition"
)

func TestStaticMapperRequiresStrongEvidenceAndExplicitAutoPolicy(t *testing.T) {
	mapper, err := NewStaticMapper(StaticMappingFile{Version: 1, Records: []StaticMappingRecord{{
		StableID: "acrcloud:recording-42", ISRC: "US-RC1-76-07839",
		QQTrackID: "0039MnYb0qxYhV", NeteaseTrackID: "347230",
	}}}, "none")
	if err != nil {
		t.Fatal(err)
	}
	result := edgerecognition.Result{
		StableID: "acrcloud:recording-42", ISRC: "USRC17607839",
		Title: "即使标题完全相同也不能作为映射键", Artist: "测试",
	}
	if _, found, err := mapper.Resolve(context.Background(), result, "auto"); err != nil || found {
		t.Fatalf("auto-platform=none 不应自行猜来源: found=%v err=%v", found, err)
	}
	identity, found, err := mapper.Resolve(context.Background(), result, "qq")
	if err != nil || !found || identity.Source != "qq" || identity.TrackID != "0039MnYb0qxYhV" {
		t.Fatalf("QQ 强映射错误: %#v found=%v err=%v", identity, found, err)
	}
	unknown := result
	unknown.StableID = "acrcloud:other-recording"
	unknown.ISRC = ""
	unknown.Title = result.Title
	if _, found, err := mapper.Resolve(context.Background(), unknown, "qq"); err != nil || found {
		t.Fatalf("只有相同标题时绝不能命中: found=%v err=%v", found, err)
	}
}

func TestStaticMapperRejectsConflictingEvidence(t *testing.T) {
	mapper, err := NewStaticMapper(StaticMappingFile{Version: 1, Records: []StaticMappingRecord{{
		StableID: "acrcloud:recording-42", ISRC: "USRC17607839", QQTrackID: "0039MnYb0qxYhV",
	}}}, "qq")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = mapper.Resolve(context.Background(), edgerecognition.Result{
		StableID: "acrcloud:recording-42", ISRC: "GBAYE6800011",
	}, "qq")
	if !errors.Is(err, ErrMappingConflict) {
		t.Fatalf("冲突 ISRC 应被拒绝: %v", err)
	}
}

func TestStaticMapperRejectsSplitRecordsMatchedByDifferentEvidence(t *testing.T) {
	mapper, err := NewStaticMapper(StaticMappingFile{Version: 1, Records: []StaticMappingRecord{
		{StableID: "acrcloud:recording-42", QQTrackID: "0039MnYb0qxYhV"},
		{ISRC: "GBAYE6800011", QQTrackID: "001z2JmX09LLgL"},
	}}, "qq")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = mapper.Resolve(context.Background(), edgerecognition.Result{
		StableID: "acrcloud:recording-42", ISRC: "GBAYE6800011",
	}, "qq")
	if !errors.Is(err, ErrMappingConflict) {
		t.Fatalf("stable_id 与 ISRC 分别命中不同记录时必须拒绝映射: %v", err)
	}
}

func TestStaticMapperValidatesPlatformNativeIDs(t *testing.T) {
	_, err := NewStaticMapper(StaticMappingFile{Version: 1, Records: []StaticMappingRecord{{
		StableID: "acrcloud:recording-42", QQTrackID: "a title guess",
	}}}, "qq")
	if err == nil {
		t.Fatal("非法 QQ songmid 不应通过")
	}
}
