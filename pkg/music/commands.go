package music

import (
	"bytes"
	"encoding/json"
	"log"
	"regexp"
	"strconv"
	"strings"
)

// trimPunctuationCutset 首尾要去除的标点集合。
// 注意：strings.Trim 是按 rune 比对的，所以中英文（多字节）标点都能命中。
// 之前的实现用 `rune(s[0]) == p`，对中文标点（UTF-8 3 字节）永远不匹配，导致
// `Normalize("，停止")` 仍然返回 "，停止"，命令命中率受影响。
const trimPunctuationCutset = "：:，,。！？!? \t\r\n"

// Normalize 文本规范化：去除首尾空白与中英文标点。
func Normalize(s string) string {
	return strings.Trim(s, trimPunctuationCutset)
}

// NormalizedForMatch 用于匹配的规范化：去除空格
func NormalizedForMatch(s string) string {
	return strings.ReplaceAll(Normalize(s), " ", "")
}

// instructionEventData 兼容 Go client {Type:"NewLine", Line:"..."} 与 Rust client {NewLine:"..."}
type instructionEventData struct {
	Type    string `json:"Type"`
	Line    string `json:"Line"`
	NewLine string `json:"NewLine"`
}

// instructionLogLine instruction.log 单行结构
type instructionLogLine struct {
	Header struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"header"`
	Payload struct {
		IsFinal bool `json:"is_final"`
		Results []struct {
			Text string `json:"text"`
		} `json:"results"`
	} `json:"payload"`
}

// ParseInstructionUserText 从 instruction 事件中提取用户最终语音文本
// 兼容三种事件载荷：
//   - Go client 对象： {"Type":"NewLine","Line":"..."} / {"Type":"NewFile"}
//   - Rust client 对象： {"NewLine":"..."}
//   - Rust client 字符串字面量： "NewFile"（serde externally-tagged 的 unit 变体）
func ParseInstructionUserText(data *json.RawMessage) string {
	if data == nil {
		return ""
	}
	raw := bytes.TrimSpace(*data)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	// Rust client 的 unit 变体（如 "NewFile"）会序列化为 JSON 字符串，
	// 不是 NewLine，直接跳过，不报警。
	if raw[0] == '"' {
		return ""
	}
	var ev instructionEventData
	if err := json.Unmarshal(raw, &ev); err != nil {
		log.Printf("⚠️ [music/parse] instruction event 外层 JSON 解析失败: %v (data=%s)", err, string(raw))
		return ""
	}
	line := ev.Line
	if line == "" {
		line = ev.NewLine
	}
	if line == "" {
		// NewFile 事件等非 NewLine 类型，正常跳过，不打 log
		return ""
	}
	var msg instructionLogLine
	if err := json.NewDecoder(strings.NewReader(line)).Decode(&msg); err != nil {
		log.Printf("⚠️ [music/parse] instruction line JSON 解析失败: %v (line=%q)", err, line)
		return ""
	}
	if !strings.EqualFold(msg.Header.Namespace, "SpeechRecognizer") ||
		!strings.EqualFold(msg.Header.Name, "RecognizeResult") {
		// 非语音识别结果（如 NLP 等），跳过不报警
		return ""
	}
	if !msg.Payload.IsFinal || len(msg.Payload.Results) == 0 {
		// 中间结果，跳过不报警
		return ""
	}
	return strings.TrimSpace(msg.Payload.Results[0].Text)
}

// PlayIntent 播放意图：系列名 + 集数（0 表示未指定或从第 1 集开始）
type PlayIntent struct {
	SeriesName string // 系列名/关键词，如「西游记」「许嵩」
	Episode    int    // 集数，0 表示未指定
	IsStory    bool   // 用户是否显式说了"故事"/"有声书"类资源词（如"播放故事西游记"）
	// 为 true 时强制走 SearchEpisode（按集排序）分支，不再依赖 stories 配置里
	// 系列名/别名的精确匹配，从而支持"播放故事"、"播放有声书"这类独立触发词，
	// 避免和普通"播放音乐"混在一起。
}

// StoryContextCommand 默认故事上下文命令。
// 用于"默认故事是三国演义第一季"、"默认故事是什么"等口令。
type StoryContextCommand struct {
	Set        bool
	Clear      bool
	Query      bool
	SeriesName string
}

// setDefaultStoryRegex 解析"设置默认故事"类命令（基于去空格后的文本匹配）。
// 支持示例：
// - 默认故事是三国演义第一季
// - 默认故事设为三国演义第一季
// - 设置默认故事三国演义第一季
// - 把默认故事设为三国演义第一季
var setDefaultStoryRegex = regexp.MustCompile(`^(?:设置)?默认故事(?:是|为|设为)?(.+)$|^把默认故事设为(.+)$`)

var queryDefaultStoryKeywords = []string{"默认故事是什么", "当前默认故事是什么", "默认故事", "当前默认故事"}
var clearDefaultStoryKeywords = []string{"清除默认故事", "取消默认故事", "删除默认故事", "重置默认故事"}

// episodeRegex 仅匹配带显式集数标记（集/回）的表达：第11集、11集、第11回、水浒传第20集、
// 第六集、第二十三集 等。集数部分同时兼容阿拉伯数字与中文数字——小爱 ASR 对个位数经常
// 转写成中文数字（"第六集"）而不是"第6集"，之前只认 \d+ 会导致这类指令 episode 恒为 0。
//
// 这里有意去掉了"裸数字"的兼容。之前的正则把 `集|回` 设成可选，会把"播放周杰伦88"
// 解析为 series="周杰伦" episode=88，触发 SearchEpisode 走故事检索分支，普通歌手名带
// 数字的关键词全部失配。如果用户明确想播某集，加上"集"/"回"即可。
var episodeRegex = regexp.MustCompile(`(?:第)?([0-9零〇一二三四五六七八九十百千两]+)[集回]`)

// ParsePlayIntent 从播放关键词中解析系列名和集数
// 例如：「西游记11集」-> {SeriesName:"西游记", Episode:11}
//
//	「水浒传第5集」-> {SeriesName:"水浒传", Episode:5}
//	「许嵩」-> {SeriesName:"许嵩", Episode:0}
//	「故事三国第一季第48集」-> {SeriesName:"三国第一季", Episode:48, IsStory:true}
func ParsePlayIntent(keyword string) PlayIntent {
	cleaned, isStory := NormalizePlayKeyword(keyword)
	norm := NormalizedForMatch(cleaned)
	if norm == "" {
		return PlayIntent{IsStory: isStory}
	}
	locs := episodeRegex.FindAllStringSubmatchIndex(norm, -1)
	if len(locs) == 0 {
		return PlayIntent{SeriesName: cleaned, Episode: 0, IsStory: isStory}
	}
	last := locs[len(locs)-1]
	epNum := parseEpisodeNumber(norm[last[2]:last[3]])
	seriesPart := strings.TrimSpace(norm[:last[0]])
	return PlayIntent{SeriesName: seriesPart, Episode: epNum, IsStory: isStory}
}

// ParseStoryContextCommand 解析默认故事上下文相关口令。
func ParseStoryContextCommand(text string) StoryContextCommand {
	norm := NormalizedForMatch(text)
	if norm == "" {
		return StoryContextCommand{}
	}
	for _, kw := range clearDefaultStoryKeywords {
		if norm == NormalizedForMatch(kw) {
			return StoryContextCommand{Clear: true}
		}
	}
	for _, kw := range queryDefaultStoryKeywords {
		if norm == NormalizedForMatch(kw) {
			return StoryContextCommand{Query: true}
		}
	}
	m := setDefaultStoryRegex.FindStringSubmatch(norm)
	if len(m) == 0 {
		return StoryContextCommand{}
	}
	series := ""
	if len(m) > 1 {
		series = m[1]
	}
	if series == "" && len(m) > 2 {
		series = m[2]
	}
	series = Normalize(series)
	if series == "" {
		return StoryContextCommand{}
	}
	return StoryContextCommand{Set: true, SeriesName: series}
}

// storyResourcePrefixes 故事类资源词：命中后 IsStory=true，强制走"按集搜索"分支，
// 不再要求 stories 配置里系列名/别名精确匹配才能识别成故事。
// 这样"播放故事xxx"、"播放有声书xxx"就等效于独立的故事触发词，和"播放音乐/播放歌曲"区分开，
// 两者不会因为都以"播放"开头而混在一起。
var storyResourcePrefixes = []string{"故事", "有声书"}

// musicResourcePrefixes 普通音乐资源词：仅用于从关键词里剥离"音乐/歌曲"之类的前缀，不影响 IsStory。
var musicResourcePrefixes = []string{"本地歌曲", "本地音乐", "歌曲", "音乐"}

// NormalizePlayKeyword 剥离"播放"之后关键词里的资源类型前缀词（故事/有声书/音乐/歌曲等），
// 返回清理后的关键词，以及是否命中了故事类前缀。
func NormalizePlayKeyword(keyword string) (string, bool) {
	keyword = Normalize(keyword)
	norm := NormalizedForMatch(keyword)
	for _, prefix := range storyResourcePrefixes {
		prefixNorm := NormalizedForMatch(prefix)
		if prefixNorm != "" && strings.HasPrefix(norm, prefixNorm) {
			return Normalize(strings.TrimSpace(keyword[len(prefix):])), true
		}
	}
	for _, prefix := range musicResourcePrefixes {
		prefixNorm := NormalizedForMatch(prefix)
		if prefixNorm != "" && strings.HasPrefix(norm, prefixNorm) {
			return Normalize(strings.TrimSpace(keyword[len(prefix):])), false
		}
	}
	return keyword, false
}

func parseInt(s string) int {
	var n int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

// parseEpisodeNumber 解析 episodeRegex 捕获组里的集数：优先按阿拉伯数字解析，
// 失败（含中文数字字符）则按中文数字解析。
func parseEpisodeNumber(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if n, ok := chineseNumeralToInt(s); ok {
		return n
	}
	return 0
}

// chineseDigits / chineseUnits 中文数字转阿拉伯数字用的映射表。
// 支持到"千"位（0~9999），足够覆盖有声书/评书常见的几百集规模（如"第七百二十九集"）。
var chineseDigits = map[rune]int{
	'零': 0, '〇': 0,
	'一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
	'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
}

var chineseUnits = map[rune]int{
	'十': 10, '百': 100, '千': 1000,
}

// chineseNumeralToInt 把中文数字（如"六"、"二十三"、"一百二十"）转换为阿拉伯数字。
// 只处理 0~9999 范围内的组合，够用于集数解析；不支持"万"及以上，也不做复杂的
// 传统读法校验（如"一百零五" vs "一百五"两种写法都能正确解析）。
func chineseNumeralToInt(s string) (int, bool) {
	total := 0
	section := 0
	num := 0
	seenDigit := false
	for _, r := range s {
		if d, ok := chineseDigits[r]; ok {
			num = d
			seenDigit = true
			continue
		}
		if u, ok := chineseUnits[r]; ok {
			if num == 0 {
				num = 1 // "十五" -> 十 单独出现表示 1*10
			}
			section += num * u
			num = 0
			seenDigit = true
			continue
		}
		// 出现无法识别的字符，说明这不是一个纯中文数字串
		return 0, false
	}
	total += section + num
	if !seenDigit {
		return 0, false
	}
	return total, true
}
