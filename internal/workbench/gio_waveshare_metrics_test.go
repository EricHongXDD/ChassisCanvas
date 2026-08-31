package workbench

import "testing"

func TestWaveshareMetricChoicesExcludeUnavailableAudioStatistics(t *testing.T) {
	unavailable := map[string]bool{
		"vol": true, "cur_dB": true, "avg_dB": true, "max_dB": true,
	}
	for _, choice := range waveshareMetricChoices {
		if unavailable[choice.value] {
			t.Errorf("未接入真实采集的字段仍出现在微雪可选指标中: %s", choice.value)
		}
	}
}
