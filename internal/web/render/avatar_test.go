package render

import (
	"testing"
	"time"
)

// AvatarOf: a push shows its author (picture by login, else initials; a bot is
// a rounded square); a push before authors were kept shows no person; an image
// watch is an eye; the rest are panel actions.
func TestAvatarOf(t *testing.T) {
	if a := AvatarOf("push", "Ada Lovelace", "ada"); a.Login != "ada" || a.Initials != "AL" || a.Badge != "github" || a.Bot {
		t.Errorf("push = %+v", a)
	}
	if a := AvatarOf("push", "Ada Lovelace", ""); a.Login != "" || a.Initials != "AL" {
		t.Errorf("no login = %+v", a)
	}
	if a := AvatarOf("push", "", "dependabot[bot]"); !a.Bot || a.Login == "" {
		t.Errorf("bot = %+v", a)
	}
	if a := AvatarOf("push", "", ""); a.Login != "" || a.Initials != "" || a.Icon != "github" {
		t.Errorf("old push = %+v", a)
	}
	if a := AvatarOf("image-watch", "", ""); a.Icon != "eye" || a.Badge != "" {
		t.Errorf("watch = %+v", a)
	}
	if a := AvatarOf("deploy", "", ""); a.Icon != "stackr" || a.Badge != "" || a.Initials != "" || a.Login != "" {
		t.Errorf("deploy = %+v", a)
	}
}

// Line 2 names a person by display name and never by id.
func TestWhoAndSourceNeverShowIDs(t *testing.T) {
	id := "8c4004f0-45ea-42a7-a4e5-a0ffcd5aa83d"
	names := map[string]string{id: "Ada Lovelace"}
	for _, by := range []string{id, "user:" + id} {
		if SourceWord(by, names) != "Ada Lovelace" || WhoWord(by, "", "", names) != "Ada Lovelace" || ViaWord(by) != "panel" {
			t.Errorf("%q: %q, %q, %q", by, SourceWord(by, names), WhoWord(by, "", "", names), ViaWord(by))
		}
		if SourceWord(by, nil) != "a member" || WhoWord(by, "", "", nil) != "" {
			t.Errorf("%q unnamed: %q, %q", by, SourceWord(by, nil), WhoWord(by, "", "", nil))
		}
	}
	if WhoWord("push", "Ada", "ada", nil) != "Ada" || WhoWord("push", "", "ada", nil) != "ada" || WhoWord("image-watch", "", "", nil) != "" {
		t.Error("push author / image watch")
	}
	if ViaWord("push") != "GitHub" || ViaWord("image-watch") != "image watch" || AgoWord(26*time.Hour) != "26h ago" || AgoWord(72*time.Hour) != "3d ago" {
		t.Error("via / ago words")
	}
}
