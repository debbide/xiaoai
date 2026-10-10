package music

import (
	"strconv"
	"testing"
)

func TestSearchRanksCombinedArtistAndTitleAboveLoosePathMatch(t *testing.T) {
	idx := &Indexer{
		config: &MusicConfig{Search: SearchConfig{MaxResults: 10}},
		songs: []IndexedSong{
			{
				Path:      "/music/周杰伦晴天翻唱/路人甲.mp3",
				NameLower: "路人甲",
			},
			{
				Path:        "/music/pop/晴天.mp3",
				NameLower:   "晴天",
				TitleLower:  "晴天",
				ArtistLower: "周杰伦",
			},
		},
	}

	got := idx.Search("周杰伦晴天", 10)
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].TitleLower != "晴天" || got[0].ArtistLower != "周杰伦" {
		t.Fatalf("expected artist/title match first, got %+v", got[0])
	}
}

func TestSearchHonorsMaxResultsAfterRanking(t *testing.T) {
	idx := &Indexer{
		config: &MusicConfig{Search: SearchConfig{MaxResults: 10}},
		songs: []IndexedSong{
			{Path: "/music/a.mp3", NameLower: "晴天现场版"},
			{Path: "/music/b.mp3", NameLower: "晴天"},
			{Path: "/music/c.mp3", NameLower: "晴天伴奏"},
		},
	}

	got := idx.Search("晴天", 1)
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	if got[0].NameLower != "晴天" {
		t.Fatalf("expected exact name match first, got %+v", got[0])
	}
}

// TestSearchEpisodeUsesDirScopeForMultiSeasonSeries 覆盖"同一系列多季、每季集数各自
// 从 1 开始编号、文件名本身不含系列名"的场景（例如 儿童评书 第1季/001-499、第2季/001-499）。
// 通过 stories[].dir 精确限定目录，即便文件名只有纯数字编号也应该能正确命中并区分季。
func TestSearchEpisodeUsesDirScopeForMultiSeasonSeries(t *testing.T) {
	idx := &Indexer{
		config: &MusicConfig{
			Search: SearchConfig{MaxResults: 20},
			Stories: []StoryConfig{
				{
					Name:    "三国演义第1季",
					Aliases: []string{"三国第一季", "三国演义第一季"},
					Dir:     "/gushi/三国演义第1季",
				},
				{
					Name:    "三国演义第2季",
					Aliases: []string{"三国第二季", "三国演义第二季"},
					Dir:     "/gushi/三国演义第2季",
				},
			},
		},
		songs: []IndexedSong{
			{Path: "/gushi/三国演义第1季/001-499/047.47 主角，白跑一趟（下）.mp3", NameLower: "047.47 主角", Episode: 47},
			{Path: "/gushi/三国演义第1季/001-499/048.48 贪官与贿赂（上）.mp3", NameLower: "048.48 贪官", Episode: 48},
			{Path: "/gushi/三国演义第1季/001-499/049.49 贪官与贿赂（下）.mp3", NameLower: "049.49 贪官", Episode: 49},
			// 第 2 季同样从 1 开始编号，若不做目录限定，纯数字匹配会和第 1 季混淆。
			{Path: "/gushi/三国演义第2季/001-499/048.48 某集.mp3", NameLower: "048.48 某集", Episode: 48},
		},
	}

	got := idx.SearchEpisode("三国第一季", 48, 20)
	if len(got) != 2 {
		t.Fatalf("expected 2 episodes (48,49) from season 1, got %d: %+v", len(got), got)
	}
	if got[0].Episode != 48 || got[0].Path != "/gushi/三国演义第1季/001-499/048.48 贪官与贿赂（上）.mp3" {
		t.Fatalf("expected episode 48 of season 1 first, got %+v", got[0])
	}
	if got[1].Episode != 49 {
		t.Fatalf("expected episode 49 next (auto-continue), got %+v", got[1])
	}
}

// TestSearchEpisodeStartsFromRequestedEpisodeWhenBeyondMaxResults 回归覆盖一个真实 bug：
// 之前的实现"先按 maxResults 截断，再在截断后的子集里找 episode"，导致当总集数（如 729 集）
// 远大于 max_results（如 20）时，排序后先截断成前 20 集（第 1~20 集），再找 >= 199 的项
// 永远找不到，for 循环里 from 默认 0，于是又从第 1 集悄悄重播——用户反馈"怎么老是播开头"。
// 正确顺序应该是：先按 episode 定位起点，再截断 maxResults。
func TestSearchEpisodeStartsFromRequestedEpisodeWhenBeyondMaxResults(t *testing.T) {
	songs := make([]IndexedSong, 0, 729)
	for ep := 1; ep <= 729; ep++ {
		songs = append(songs, IndexedSong{
			Path:    "/gushi/三国/001-729/" + strconv.Itoa(ep) + ".mp3",
			Episode: ep,
		})
	}
	idx := &Indexer{
		config: &MusicConfig{
			Search: SearchConfig{MaxResults: 20},
			Stories: []StoryConfig{
				{Name: "三国演义第1季", Aliases: []string{"三国演义第一季"}, Dir: "/gushi/三国"},
			},
		},
		songs: songs,
	}

	got := idx.SearchEpisode("三国演义第一季", 199, 20)
	if len(got) == 0 {
		t.Fatalf("expected non-empty result for episode 199, got 0")
	}
	if got[0].Episode != 199 {
		t.Fatalf("expected to start from episode 199, got episode %d (bug: silently restarted from episode 1)", got[0].Episode)
	}
	if len(got) > 20 {
		t.Fatalf("expected at most max_results=20 items, got %d", len(got))
	}

	// 请求一个超出范围的集数（总共只有 729 集，请求第 999 集），应该返回空而不是默默从头播放。
	outOfRange := idx.SearchEpisode("三国演义第一季", 999, 20)
	if len(outOfRange) != 0 {
		t.Fatalf("expected empty result for out-of-range episode 999, got %d items starting at ep=%d",
			len(outOfRange), outOfRange[0].Episode)
	}
}
