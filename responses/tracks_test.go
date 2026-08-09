package responses

import (
	"encoding/xml"
	"testing"
)

func TestTrackAudioStreamAndLoudnessResponse(t *testing.T) {
	var track ResponseTrack
	if err := xml.Unmarshal([]byte(`<Track titleSort="Romanized track" parentTitleSort="Romanized album" grandparentTitleSort="Romanized artist" thumbBlurHash="L4F5?^_3_3t7~qM{t7M{?b9FM{of"><Media><Part key="/library/parts/1/file"><Stream id="video" streamType="1"/><Stream id="audio" streamType="2"/></Part></Media></Track>`), &track); err != nil {
		t.Fatal(err)
	}
	if got := track.AudioStreamID(); got != "audio" {
		t.Fatalf("AudioStreamID() = %q, want audio", got)
	}
	if got := track.ThumbBlurHash; got != "L4F5?^_3_3t7~qM{t7M{?b9FM{of" {
		t.Fatalf("ThumbBlurHash = %q", got)
	}
	if track.TitleSort != "Romanized track" || track.ParentTitleSort != "Romanized album" || track.GrandParentTitleSort != "Romanized artist" {
		t.Fatalf("sort metadata was not decoded: %#v", track)
	}

	var levels ResponseLoudnessLevelsMediaContainer
	if err := xml.Unmarshal([]byte(`<MediaContainer><Level v="-18.3"/><Level v="-9.1"/></MediaContainer>`), &levels); err != nil {
		t.Fatal(err)
	}
	if len(levels.Levels) != 2 || levels.Levels[0].Value != -18.3 || levels.Levels[1].Value != -9.1 {
		t.Fatalf("unexpected levels: %#v", levels.Levels)
	}
}
