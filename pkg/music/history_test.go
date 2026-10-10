package music

import (
	"path/filepath"
	"testing"
)

func TestPlayHistoryStoreSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "play_history.json")

	store := NewPlayHistoryStore(path)
	store.Update("西游记", 12, "/gushi/012.mp3")

	loaded := NewPlayHistoryStore(path)
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	entry := loaded.Get()
	if entry.SeriesName != "西游记" || entry.Episode != 12 {
		t.Fatalf("unexpected loaded entry: %+v", entry)
	}
}

func TestPlayHistoryStoreLoadMissingFileIsNotError(t *testing.T) {
	dir := t.TempDir()
	store := NewPlayHistoryStore(filepath.Join(dir, "does-not-exist.json"))
	if err := store.Load(); err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if entry := store.Get(); entry.SeriesName != "" {
		t.Fatalf("expected empty entry, got %+v", entry)
	}
}

func TestPlayHistoryStoreIgnoresEmptySeriesOrEpisode(t *testing.T) {
	dir := t.TempDir()
	store := NewPlayHistoryStore(filepath.Join(dir, "play_history.json"))

	store.Update("", 5, "/x.mp3")
	if entry := store.Get(); entry.SeriesName != "" {
		t.Fatalf("expected empty series to be ignored, got %+v", entry)
	}

	store.Update("水浒传", 0, "/x.mp3")
	if entry := store.Get(); entry.SeriesName != "" {
		t.Fatalf("expected episode<=0 to be ignored, got %+v", entry)
	}
}

func TestPlayHistoryStoreLatestOverwritesPrevious(t *testing.T) {
	dir := t.TempDir()
	store := NewPlayHistoryStore(filepath.Join(dir, "play_history.json"))

	store.Update("三国演义", 3, "/gushi/003.mp3")
	store.Update("三国演义", 4, "/gushi/004.mp3")

	entry := store.Get()
	if entry.Episode != 4 {
		t.Fatalf("expected latest episode to win, got %+v", entry)
	}
}
