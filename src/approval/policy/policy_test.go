package policy

import "testing"

func TestRejectInvalidConfiguration(t *testing.T) {
	for _, text := range []string{
		DefaultJSON + " trailing", DefaultJSON + " {}", `{"version":"v","typo":1}`,
		`{"version":"v","rules":[{"id":"x","names":["Get-Date"],"max_args":-1,"reason":"r"}]}`,
		`{"version":"v","rules":[{"id":"x","names":["Get-Date","get-date"],"reason":"r"}]}`,
		`{"version":"v","rules":[{"id":"x","names":["& whoami"],"reason":"r"}]}`,
	} {
		if _, err := Load(text); err == nil {
			t.Fatalf("accepted bad config: %s", text)
		}
	}
	snapshot, err := Load(DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Denied("REMOVE-ITEM") {
		t.Fatal("deny should be case insensitive")
	}
	if _, ok := snapshot.Rule("wRiTe-OuTpUt"); !ok {
		t.Fatal("allow should be case insensitive")
	}
}
