package recognitionrelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"casescreen/internal/edgerecognition"
)

// DeterministicProvider 是端到端测试专用提供方。它不会联网，也不会冒充真实识歌。
// 同一输入字节得到同一 stable_id；不同采样内容应被视为不同测试录音。
type DeterministicProvider struct{}

func (DeterministicProvider) Recognize(_ context.Context, submission edgerecognition.Submission) (edgerecognition.Response, error) {
	data := submission.PCM
	if submission.Fingerprint != nil {
		data = submission.Fingerprint.Data
	}
	if len(data) == 0 || submission.SampledAt.IsZero() || submission.Duration <= 0 {
		return edgerecognition.Response{}, errors.New("deterministic provider 收到无效测试采样")
	}
	digest := sha256.Sum256(data)
	stableID := "deterministic:sha256:" + hex.EncodeToString(digest[:])
	offset := submission.Duration
	if offset > 5*time.Minute {
		offset = 5 * time.Minute
	}
	return edgerecognition.Response{
		Matched: true,
		Result: edgerecognition.Result{
			StableID: stableID, Title: "Deterministic test sample", Artist: "CaseScreen",
			Album: "Relay self-test", Duration: 5 * time.Minute,
			PlaybackOffset: offset, Confidence: 1, SampledAt: submission.SampledAt,
		},
	}, nil
}
