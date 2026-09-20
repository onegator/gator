package notify

import "testing"

// The iOS app has its own bundle id, and Apple treats a mismatched topic as a dead device.
func TestAPNsTopicPerPlatform(t *testing.T) {
	both := &APNs{topic: "dev.onegator.gator", iosTopic: "dev.onegator.gator.ios"}
	if got := both.topicFor("ios"); got != "dev.onegator.gator.ios" {
		t.Errorf("ios topic = %q", got)
	}
	if got := both.topicFor("macos"); got != "dev.onegator.gator" {
		t.Errorf("macos topic = %q", got)
	}

	// One bundle id for both apps stays legal: every device keeps the single topic.
	one := &APNs{topic: "dev.onegator.gator"}
	for _, platform := range []string{"ios", "macos", ""} {
		if got := one.topicFor(platform); got != "dev.onegator.gator" {
			t.Errorf("topicFor(%q) = %q, want the only topic", platform, got)
		}
	}
	if one.Name() != "apns dev.onegator.gator" {
		t.Errorf("Name() = %q", one.Name())
	}
}
