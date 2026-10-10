package music

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildMP3FrameHeader 构造一个合法的 MPEG1 Layer III 帧头（4 字节），指定比特率和采样率。
// bitrateIdx / sampleRateIdx 按标准表（见 mp3duration.go 里的表）传对应下标。
func buildMP3FrameHeader(bitrateIdx, sampleRateIdx byte) []byte {
	b := make([]byte, 4)
	b[0] = 0xFF
	// MPEG1 (11) + Layer III (01) + 无 CRC 保护 (1)
	b[1] = 0xE0 | (0x03 << 3) | (0x01 << 1) | 0x01
	b[2] = (bitrateIdx << 4) | (sampleRateIdx << 2)
	b[3] = 0xC4 // 双声道随便填一个合法值，不影响我们解析的字段
	return b
}

func TestParseMP3FrameHeaderDecodesBitrateAndSampleRate(t *testing.T) {
	// MPEG1 Layer III 比特率表下标 9 = 128kbps；采样率下标 0 = 44100Hz
	header := buildMP3FrameHeader(9, 0)
	info, ok := parseMP3FrameHeader(header)
	if !ok {
		t.Fatal("expected valid frame header to parse")
	}
	if info.BitrateKbps != 128 {
		t.Fatalf("expected 128kbps, got %d", info.BitrateKbps)
	}
	if info.SampleRate != 44100 {
		t.Fatalf("expected 44100Hz, got %d", info.SampleRate)
	}
}

func TestParseMP3FrameHeaderDecodesLowerBitrate(t *testing.T) {
	// mp3BitrateV1L3 下标 5 = 64kbps —— 这正是导致真实 bug 的场景：有声书常见的低比特率
	// 编码，之前固定假设 128kbps 会把真实时长砍掉一半。
	header := buildMP3FrameHeader(5, 0)
	info, ok := parseMP3FrameHeader(header)
	if !ok {
		t.Fatal("expected valid frame header to parse")
	}
	if info.BitrateKbps != 64 {
		t.Fatalf("expected 64kbps, got %d", info.BitrateKbps)
	}
}

func TestParseMP3FrameHeaderRejectsInvalidSync(t *testing.T) {
	if _, ok := parseMP3FrameHeader([]byte{0x00, 0x00, 0x00, 0x00}); ok {
		t.Fatal("expected invalid sync to be rejected")
	}
}

func TestMP3ID3v2SizeDecodesSynchsafeInteger(t *testing.T) {
	// ID3v2 header: "ID3" + version(2) + flags(1) + synchsafe size(4)
	// size = 0x7F 0x7F 0x7F 0x7F 表示每字节 0x7F，即 (0x7F<<21)|(0x7F<<14)|(0x7F<<7)|0x7F = 0x0FFFFFFF
	b := []byte{'I', 'D', '3', 3, 0, 0, 0x00, 0x00, 0x02, 0x00} // size synchsafe = 0x100 = 256
	got := mp3ID3v2Size(b)
	want := int64(256 + 10)
	if got != want {
		t.Fatalf("expected id3v2 size %d, got %d", want, got)
	}
}

func TestMP3ID3v2SizeReturnsZeroWithoutID3Tag(t *testing.T) {
	b := []byte{0xFF, 0xFB, 0x90, 0xC4, 0, 0, 0, 0, 0, 0}
	if got := mp3ID3v2Size(b); got != 0 {
		t.Fatalf("expected 0 for non-ID3 file, got %d", got)
	}
}

// TestProbeMP3DurationUsesRealBitrateNotFixedAssumption 端到端回归覆盖真实事故：
// 构造一个 64kbps 的"假 mp3"（帧头 + 填充数据），确认算出来的时长约等于用 64kbps 算出的值，
// 而不是之前固定假设 128kbps 算出的（会短一半）。
func TestProbeMP3DurationUsesRealBitrateNotFixedAssumption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.mp3")

	header := buildMP3FrameHeader(5, 0) // 64kbps, 44100Hz
	// 64kbps = 8000 字节/秒，填充出约 30 秒的数据量：8000*30 = 240000 字节
	body := make([]byte, 240_000)
	data := append(append([]byte{}, header...), body...)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write test mp3: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	dur, err := probeMP3Duration(path, info.Size())
	if err != nil {
		t.Fatalf("probeMP3Duration failed: %v", err)
	}

	// 期望约 30 秒（用真实 64kbps 计算）。如果仍然固定假设 128kbps，会算出约 15 秒——
	// 明显偏小，验证了这次修复确实在用真实比特率而不是猜测值。
	if dur < 28*time.Second || dur > 32*time.Second {
		t.Fatalf("expected duration close to 30s (using real 64kbps), got %v (bug: would be ~15s under old 128kbps assumption)", dur)
	}
}

func TestProbeMP3DurationSkipsID3v2Header(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "with_id3.mp3")

	// 构造一个 100 字节的 ID3v2 头（含 synchsafe size = 90，加上 10 字节头本身 = 100）
	id3 := make([]byte, 100)
	id3[0], id3[1], id3[2] = 'I', 'D', '3'
	id3[3], id3[4], id3[5] = 3, 0, 0
	// synchsafe(90) = 0,0,0,90 (90 < 128，最高位不需要进位)
	id3[6], id3[7], id3[8], id3[9] = 0, 0, 0, 90

	header := buildMP3FrameHeader(9, 0) // 128kbps
	body := make([]byte, 128_000)       // 128kbps = 16000B/s，8秒数据量
	data := append(append(append([]byte{}, id3...), header...), body...)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write test mp3: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	dur, err := probeMP3Duration(path, info.Size())
	if err != nil {
		t.Fatalf("probeMP3Duration failed: %v", err)
	}
	if dur < 7*time.Second || dur > 9*time.Second {
		t.Fatalf("expected duration close to 8s, got %v (ID3v2 header may not have been skipped correctly)", dur)
	}
}

func TestProbeMP3DurationReturnsErrorForNonMP3(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not_mp3.mp3")
	if err := os.WriteFile(path, []byte("this is not an mp3 file at all, just plain text padding to be long enough"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, _ := os.Stat(path)
	if _, err := probeMP3Duration(path, info.Size()); err == nil {
		t.Fatal("expected error for non-mp3 content")
	}
}
