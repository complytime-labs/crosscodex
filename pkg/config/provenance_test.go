package config

import "testing"

func TestSourceTracker_TrackAndRetrieve(t *testing.T) {
	tracker := newSourceTracker()
	node := mustParseYAML(t, "tls:\n  mode: mutual\n  ca: /etc/ca.crt\n")
	tracker.track(node, "/etc/crosscodex/config.yaml")

	got := tracker.sourceOf("tls.mode")
	if got != "/etc/crosscodex/config.yaml" {
		t.Errorf("sourceOf(tls.mode) = %q, want file path", got)
	}

	got = tracker.sourceOf("tls.ca")
	if got != "/etc/crosscodex/config.yaml" {
		t.Errorf("sourceOf(tls.ca) = %q, want file path", got)
	}
}

func TestSourceTracker_LaterLayerOverwrites(t *testing.T) {
	tracker := newSourceTracker()

	base := mustParseYAML(t, "tls:\n  mode: off\n")
	tracker.track(base, "/etc/crosscodex/config.yaml")

	overlay := mustParseYAML(t, "tls:\n  mode: mutual\n")
	tracker.track(overlay, "/home/user/.config/crosscodex/conf.d/10-tls.yaml")

	got := tracker.sourceOf("tls.mode")
	want := "/home/user/.config/crosscodex/conf.d/10-tls.yaml"
	if got != want {
		t.Errorf("sourceOf(tls.mode) = %q, want %q", got, want)
	}
}

func TestSourceTracker_UnknownPathReturnsDefault(t *testing.T) {
	tracker := newSourceTracker()
	got := tracker.sourceOf("nonexistent.key")
	if got != "compiled defaults" {
		t.Errorf("sourceOf(unknown) = %q, want %q", got, "compiled defaults")
	}
}

func TestSourceTracker_NilIsNoOp(t *testing.T) {
	var tracker *sourceTracker
	got := tracker.sourceOf("anything")
	if got != "" {
		t.Errorf("nil tracker sourceOf = %q, want empty", got)
	}
}

func TestFormatSource_WithTracker(t *testing.T) {
	tracker := newSourceTracker()
	node := mustParseYAML(t, "tls:\n  mode: bogus\n")
	tracker.track(node, "/tmp/bad.yaml")

	got := formatSource(tracker, "tls.mode")
	want := " (set in /tmp/bad.yaml)"
	if got != want {
		t.Errorf("formatSource = %q, want %q", got, want)
	}
}

func TestFormatSource_NilTracker(t *testing.T) {
	got := formatSource(nil, "tls.mode")
	if got != "" {
		t.Errorf("formatSource(nil) = %q, want empty", got)
	}
}
