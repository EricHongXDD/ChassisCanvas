package audio

import (
	"encoding/binary"
	"fmt"
	"math"
)

// DecodeInterleavedPCM 把多声道 PCM 帧混合为 -1..1 单声道。
func DecodeInterleavedPCM(data []byte, format SampleFormat) ([]float64, error) {
	if err := format.Validate(); err != nil {
		return nil, err
	}
	if len(data)%format.BlockAlign != 0 {
		return nil, fmt.Errorf("PCM 数据 %d 字节未按 %d 字节帧对齐", len(data), format.BlockAlign)
	}
	frames := len(data) / format.BlockAlign
	output := make([]float64, frames)
	bytesPerSample := (format.BitsPerSample + 7) / 8
	for frame := 0; frame < frames; frame++ {
		base := frame * format.BlockAlign
		sum := 0.0
		for channel := 0; channel < format.Channels; channel++ {
			offset := base + channel*bytesPerSample
			// 线性等权下混不会在时域引入原信号不存在的谐波；
			// 左右反相信号按真实叠加语义相消。非法浮点先按静音处理。
			sample := clampAudio(decodeSample(data[offset:offset+bytesPerSample], format))
			sum += sample
		}
		output[frame] = clampAudio(sum / float64(format.Channels))
	}
	return output, nil
}

func decodeSample(data []byte, format SampleFormat) float64 {
	switch format.Encoding {
	case PCMUnsigned:
		return (float64(data[0]) - 128) / 128
	case PCMSigned:
		switch format.BitsPerSample {
		case 16:
			return float64(int16(binary.LittleEndian.Uint16(data))) / 32768
		case 24:
			value := int32(data[0]) | int32(data[1])<<8 | int32(data[2])<<16
			if value&0x800000 != 0 {
				value |= ^int32(0xFFFFFF)
			}
			return float64(value) / 8388608
		case 32:
			return float64(int32(binary.LittleEndian.Uint32(data))) / 2147483648
		}
	case PCMFloat:
		if format.BitsPerSample == 32 {
			return float64(math.Float32frombits(binary.LittleEndian.Uint32(data)))
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(data))
	}
	return 0
}

func clampAudio(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Max(-1, math.Min(1, value))
}
