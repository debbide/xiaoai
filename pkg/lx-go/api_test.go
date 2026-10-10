package lxgo

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	id3v2 "github.com/bogem/id3v2/v2"
)

func mustEngine(t *testing.T, name, code string) *Engine {
	t.Helper()
	engine, err := NewEngine(name, code)
	if err != nil {
		t.Fatalf("NewEngine(%s) failed: %v", name, err)
	}
	t.Cleanup(engine.Close)
	return engine
}

func TestHandleMusicSearchFallsBackToNextSource(t *testing.T) {
	engineA := mustEngine(t, "sourceA", `
		const { on, send, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async ({ source }) => { throw new Error(source + ' failed'); });
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['search'] } } });
	`)
	engineB := mustEngine(t, "sourceB", `
		const { on, send, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async ({ info }) => [{ name: info.keyword, source: 'wy' }]);
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['search'] } } });
	`)

	reg := &Registry{Sources: []*Source{
		{Name: "sourceA", Engine: engineA},
		{Name: "sourceB", Engine: engineB},
	}}
	srv := NewServer(reg)

	req := httptest.NewRequest(http.MethodGet, "/api/music/search?name=%E5%91%A8%E6%9D%B0%E4%BC%A6&source=wy", nil)
	w := httptest.NewRecorder()
	srv.handleMusicSearch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	var result []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(result) != 1 || result[0]["source"] != "wy" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestHandleMusicSearchNoCandidates(t *testing.T) {
	engineA := mustEngine(t, "sourceA", `
		const { send, EVENT_NAMES } = globalThis.lx;
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['musicUrl'] } } });
	`)
	reg := &Registry{Sources: []*Source{{Name: "sourceA", Engine: engineA}}}
	srv := NewServer(reg)

	req := httptest.NewRequest(http.MethodGet, "/api/music/search?name=x&source=wy", nil)
	w := httptest.NewRecorder()
	srv.handleMusicSearch(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleMusicURLReturnsURLAndType(t *testing.T) {
	engine := mustEngine(t, "sourceA", `
		const { on, send, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async ({ info }) => 'https://example.com/' + info.musicInfo.id + '.mp3');
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['musicUrl'] } } });
	`)
	reg := &Registry{Sources: []*Source{{Name: "sourceA", Engine: engine}}}
	srv := NewServer(reg)

	body := `{"songInfo":{"id":"123","source":"wy"},"quality":"320k"}`
	req := httptest.NewRequest(http.MethodPost, "/api/music/url", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleMusicURL(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["url"] != "https://example.com/123.mp3" || result["type"] != "320k" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestHandleMusicURLFallsBackAndPublishesProgress(t *testing.T) {
	engineA := mustEngine(t, "sourceA", `
		const { on, send, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async () => { throw new Error('boom'); });
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['musicUrl'] } } });
	`)
	engineB := mustEngine(t, "sourceB", `
		const { on, send, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async () => 'https://example.com/ok.mp3');
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['musicUrl'] } } });
	`)
	reg := &Registry{Sources: []*Source{
		{Name: "sourceA", Engine: engineA},
		{Name: "sourceB", Engine: engineB},
	}}
	srv := NewServer(reg)

	ch, cancel := srv.progress.Subscribe("req-1")
	defer cancel()

	body := `{"songInfo":{"id":"1","source":"wy"},"quality":"128k"}`
	req := httptest.NewRequest(http.MethodPost, "/api/music/url", strings.NewReader(body))
	req.Header.Set(progressHeader, "req-1")
	w := httptest.NewRecorder()
	srv.handleMusicURL(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}

	var events []ProgressEvent
	for len(events) < 4 {
		events = append(events, <-ch)
	}
	if events[0].Stage != "trying" || events[0].Source != "sourceA" {
		t.Fatalf("unexpected first event: %#v", events[0])
	}
	if events[1].Stage != "failed" || events[1].Source != "sourceA" {
		t.Fatalf("unexpected second event: %#v", events[1])
	}
	foundSuccess := false
	for _, ev := range events {
		if ev.Stage == "success" && ev.Source == "sourceB" && ev.Done {
			foundSuccess = true
		}
	}
	if !foundSuccess {
		t.Fatalf("expected a success/done event for sourceB, got: %#v", events)
	}
}

func TestHandleMusicDownloadProxiesWithoutTagInjection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("fake-mp3-bytes"))
	}))
	defer upstream.Close()

	srv := NewServer(&Registry{})
	req := httptest.NewRequest(http.MethodGet, "/api/music/download?url="+upstream.URL+"&tag=0", nil)
	w := httptest.NewRecorder()
	srv.handleMusicDownload(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != "fake-mp3-bytes" {
		t.Fatalf("unexpected body: %q", w.Body.String())
	}
}

func TestHandleMusicDownloadInjectsID3Tag(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		// 内容本身是不是合法 mp3 帧不影响 ID3 标签注入：ID3v2 标签只是文件开头的一段
		// 元数据块，找不到已有标签时会新建一个，后面原样跟上"音频"字节即可。
		_, _ = w.Write([]byte("fake-audio-payload"))
	}))
	defer upstream.Close()

	srv := NewServer(&Registry{})
	q := url.Values{}
	q.Set("url", upstream.URL)
	q.Set("filename", "song.mp3")
	q.Set("name", "稻香")
	q.Set("singer", "周杰伦")
	q.Set("album", "还在流浪")
	req := httptest.NewRequest(http.MethodGet, "/api/music/download?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	srv.handleMusicDownload(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "song.mp3") {
		t.Fatalf("unexpected content-disposition: %s", w.Header().Get("Content-Disposition"))
	}
	tag, err := id3v2.ParseReader(bytes.NewReader(w.Body.Bytes()), id3v2.Options{Parse: true})
	if err != nil {
		t.Fatalf("failed to parse tagged output: %v", err)
	}
	defer tag.Close()
	if tag.Title() != "稻香" || tag.Artist() != "周杰伦" || tag.Album() != "还在流浪" {
		t.Fatalf("unexpected tag: title=%q artist=%q album=%q", tag.Title(), tag.Artist(), tag.Album())
	}
	if !strings.HasSuffix(w.Body.String(), "fake-audio-payload") {
		t.Fatalf("expected original audio payload to be preserved at the end of the file")
	}
}
