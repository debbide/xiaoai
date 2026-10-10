package music

import "testing"

func TestNormalizeTrimsChinesePunctuation(t *testing.T) {
	cases := map[string]string{
		"，停止播放":   "停止播放",
		"播放：晴天":   "播放：晴天",
		"播放！":     "播放",
		"  播放,晴天": "播放,晴天",
		"播放？":     "播放",
		"。停止。":    "停止",
		"\t停止\n":  "停止",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizedForMatchStripsSpaces(t *testing.T) {
	if got := NormalizedForMatch("  下 一首 "); got != "下一首" {
		t.Errorf("NormalizedForMatch unexpected: %q", got)
	}
}

func TestParsePlayIntentStripsSongResourcePrefix(t *testing.T) {
	intent := ParsePlayIntent("歌曲周杰伦晴天")

	if intent.SeriesName != "周杰伦晴天" {
		t.Fatalf("expected query without resource prefix, got %q", intent.SeriesName)
	}
	if intent.Episode != 0 {
		t.Fatalf("expected no episode, got %d", intent.Episode)
	}
}

func TestParsePlayIntentKeepsEpisodeAfterResourcePrefix(t *testing.T) {
	intent := ParsePlayIntent("故事西游记第11集")

	if intent.SeriesName != "西游记" {
		t.Fatalf("expected series name without resource prefix, got %q", intent.SeriesName)
	}
	if intent.Episode != 11 {
		t.Fatalf("expected episode 11, got %d", intent.Episode)
	}
}

func TestParsePlayIntentDoesNotEatNakedTrailingNumber(t *testing.T) {
	// 没有显式"集/回"后缀时，数字保留在 SeriesName 里，不当作集数。
	cases := []struct {
		in     string
		series string
		ep     int
	}{
		{"周杰伦88", "周杰伦88", 0},
		{"5566", "5566", 0},
		{"1995年", "1995年", 0},
		{"播放周杰伦88", "播放周杰伦88", 0},
		{"西游记11集", "西游记", 11},
		{"水浒传第5集", "水浒传", 5},
		{"西游记第20回", "西游记", 20},
	}
	for _, c := range cases {
		got := ParsePlayIntent(c.in)
		if got.SeriesName != c.series || got.Episode != c.ep {
			t.Errorf("ParsePlayIntent(%q) = {%q, %d}, want {%q, %d}",
				c.in, got.SeriesName, got.Episode, c.series, c.ep)
		}
	}
}

func TestParsePlayIntentMarksStoryPrefix(t *testing.T) {
	cases := []struct {
		in      string
		series  string
		ep      int
		isStory bool
	}{
		{"故事西游记第11集", "西游记", 11, true},
		{"有声书三国演义", "三国演义", 0, true},
		{"故事三国第一季第48集", "三国第一季", 48, true},
		{"歌曲周杰伦晴天", "周杰伦晴天", 0, false},
		{"周杰伦晴天", "周杰伦晴天", 0, false},
	}
	for _, c := range cases {
		got := ParsePlayIntent(c.in)
		if got.SeriesName != c.series || got.Episode != c.ep || got.IsStory != c.isStory {
			t.Errorf("ParsePlayIntent(%q) = {%q, %d, isStory=%v}, want {%q, %d, isStory=%v}",
				c.in, got.SeriesName, got.Episode, got.IsStory, c.series, c.ep, c.isStory)
		}
	}
}

func TestParsePlayIntentSupportsChineseEpisodeNumerals(t *testing.T) {
	// 小爱 ASR 对个位数经常转写成中文数字（"第六集"）而不是"第6集"。
	cases := []struct {
		in     string
		series string
		ep     int
	}{
		{"故事三国第六集", "三国", 6},
		{"故事三国第二十三集", "三国", 23},
		{"故事三国第一百集", "三国", 100},
		{"故事三国第一百二十九集", "三国", 129},
		{"水浒传第五回", "水浒传", 5},
		{"西游记第十回", "西游记", 10},
	}
	for _, c := range cases {
		got := ParsePlayIntent(c.in)
		if got.SeriesName != c.series || got.Episode != c.ep {
			t.Errorf("ParsePlayIntent(%q) = {%q, %d}, want {%q, %d}",
				c.in, got.SeriesName, got.Episode, c.series, c.ep)
		}
	}
}

func TestParsePlayIntentEpisodeOnlyReturnsEmptySeries(t *testing.T) {
	got := ParsePlayIntent("第4集")
	if got.SeriesName != "" || got.Episode != 4 {
		t.Fatalf("ParsePlayIntent(第4集) = {%q, %d}, want {\"\", 4}", got.SeriesName, got.Episode)
	}
}

func TestParseStoryContextCommand(t *testing.T) {
	cases := []struct {
		in   string
		want StoryContextCommand
	}{
		{"默认故事是三国演义第一季", StoryContextCommand{Set: true, SeriesName: "三国演义第一季"}},
		{"把默认故事设为三国第一季", StoryContextCommand{Set: true, SeriesName: "三国第一季"}},
		{"当前默认故事是什么", StoryContextCommand{Query: true}},
		{"清除默认故事", StoryContextCommand{Clear: true}},
	}
	for _, c := range cases {
		got := ParseStoryContextCommand(c.in)
		if got != c.want {
			t.Fatalf("ParseStoryContextCommand(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestChineseNumeralToInt(t *testing.T) {
	cases := map[string]int{
		"零":     0,
		"六":     6,
		"十":     10,
		"十五":    15,
		"二十":    20,
		"二十三":   23,
		"一百":    100,
		"一百零五":  105,
		"一百二十九": 129,
		"九百九十九": 999,
	}
	for in, want := range cases {
		got, ok := chineseNumeralToInt(in)
		if !ok {
			t.Errorf("chineseNumeralToInt(%q) failed to parse", in)
			continue
		}
		if got != want {
			t.Errorf("chineseNumeralToInt(%q) = %d, want %d", in, got, want)
		}
	}
}
