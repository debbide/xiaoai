package music

import (
	"errors"
	"io"
	"os"
	"time"
)

// errNoMP3Frame 表示没能在文件头部找到合法的 MPEG 音频帧，probeMP3Duration 会返回它。
var errNoMP3Frame = errors.New("no valid mp3 frame header found")

// MPEG 音频帧头比特率表（kbps），按 [MPEG 版本][Layer] 区分。
// 参考 ISO/IEC 11172-3 附录 B.1。
var (
	mp3BitrateV1L1  = []int{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448}
	mp3BitrateV1L2  = []int{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384}
	mp3BitrateV1L3  = []int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	mp3BitrateV2L1  = []int{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256}
	mp3BitrateV2L23 = []int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
)

// mp3FrameInfo 从 MP3 文件中解析出的首个有效帧信息。
type mp3FrameInfo struct {
	BitrateKbps int
	SampleRate  int
}

// parseMP3FrameHeader 解析 4 字节 MPEG 音频帧头（已确认前两字节是合法帧同步）。
// 返回该帧的真实编码比特率/采样率；ok=false 表示这不是一个合法帧头（继续找下一个字节）。
func parseMP3FrameHeader(b []byte) (mp3FrameInfo, bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return mp3FrameInfo{}, false
	}
	versionBits := (b[1] >> 3) & 0x03 // 00=MPEG2.5 01=保留 10=MPEG2 11=MPEG1
	layerBits := (b[1] >> 1) & 0x03   // 00=保留 01=Layer III 10=Layer II 11=Layer I
	if versionBits == 0x01 || layerBits == 0x00 {
		return mp3FrameInfo{}, false
	}
	bitrateIdx := int((b[2] >> 4) & 0x0F)
	sampleRateIdx := int((b[2] >> 2) & 0x03)
	// 0000=自由比特率（不支持，跳过）；1111=非法值；sampleRateIdx==3 为保留值。
	if bitrateIdx == 0 || bitrateIdx == 0x0F || sampleRateIdx == 3 {
		return mp3FrameInfo{}, false
	}

	isV1 := versionBits == 0x03
	var bitrateTable []int
	switch {
	case isV1 && layerBits == 0x03: // Layer I
		bitrateTable = mp3BitrateV1L1
	case isV1 && layerBits == 0x02: // Layer II
		bitrateTable = mp3BitrateV1L2
	case isV1: // Layer III
		bitrateTable = mp3BitrateV1L3
	case layerBits == 0x03: // MPEG2/2.5 Layer I
		bitrateTable = mp3BitrateV2L1
	default: // MPEG2/2.5 Layer II/III
		bitrateTable = mp3BitrateV2L23
	}
	if bitrateIdx >= len(bitrateTable) || bitrateTable[bitrateIdx] == 0 {
		return mp3FrameInfo{}, false
	}

	var sampleRate int
	switch versionBits {
	case 0x03: // MPEG1
		sampleRate = []int{44100, 48000, 32000}[sampleRateIdx]
	case 0x02: // MPEG2
		sampleRate = []int{22050, 24000, 16000}[sampleRateIdx]
	default: // MPEG2.5
		sampleRate = []int{11025, 12000, 8000}[sampleRateIdx]
	}

	return mp3FrameInfo{BitrateKbps: bitrateTable[bitrateIdx], SampleRate: sampleRate}, true
}

// mp3ID3v2Size 如果文件开头是 ID3v2 头，返回其总大小（含 10 字节头本身），否则返回 0。
// ID3v2 size 字段是 synchsafe 编码：4 个字节，每字节只用低 7 位。
func mp3ID3v2Size(b []byte) int64 {
	if len(b) < 10 || b[0] != 'I' || b[1] != 'D' || b[2] != '3' {
		return 0
	}
	return int64(b[6]&0x7F)<<21 | int64(b[7]&0x7F)<<14 | int64(b[8]&0x7F)<<7 | int64(b[9]&0x7F) + 10
}

// mp3ProbeReadBytes 读取文件头部用来查找第一个有效帧的字节数。
// 64KB 足够跳过常见 ID3v2 头（哪怕带较大的封面图）并找到音频帧。
const mp3ProbeReadBytes = 64 * 1024

// probeMP3Duration 读取文件头部，解析出真实编码的比特率/采样率，按 CBR 假设估算整个文件的
// 播放时长：duration = (文件总字节数 - 帧起始偏移) * 8 / 比特率。
//
// 为什么不猜一个固定比特率：之前的实现假设所有文件都是 128kbps，但实际有声书/评书类
// mp3 常见编码在 64kbps 甚至更低，用 128kbps 估算会把真实时长砍掉一半，导致看门狗在
// 歌曲远没播完时就误判"早该播完了"提前切歌。读取真实帧头里编码的比特率能从根本上避免
// 这个问题——这是数据来源，不是猜测。
//
// 对绝大多数"批量转码的有声书/播客 mp3"（几乎都是 CBR）这个估算已经足够准确；
// 对真正的 VBR 文件会有一定偏差，但不会离谱到几倍（因为大部分帧的比特率不会跟第一帧
// 差太多），调用方也应该在看门狗上留出合理缓冲，不要指望这个值精确到秒。
//
// 解析失败（不是合法 mp3、文件太小、读取出错）返回 0 和对应 error，调用方应该跳过
// 基于时长的判断，不要因为探测失败就影响正常播放。
func probeMP3Duration(path string, fileSize int64) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	buf := make([]byte, mp3ProbeReadBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, err
	}
	buf = buf[:n]
	if len(buf) < 10 {
		return 0, errNoMP3Frame
	}

	start := int(mp3ID3v2Size(buf))
	if start < 0 || start >= len(buf) {
		return 0, errNoMP3Frame
	}

	for i := start; i+4 <= len(buf); i++ {
		if buf[i] != 0xFF || buf[i+1]&0xE0 != 0xE0 {
			continue
		}
		info, ok := parseMP3FrameHeader(buf[i : i+4])
		if !ok {
			continue
		}
		audioBytes := fileSize - int64(i)
		if audioBytes <= 0 || info.BitrateKbps <= 0 {
			return 0, errNoMP3Frame
		}
		seconds := float64(audioBytes*8) / (float64(info.BitrateKbps) * 1000)
		return time.Duration(seconds * float64(time.Second)), nil
	}
	return 0, errNoMP3Frame
}
