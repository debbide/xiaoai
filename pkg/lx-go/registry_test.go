package lxgo

import (
	"testing"
)

func TestParseScriptName(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "bang comment",
			src:  "/*!\n * @name 墨澜聚合音源\n * @description xxx\n */\nconst a = 1",
			want: "墨澜聚合音源",
		},
		{
			name: "jsdoc comment",
			src:  "/**\n * @name HYWmusic_beta_公益测试\n * @version v0.74.0\n */\n'use strict'",
			want: "HYWmusic_beta_公益测试",
		},
		{
			name: "no name",
			src:  "const a = 1",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseScriptName(c.src); got != c.want {
				t.Fatalf("parseScriptName() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSelfTestSourcePassesWhenSearchReturnsResults(t *testing.T) {
	engine := mustEngine(t, "ok-search", `
		const { on, send, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async ({ info }) => [{ name: info.keyword, source: 'wy' }]);
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['search'] } } });
	`)
	ok, info := selfTestSource(engine)
	if !ok {
		t.Fatalf("expected self-test to pass, got info: %s", info)
	}
}

func TestSelfTestSourceFailsWhenSearchErrors(t *testing.T) {
	engine := mustEngine(t, "bad-search", `
		const { on, send, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async () => { throw new Error('upstream down'); });
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['search'] } } });
	`)
	ok, info := selfTestSource(engine)
	if ok {
		t.Fatalf("expected self-test to fail, got info: %s", info)
	}
	if info == "" {
		t.Fatal("expected a non-empty failure message")
	}
}

func TestSelfTestSourceFailsWhenSearchReturnsEmpty(t *testing.T) {
	engine := mustEngine(t, "empty-search", `
		const { on, send, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async () => []);
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['search'] } } });
	`)
	ok, _ := selfTestSource(engine)
	if ok {
		t.Fatal("expected self-test to fail on empty result")
	}
}

func TestSelfTestSourceFallsBackToMusicURLForWY(t *testing.T) {
	engine := mustEngine(t, "musicurl-only", `
		const { on, send, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async ({ info }) => 'https://example.com/' + info.musicInfo.id + '.mp3');
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['musicUrl'] } } });
	`)
	ok, info := selfTestSource(engine)
	if !ok {
		t.Fatalf("expected musicUrl fallback self-test to pass, got info: %s", info)
	}
}

func TestSelfTestSourceSkippedWhenNeitherActionSupported(t *testing.T) {
	engine := mustEngine(t, "no-testable-action", `
		const { send, EVENT_NAMES } = globalThis.lx;
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['lyric'] } } });
	`)
	ok, _ := selfTestSource(engine)
	if !ok {
		t.Fatal("expected self-test to be skipped (pass-through) when neither search nor wy musicUrl is supported")
	}
}
